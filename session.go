package monty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/go-monty/internal/pb"
)

// Limits bounds a session. Zero values mean unlimited, except
// MaxRecursionDepth and MaxSuspensions, which default to 1000.
type Limits struct {
	// MaxDuration caps the time the interpreter spends executing one run
	// (a Start or Run, across its host calls). Time spent in host functions
	// does not count. Exceeding it raises TimeoutError in the sandbox.
	MaxDuration time.Duration
	// MaxTurnDuration caps the time the interpreter runs between two host
	// calls of a run.
	MaxTurnDuration time.Duration
	// MaxMemory caps the sandbox heap in bytes. Exceeding it raises
	// MemoryError; a burst past the ceiling above it kills the worker.
	MaxMemory uint64
	// GCInterval is the number of allocations between cycle collections.
	GCInterval uint64
	// MaxRecursionDepth caps Python call depth (RecursionError).
	MaxRecursionDepth uint64
	// MaxSuspensions caps the times the sandbox pauses for the host
	// (functions, name lookups, OS calls, waits on deferred calls) over the
	// life of the session, as upstream does. Exceeding it aborts the current
	// run with a RuntimeError; Reset starts the count over.
	MaxSuspensions uint64
	// MaxSleep caps the total time the session's time.sleep() and
	// asyncio.sleep() calls wait under SleepSystem. Exceeding it aborts the
	// current run with a TimeoutError.
	MaxSleep time.Duration
}

func (l *Limits) proto() *pb.ResourceLimits {
	if l == nil {
		return nil
	}
	out := &pb.ResourceLimits{}
	if l.MaxDuration > 0 {
		v := uint64(l.MaxDuration / time.Microsecond)
		out.MaxFeedDurationMicros = &v
	}
	if l.MaxTurnDuration > 0 {
		v := uint64(l.MaxTurnDuration / time.Microsecond)
		out.MaxTurnDurationMicros = &v
	}
	if l.MaxSleep > 0 {
		v := uint64(l.MaxSleep / time.Microsecond)
		out.MaxTotalSleepMicros = &v
	}
	if l.MaxMemory > 0 {
		v := l.MaxMemory
		out.MaxMemoryBytes = &v
	}
	if l.GCInterval > 0 {
		v := l.GCInterval
		out.GcInterval = &v
	}
	if l.MaxRecursionDepth > 0 {
		v := l.MaxRecursionDepth
		out.MaxRecursionDepth = &v
	}
	if l.MaxSuspensions > 0 {
		v := l.MaxSuspensions
		out.MaxSuspensions = &v
	}
	return out
}

// TypeCheckFormat selects how typing diagnostics are rendered.
type TypeCheckFormat string

const (
	TypeCheckFull      TypeCheckFormat = "full"
	TypeCheckConcise   TypeCheckFormat = "concise"
	TypeCheckAzure     TypeCheckFormat = "azure"
	TypeCheckJSON      TypeCheckFormat = "json"
	TypeCheckJSONLines TypeCheckFormat = "json-lines"
	TypeCheckRDJSON    TypeCheckFormat = "rdjson"
	TypeCheckPylint    TypeCheckFormat = "pylint"
	TypeCheckGitLab    TypeCheckFormat = "gitlab"
	TypeCheckGitHub    TypeCheckFormat = "github"
)

func (f TypeCheckFormat) proto() pb.TypeCheckFormat {
	switch f {
	case TypeCheckConcise:
		return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_CONCISE
	case TypeCheckAzure:
		return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_AZURE
	case TypeCheckJSON:
		return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_JSON
	case TypeCheckJSONLines:
		return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_JSON_LINES
	case TypeCheckRDJSON:
		return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_RDJSON
	case TypeCheckPylint:
		return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_PYLINT
	case TypeCheckGitLab:
		return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_GITLAB
	case TypeCheckGitHub:
		return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_GITHUB
	}
	return pb.TypeCheckFormat_TYPE_CHECK_FORMAT_FULL
}

// SessionOptions configures a Session.
type SessionOptions struct {
	// ScriptName is the file name shown in tracebacks. Defaults to "main.py".
	ScriptName string
	// Limits bounds the session; nil applies the defaults described on Limits.
	Limits *Limits
	// TypeCheck runs the type checker on every snippet before executing it,
	// so unsupported APIs fail with a TypingError instead of at runtime.
	TypeCheck bool
	// TypeCheckStubs holds optional .pyi stub contents declaring the host
	// functions and inputs, used by the type checker.
	TypeCheckStubs string
	// TypeCheckFormat renders diagnostics; the zero value is TypeCheckFull.
	TypeCheckFormat TypeCheckFormat
	// TypeCheckColor renders full/concise diagnostics with ANSI colours.
	TypeCheckColor bool
	// AssertMessageAnnotations controls introspected assert messages: nil
	// keeps the default (on, 120-byte operand reprs), 0 disables them, any
	// other value sets the per-operand byte budget.
	AssertMessageAnnotations *uint32
	// OSPolicy selects the session's clock, timezone, sleeps and randomness.
	OSPolicy OSPolicy
	// PrintFlushInterval batches print() output: the sandbox holds it for up
	// to this long before delivering it. Zero delivers every completed line
	// as it is printed.
	PrintFlushInterval time.Duration
}

func (o *SessionOptions) proto() *pb.Configure {
	name := o.ScriptName
	if name == "" {
		name = "main.py"
	}
	cfg := &pb.Configure{
		ScriptName:               name,
		Limits:                   o.Limits.proto(),
		TypeCheck:                o.TypeCheck,
		MontyVersion:             "go-monty",
		AssertMessageAnnotations: o.AssertMessageAnnotations,
		TypeCheckFormat:          o.TypeCheckFormat.proto(),
		TypeCheckColor:           o.TypeCheckColor,
		ProtocolVersion:          protocolVersion,
		OsPolicy:                 o.OSPolicy.proto(),
	}
	flush := uint32(o.PrintFlushInterval / time.Millisecond)
	cfg.PrintFlushIntervalMs = &flush
	if o.TypeCheckStubs != "" {
		stubs := o.TypeCheckStubs
		cfg.TypeCheckStubs = &stubs
	}
	return cfg
}

// Call is one invocation of a host function or method from sandbox code.
type Call struct {
	// Name is the function or method name as called.
	Name string
	// Args holds the positional arguments (the receiver excluded).
	Args []any
	// Kwargs holds the keyword arguments.
	Kwargs map[string]any
}

// Function is a Go function exposed to sandbox code. The returned value is
// converted as documented on value conversion. Return an *Exception (see
// Raise) to raise a specific Python exception; any other error raises
// RuntimeError with its message.
type Function func(ctx context.Context, call *Call) (any, error)

// StartOptions configures one Session.Start.
type StartOptions struct {
	// Inputs are bound as global variables before the code runs.
	Inputs map[string]any
	// Stdout and Stderr receive print() output as it happens. nil means
	// os.Stdout and os.Stderr; use io.Discard to drop output.
	Stdout io.Writer
	Stderr io.Writer
	// SkipTypeCheck skips the checker for this run in a TypeCheck session.
	SkipTypeCheck bool
}

// RunOptions configures one Session.Run: what the code may reach, and where
// its output goes.
type RunOptions struct {
	// Inputs are bound as global variables before the code runs.
	Inputs map[string]any
	// Functions are callable from the code by name and run synchronously,
	// each call pausing the sandbox until it returns. Their names are bound
	// as globals, shadowing a Python builtin of the same name, and stay
	// bound for later runs.
	Functions map[string]Function
	// AsyncFunctions are callable like Functions but run in their own
	// goroutines while the sandbox continues. Code that needs the result
	// (awaiting it, or using it) waits for it; asyncio.gather over several
	// calls runs them concurrently. A call the code never awaits outlives
	// the run: Session.Wait joins it.
	AsyncFunctions map[string]Function
	// Values are resolved lazily: an entry is converted only when the code
	// reads its name, unlike Inputs, which are bound before the code runs.
	// Entries may be *HostObject or *HostClass values.
	Values map[string]any
	// Stdout and Stderr receive print() output as it happens. nil means
	// os.Stdout and os.Stderr; use io.Discard to drop output.
	Stdout io.Writer
	Stderr io.Writer
	// OS services filesystem, environment and clock requests. nil declines
	// every call, so pathlib operations raise PermissionError.
	OS OSHandler
	// SkipTypeCheck skips the checker for this run in a TypeCheck session.
	SkipTypeCheck bool
}

func (o *RunOptions) start() StartOptions {
	inputs := o.Inputs
	if len(o.Functions)+len(o.AsyncFunctions) > 0 {
		// Bound up front rather than looked up on first use, so a function
		// named like a builtin (open, exec, input) is the one the code calls.
		inputs = make(map[string]any, len(o.Functions)+len(o.AsyncFunctions)+len(o.Inputs))
		for name := range o.Functions {
			inputs[name] = FunctionRef{Name: name}
		}
		for name := range o.AsyncFunctions {
			inputs[name] = FunctionRef{Name: name}
		}
		maps.Copy(inputs, o.Inputs)
	}
	return StartOptions{Inputs: inputs, Stdout: o.Stdout, Stderr: o.Stderr, SkipTypeCheck: o.SkipTypeCheck}
}

// Session is a Python REPL: globals persist across runs. It is safe for
// concurrent use, but calls are serialised. A session lives on one wasm
// instance; if the worker dies (WorkerError), close it and create a new one.
type Session struct {
	rt *Runtime

	mu             sync.Mutex
	w              *worker
	store          *objectStore
	maxSuspensions uint64
	suspensions    uint64
	// sleepLimit is Limits.MaxSleep (zero: unlimited), sleepMax the cap on
	// one system sleep.
	sleepLimit    time.Duration
	sleepUsed     time.Duration
	sleepMax      time.Duration
	executionTime time.Duration
	pending       *Suspension
	// driver is the Run in progress, which waits out async sleeps without
	// blocking the sandbox's other tasks.
	driver *driver
	stdout io.Writer
	stderr io.Writer
	closed bool
	// release returns the worker to a Pool instead of closing it.
	release func(ctx context.Context, w *worker)
	// calls and running track AsyncFunctions calls, which can outlive the
	// run that started them.
	calls   sync.WaitGroup
	running atomic.Int64
}

const defaultMaxSuspensions = 1000

// NewSession creates a fresh sandbox.
func (r *Runtime) NewSession(ctx context.Context, opts SessionOptions) (*Session, error) {
	s, err := r.newSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.configure(ctx, opts); err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	return s, nil
}

// Restore recreates a session from a Dump: an idle session, or one paused
// at a host call, in which case Pending returns the Suspension to answer
// (with its methods, or with Continue). Host objects do not travel with a
// dump; a restored session reports calls on them as Suspensions with
// ObjectID set but no Object.
//
// The dump carries the session's script name and limits. Its OSPolicy and
// print settings do not travel: pass the SessionOptions to restore them,
// whose Limits can only tighten the dump's.
func (r *Runtime) Restore(ctx context.Context, state []byte, opts ...SessionOptions) (*Session, error) {
	s, err := r.newSession(ctx)
	if err != nil {
		return nil, err
	}
	var o SessionOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	s.budget(o)
	cfg := o.proto()
	cfg.First = &pb.Configure_Load{Load: &pb.Load{State: state}}
	events, err := s.send(ctx, &pb.ClientRequest{Kind: &pb.ClientRequest_Configure{Configure: cfg}})
	if err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	last := events[len(events)-1]
	if _, ok := last.Kind.(*pb.WorkerEvent_Ok); ok {
		s.noteBudget(last)
		return s, nil
	}
	if _, err := s.progress(ctx, events, false); err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	return s, nil
}

func (r *Runtime) newSession(ctx context.Context) (*Session, error) {
	w, err := r.newWorker(ctx, newTailBuffer(64*1024))
	if err != nil {
		return nil, err
	}
	return r.sessionFor(w), nil
}

func (r *Runtime) sessionFor(w *worker) *Session {
	w.runs++
	return &Session{rt: r, w: w, store: newObjectStore(), maxSuspensions: defaultMaxSuspensions, sleepMax: defaultMaxSleep, stdout: os.Stdout, stderr: os.Stderr}
}

func (s *Session) codec() *codec { return &codec{store: s.store} }

// send runs one turn, streaming prints to the session's current writers.
func (s *Session) send(ctx context.Context, req *pb.ClientRequest) ([]*pb.WorkerEvent, error) {
	return s.w.send(ctx, req, func(p *pb.Print) {
		for _, seg := range p.GetSegments() {
			w := s.stdout
			if seg.GetStream() == pb.PrintStream_PRINT_STREAM_STDERR {
				w = s.stderr
			}
			if w != nil {
				_, _ = io.WriteString(w, seg.GetText())
			}
		}
	})
}

// budget resets the limits this package enforces itself to opts.
func (s *Session) budget(opts SessionOptions) {
	s.maxSuspensions = defaultMaxSuspensions
	s.suspensions, s.sleepLimit, s.sleepUsed = 0, 0, 0
	if opts.Limits != nil {
		if opts.Limits.MaxSuspensions > 0 {
			s.maxSuspensions = opts.Limits.MaxSuspensions
		}
		s.sleepLimit = opts.Limits.MaxSleep
	}
	s.sleepMax = opts.OSPolicy.maxSleep()
}

func (s *Session) configure(ctx context.Context, opts SessionOptions) error {
	s.budget(opts)
	events, err := s.send(ctx, &pb.ClientRequest{Kind: &pb.ClientRequest_Configure{Configure: opts.proto()}})
	if err != nil {
		return err
	}
	return s.expectOk(events, "Configure")
}

func (s *Session) expectOk(events []*pb.WorkerEvent, what string) error {
	last := events[len(events)-1]
	s.noteBudget(last)
	switch k := last.Kind.(type) {
	case *pb.WorkerEvent_Ok:
		return nil
	case *pb.WorkerEvent_Error:
		return s.exception(k.Error.GetException())
	case *pb.WorkerEvent_SessionRefused:
		return fmt.Errorf("monty: session refused: %s", k.SessionRefused.GetMessage())
	case *pb.WorkerEvent_FatalError:
		s.w.dead = true
		return &WorkerError{Message: k.FatalError.GetMessage(), Stderr: s.w.stderr.String()}
	}
	return &WorkerError{Message: fmt.Sprintf("unexpected reply to %s: %T", what, last.Kind)}
}

func (s *Session) exception(e *pb.RaisedException) *Exception {
	return exceptionFromProto(e)
}

// noteBudget adopts what a reply reports. The limits a restored dump
// carries can only tighten the configured ones.
func (s *Session) noteBudget(ev *pb.WorkerEvent) {
	if v := ev.GetMaxSuspensions(); v > 0 && v < s.maxSuspensions {
		s.maxSuspensions = v
	}
	if v := time.Duration(ev.GetMaxTotalSleepMicros()) * time.Microsecond; v > 0 && (s.sleepLimit == 0 || v < s.sleepLimit) {
		s.sleepLimit = v
	}
	if ev.TotalExecutionMicros > 0 {
		s.executionTime = time.Duration(ev.TotalExecutionMicros) * time.Microsecond
	}
}

// ExecutionTime is the cumulative time the interpreter has spent executing
// this session's code. It excludes time spent in host functions, accumulates
// across runs and survives Dump and Restore.
func (s *Session) ExecutionTime() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executionTime
}

// Pending returns the host call the session is paused at, or nil when idle.
func (s *Session) Pending() *Suspension {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

func (s *Session) setWriters(stdout, stderr io.Writer) {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	s.stdout, s.stderr = stdout, stderr
}

// Start feeds code to the session and runs it until it completes or pauses
// at a host call. A paused run is a Progress with a Suspension: inspect it,
// Dump it for later, or answer it with one of its methods. Runs paused this
// way never call any host function on their own.
func (s *Session) Start(ctx context.Context, code string, opts StartOptions) (*Progress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.start(ctx, code, opts)
}

func (s *Session) start(ctx context.Context, code string, opts StartOptions) (*Progress, error) {
	if s.closed {
		return nil, ErrClosed
	}
	if s.pending != nil {
		return nil, errors.New("monty: session is paused at a host call; answer it or Continue first")
	}
	s.setWriters(opts.Stdout, opts.Stderr)
	a := s.codec().arena()
	inputs := make([]*pb.NamedRef, 0, len(opts.Inputs))
	for _, name := range slices.Sorted(maps.Keys(opts.Inputs)) {
		idx, err := a.encode(opts.Inputs[name])
		if err != nil {
			return nil, fmt.Errorf("monty: input %q: %w", name, err)
		}
		inputs = append(inputs, &pb.NamedRef{Name: name, Value: idx})
	}
	req := &pb.ClientRequest{Kind: &pb.ClientRequest_Feed{Feed: &pb.Feed{
		Code:          code,
		Inputs:        inputs,
		Values:        a.proto(),
		SkipTypeCheck: opts.SkipTypeCheck,
	}}}
	events, err := s.send(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.progress(ctx, events, false)
}

// Run executes code in the session, answering every host call from opts,
// and returns the value of the code's last expression statement (nil when
// there is none), or an error: *Exception when the code raised,
// *TypingError when type checking rejected it, or *WorkerError when the
// sandbox died.
func (s *Session) Run(ctx context.Context, code string, opts RunOptions) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := newDriver(s, &opts).own()
	defer d.close()
	p, err := s.start(ctx, code, opts.start())
	if err != nil {
		return nil, err
	}
	return d.drive(ctx, p)
}

// Continue answers the pending host call of a paused (typically restored)
// session from opts and runs to completion, like Run.
func (s *Session) Continue(ctx context.Context, opts RunOptions) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.pending == nil {
		return nil, errors.New("monty: nothing to continue")
	}
	s.setWriters(opts.Stdout, opts.Stderr)
	d := newDriver(s, &opts).own()
	defer d.close()
	return d.drive(ctx, &Progress{Suspension: s.pending})
}

// drive resolves suspensions from the driver's options until the run
// completes.
func (d *driver) drive(ctx context.Context, p *Progress) (any, error) {
	s := d.s
	// A paused worker must always get an answer, or the next run would break
	// the protocol. If a host callback panics, abort the feed first.
	defer func() {
		if r := recover(); r != nil {
			if s.pending != nil && !s.w.dead {
				_ = s.pending.abort(context.WithoutCancel(ctx), "RuntimeError", fmt.Sprintf("host callback panicked: %v", r))
			}
			panic(r)
		}
	}()
	for !p.Done() {
		var err error
		if p, err = d.resolve(ctx, p.Suspension); err != nil {
			return nil, err
		}
	}
	return p.Value, nil
}

// progress classifies a turn's events into a Progress or an error, marking
// the session paused when the turn ended in a host call. resumed says the
// turn answered a suspension.
func (s *Session) progress(ctx context.Context, events []*pb.WorkerEvent, resumed bool) (*Progress, error) {
	last := events[len(events)-1]
	s.noteBudget(last)
	s.pending = nil
	su := &Suspension{s: s}
	switch k := last.Kind.(type) {
	case *pb.WorkerEvent_Complete:
		vals, err := s.codec().decode(k.Complete.GetValues())
		if err != nil {
			return nil, &WorkerError{Message: err.Error()}
		}
		v, err := vals.at(k.Complete.GetValue())
		if err != nil {
			return nil, &WorkerError{Message: err.Error()}
		}
		return &Progress{Value: v}, nil
	case *pb.WorkerEvent_Error:
		return nil, s.failed(ctx, last, resumed, s.exception(k.Error.GetException()))
	case *pb.WorkerEvent_TypingError:
		return nil, s.failed(ctx, last, resumed, &TypingError{Diagnostics: k.TypingError.GetDiagnostics()})
	case *pb.WorkerEvent_SessionRefused:
		return nil, fmt.Errorf("monty: session refused: %s", k.SessionRefused.GetMessage())
	case *pb.WorkerEvent_FatalError:
		s.w.dead = true
		return nil, &WorkerError{Message: k.FatalError.GetMessage(), Stderr: s.w.stderr.String()}
	case *pb.WorkerEvent_FunctionCall:
		fc := k.FunctionCall
		su.Kind = SuspensionCall
		su.CallID = fc.GetCallId()
		su.Call = &Call{Name: fc.GetFunctionName()}
		s.pending = su
		vals, err := s.codec().decode(fc.GetValues())
		if err != nil {
			return nil, s.protocolError("decoding call values: %v", err)
		}
		if su.Call.Args, err = vals.list(fc.GetArgs()); err != nil {
			return nil, s.protocolError("decoding call arguments: %v", err)
		}
		if su.Call.Kwargs, err = vals.stringDict(fc.GetKwargs()); err != nil {
			return nil, s.protocolError("decoding call keyword arguments: %v", err)
		}
		s.resolveReceiver(su, fc.GetObjectId())
	case *pb.WorkerEvent_NameLookup:
		su.Kind = SuspensionLookup
		su.Name = k.NameLookup.GetName()
		s.resolveReceiver(su, k.NameLookup.GetObjectId())
	case *pb.WorkerEvent_OsCall:
		su.Kind = SuspensionOS
		su.CallID = k.OsCall.GetCallId()
		s.pending = su
		call, err := s.codec().osCall(k.OsCall)
		if err != nil {
			return nil, s.protocolError("decoding OS call: %v", err)
		}
		su.OSCall = call
	case *pb.WorkerEvent_ResolveFutures:
		su.Kind = SuspensionFutures
		su.PendingCallIDs = k.ResolveFutures.GetPendingCallIds()
	default:
		return nil, &WorkerError{Message: fmt.Sprintf("unexpected event %T", last.Kind)}
	}
	s.pending = su
	s.suspensions++
	if s.maxSuspensions > 0 && s.suspensions > s.maxSuspensions {
		return su.answer(ctx, abortFeed("RuntimeError", fmt.Sprintf("suspension limit %d exceeded", s.maxSuspensions)))
	}
	if su.Kind == SuspensionOS {
		switch {
		case su.OSCall == nil:
			// A call this package does not know raises the sandbox's default.
			return su.decline(ctx)
		case su.OSCall.Name == systemSleep, su.OSCall.Name == asyncSystemSleep:
			return s.sleep(ctx, su)
		}
	}
	return &Progress{Suspension: su}, nil
}

// failed returns err for a turn that ended in an error. A worker that
// rejects an answer keeps the feed paused, and one that could not even
// decode it does not say; the feed is then aborted so the session is idle
// again. Aborting a feed that already ended is a harmless error reply.
func (s *Session) failed(ctx context.Context, ev *pb.WorkerEvent, resumed bool, err error) error {
	suspended := ev.GetFeedSuspended() || (resumed && ev.FeedSuspended == nil)
	if !suspended || s.w.dead {
		return err
	}
	events, aerr := s.send(context.WithoutCancel(ctx), abortFeed("RuntimeError", err.Error()))
	if aerr != nil {
		return aerr
	}
	if k, ok := events[len(events)-1].Kind.(*pb.WorkerEvent_FatalError); ok {
		s.w.dead = true
		return &WorkerError{Message: k.FatalError.GetMessage(), Stderr: s.w.stderr.String()}
	}
	return err
}

// sleep waits out a system sleep: capped per call, charged to the session's
// sleep budget and never shown to an OSHandler. During a Run an
// asyncio.sleep is deferred instead, so the sandbox's other tasks keep
// running.
func (s *Session) sleep(ctx context.Context, su *Suspension) (*Progress, error) {
	d := time.Duration(su.OSCall.Seconds * float64(time.Second))
	if d < 0 || d > s.sleepMax {
		d = s.sleepMax
	}
	if total := s.sleepUsed + d; s.sleepLimit > 0 && total > s.sleepLimit {
		return su.answer(ctx, abortFeed("TimeoutError", fmt.Sprintf("sleep limit exceeded: %v > %v", total, s.sleepLimit)))
	}
	s.sleepUsed += d
	if s.driver != nil && su.OSCall.Name == asyncSystemSleep {
		s.driver.sleep(su.CallID, d)
		return su.deferCall(ctx)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return su.ret(ctx, nil)
	case <-ctx.Done():
		_ = su.abort(context.WithoutCancel(ctx), "RuntimeError", "cancelled while sleeping")
		return nil, context.Cause(ctx)
	}
}

// protocolError reports a malformed suspension: the worker is paused with a
// payload the host cannot interpret, so the feed is aborted.
func (s *Session) protocolError(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if s.pending != nil && !s.w.dead {
		_ = s.pending.abort(context.Background(), "RuntimeError", msg)
	}
	return &WorkerError{Message: msg}
}

// resolveReceiver attaches the host object or class a routed call targets.
func (s *Session) resolveReceiver(su *Suspension, id *pb.Uuid) {
	if id == nil {
		return
	}
	su.ObjectID = uuidString(id)
	if o, ok := s.store.objects[su.ObjectID]; ok {
		su.Object = o
	} else if c, ok := s.store.classes[su.ObjectID]; ok {
		su.Class = c
	}
}

// Dump serialises the session into bytes that Runtime.Restore turns back
// into a session, in this or another process. It works both between runs
// and while paused at a host call, in which case the restored session is
// paused at the same call.
func (s *Session) Dump(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dump(ctx)
}

func (s *Session) dump(ctx context.Context) ([]byte, error) {
	if s.closed {
		return nil, ErrClosed
	}
	events, err := s.send(ctx, &pb.ClientRequest{Kind: &pb.ClientRequest_Dump{Dump: &pb.Dump{}}})
	if err != nil {
		return nil, err
	}
	last := events[len(events)-1]
	if k, ok := last.Kind.(*pb.WorkerEvent_DumpResult); ok {
		return k.DumpResult.GetState(), nil
	}
	return nil, s.expectOk(events, "Dump")
}

// Reset drops all session state, including a paused run and host objects,
// and reconfigures the sandbox on the same wasm instance.
func (s *Session) Reset(ctx context.Context, opts SessionOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.reset(ctx); err != nil {
		return err
	}
	return s.configure(ctx, opts)
}

func (s *Session) reset(ctx context.Context) error {
	if s.pending != nil {
		s.pending.done = true
		s.pending = nil
	}
	s.store = newObjectStore()
	events, err := s.send(ctx, &pb.ClientRequest{Kind: &pb.ClientRequest_Reset_{Reset_: &pb.Reset{}}})
	if err != nil {
		return err
	}
	return s.expectOk(events, "Reset")
}

// Wait blocks until every AsyncFunctions call the session's runs started has
// returned, and reports how many were still running. A call the code never
// awaited outlives its run; it sees the run's context, so cancel that first
// to stop such calls instead of letting them finish.
func (s *Session) Wait() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	running := int(s.running.Load())
	s.calls.Wait()
	return running
}

// Close releases the sandbox instance, or hands it back to the Pool it came
// from.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.release != nil {
		s.release(ctx, s.w)
		return nil
	}
	return s.w.close(ctx)
}

// ---------------------------------------------------------------------------
// Suspensions
// ---------------------------------------------------------------------------

// Progress is the state of a run: complete with Value, or paused at
// Suspension.
type Progress struct {
	// Value is the run's result once Done.
	Value any
	// Suspension is the pending host call while not Done.
	Suspension *Suspension
}

// Done reports whether the run completed.
func (p *Progress) Done() bool { return p.Suspension == nil }

// SuspensionKind says what a paused run is waiting for.
type SuspensionKind int

const (
	// SuspensionCall is a host function call, or a method call on a host
	// object or class when Object or Class is set.
	SuspensionCall SuspensionKind = iota + 1
	// SuspensionLookup is a read of an undefined name, or of a lazy
	// attribute of a host object when Object or Class is set.
	SuspensionLookup
	// SuspensionOS is a filesystem, environment or clock request.
	SuspensionOS
	// SuspensionFutures means every sandbox task is blocked on host calls
	// that were answered with Defer.
	SuspensionFutures
)

// Suspension is a paused run waiting for the host. Exactly one of its answer
// methods (Return, Raise, Decline, Defer, Settle) resumes it; each returns
// the next Progress. A Suspension that was answered, or whose session was
// Reset, rejects further answers.
type Suspension struct {
	Kind SuspensionKind
	// Call is the pending call for SuspensionCall.
	Call *Call
	// Name is the looked-up name for SuspensionLookup.
	Name string
	// OSCall is the pending request for SuspensionOS.
	OSCall *OSCall
	// PendingCallIDs lists the deferred calls the sandbox is blocked on for
	// SuspensionFutures.
	PendingCallIDs []uint32
	// Object is the receiver of a method call or lazy attribute read on a
	// host object; Class the same for a host class (construction arrives as
	// a call named "__call__"). ObjectID is the raw receiver identity, set
	// even when the object is unknown to this session (after Restore).
	Object   *HostObject
	Class    *HostClass
	ObjectID string
	// CallID identifies the call for Defer and Settle.
	CallID uint32

	s    *Session
	done bool
}

// Result is the outcome of a deferred call, for Settle.
type Result struct {
	Value any
	Err   error
}

// Return resumes the run with value as the call's result, the attribute's
// value, or the OS call's result.
func (su *Suspension) Return(ctx context.Context, value any) (*Progress, error) {
	su.s.mu.Lock()
	defer su.s.mu.Unlock()
	if err := su.check(); err != nil {
		return nil, err
	}
	return su.ret(ctx, value)
}

func (su *Suspension) ret(ctx context.Context, value any) (*Progress, error) {
	a := su.s.codec().arena()
	switch su.Kind {
	case SuspensionLookup:
		idx, err := a.encode(value)
		if err != nil {
			return su.raise(ctx, Raise("TypeError", "value for %q: %v", su.Name, err))
		}
		return su.answer(ctx, resumeLookup(&pb.ResumeNameLookup{Values: a.proto(), Kind: &pb.ResumeNameLookup_Value{Value: idx}}))
	case SuspensionCall, SuspensionOS:
		var idx uint32
		var err error
		if su.Kind == SuspensionOS {
			idx, err = a.osResult(su.OSCall, value)
		} else {
			idx, err = a.encode(value)
		}
		if err != nil {
			return su.raise(ctx, Raise("TypeError", "host returned an unsupported value: %v", err))
		}
		return su.answer(ctx, resumeCall(su.CallID, &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_ReturnValue{ReturnValue: idx}}, a))
	}
	return nil, errors.New("monty: Return is not valid for a futures suspension; use Settle")
}

// Raise resumes the run by raising err in the sandbox: an *Exception as
// itself, any other error as RuntimeError.
func (su *Suspension) Raise(ctx context.Context, err error) (*Progress, error) {
	su.s.mu.Lock()
	defer su.s.mu.Unlock()
	if cerr := su.check(); cerr != nil {
		return nil, cerr
	}
	return su.raise(ctx, err)
}

func (su *Suspension) raise(ctx context.Context, err error) (*Progress, error) {
	switch su.Kind {
	case SuspensionLookup:
		return su.answer(ctx, resumeLookup(&pb.ResumeNameLookup{Kind: &pb.ResumeNameLookup_Error{Error: raisedFromError(err)}}))
	case SuspensionCall, SuspensionOS:
		return su.answer(ctx, resumeCall(su.CallID, callError(err), nil))
	}
	exc := raisedFromError(err)
	return su.answer(ctx, abortFeed(exc.GetExcType(), exc.GetMessage()))
}

// Decline resumes the run without an answer: a name lookup raises NameError
// (AttributeError for an attribute), a function call NameError, an OS call
// its default error (PermissionError for filesystem calls).
func (su *Suspension) Decline(ctx context.Context) (*Progress, error) {
	su.s.mu.Lock()
	defer su.s.mu.Unlock()
	if err := su.check(); err != nil {
		return nil, err
	}
	return su.decline(ctx)
}

func (su *Suspension) decline(ctx context.Context) (*Progress, error) {
	switch su.Kind {
	case SuspensionLookup:
		return su.answer(ctx, resumeLookup(&pb.ResumeNameLookup{Kind: &pb.ResumeNameLookup_Undefined{Undefined: unit()}}))
	case SuspensionCall:
		if su.Object != nil || su.Class != nil || su.ObjectID != "" {
			return su.raise(ctx, Raise("AttributeError", "'%s' object has no attribute '%s'", su.receiverName(), su.Call.Name))
		}
		return su.answer(ctx, resumeCall(su.CallID, &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_NotFound{NotFound: su.Call.Name}}, nil))
	case SuspensionOS:
		return su.answer(ctx, resumeCall(su.CallID, &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_NotHandled{NotHandled: unit()}}, nil))
	}
	return nil, errors.New("monty: Decline is not valid for a futures suspension; use Settle")
}

// Defer resumes the run with a future for this call: the sandbox continues
// (other asyncio tasks run) until it needs the result, when it pauses with
// a SuspensionFutures naming CallID. Settle that with the outcome.
func (su *Suspension) Defer(ctx context.Context) (*Progress, error) {
	su.s.mu.Lock()
	defer su.s.mu.Unlock()
	if err := su.check(); err != nil {
		return nil, err
	}
	return su.deferCall(ctx)
}

func (su *Suspension) deferCall(ctx context.Context) (*Progress, error) {
	if su.Kind != SuspensionCall && su.Kind != SuspensionOS {
		return nil, errors.New("monty: only calls can be deferred")
	}
	return su.answer(ctx, resumeCall(su.CallID, &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_Future{Future: su.CallID}}, nil))
}

// Settle resumes a SuspensionFutures run with the outcomes of some or all of
// its PendingCallIDs. At least one is required; the rest stay pending.
func (su *Suspension) Settle(ctx context.Context, results map[uint32]Result) (*Progress, error) {
	su.s.mu.Lock()
	defer su.s.mu.Unlock()
	if err := su.check(); err != nil {
		return nil, err
	}
	return su.settle(ctx, results)
}

func (su *Suspension) settle(ctx context.Context, results map[uint32]Result) (*Progress, error) {
	if su.Kind != SuspensionFutures {
		return nil, errors.New("monty: Settle is only valid for a futures suspension")
	}
	if len(results) == 0 {
		return nil, errors.New("monty: Settle needs at least one result")
	}
	a := su.s.codec().arena()
	out := make([]*pb.FutureResult, 0, len(results))
	for _, id := range slices.Sorted(maps.Keys(results)) {
		r := results[id]
		var res *pb.ExtFunctionResult
		if r.Err != nil {
			res = callError(r.Err)
		} else if idx, err := a.encode(r.Value); err != nil {
			res = callError(Raise("TypeError", "host returned an unsupported value: %v", err))
		} else {
			res = &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_ReturnValue{ReturnValue: idx}}
		}
		out = append(out, &pb.FutureResult{CallId: id, Result: res})
	}
	return su.answer(ctx, &pb.ClientRequest{Kind: &pb.ClientRequest_ResumeFutures{ResumeFutures: &pb.ResumeFutures{Results: out, Values: a.proto()}}})
}

// Dump serialises the paused run; Runtime.Restore returns a session paused
// at this same call.
func (su *Suspension) Dump(ctx context.Context) ([]byte, error) {
	su.s.mu.Lock()
	defer su.s.mu.Unlock()
	if err := su.check(); err != nil {
		return nil, err
	}
	return su.s.dump(ctx)
}

// Resolve answers this suspension from opts the way Run would, one step.
func (su *Suspension) Resolve(ctx context.Context, opts RunOptions) (*Progress, error) {
	su.s.mu.Lock()
	defer su.s.mu.Unlock()
	if err := su.check(); err != nil {
		return nil, err
	}
	d := newDriver(su.s, &opts)
	defer d.close()
	return d.resolve(ctx, su)
}

func (su *Suspension) check() error {
	if su.s.closed {
		return ErrClosed
	}
	if su.done || su.s.pending != su {
		return errors.New("monty: suspension already answered")
	}
	return nil
}

func (su *Suspension) receiverName() string {
	switch {
	case su.Object != nil && su.Object.Class != nil:
		return su.Object.Class.Name
	case su.Class != nil:
		return su.Class.Name
	}
	return "object"
}

// abort ends the paused feed with an exception; the session stays usable.
func (su *Suspension) abort(ctx context.Context, excType, message string) error {
	_, err := su.answer(ctx, abortFeed(excType, message))
	return err
}

// answer sends the reply for this suspension and classifies what follows.
func (su *Suspension) answer(ctx context.Context, req *pb.ClientRequest) (*Progress, error) {
	su.done = true
	events, err := su.s.send(ctx, req)
	if err != nil && !su.s.w.dead {
		// The answer itself was unsendable (e.g. a result over the frame
		// limit): raise that in the sandbox instead of leaving it paused.
		events, err = su.s.send(ctx, abortFeed("RuntimeError", err.Error()))
	}
	if err != nil {
		su.s.pending = nil
		return nil, err
	}
	return su.s.progress(ctx, events, true)
}

func abortFeed(excType, message string) *pb.ClientRequest {
	return &pb.ClientRequest{Kind: &pb.ClientRequest_AbortFeed{AbortFeed: &pb.AbortFeed{
		Exception: &pb.RaisedException{ExcType: excType, Message: &message},
	}}}
}

// resumeCall answers a call; a holds the returned value, if any.
func resumeCall(callID uint32, result *pb.ExtFunctionResult, a *arena) *pb.ClientRequest {
	resume := &pb.ResumeCall{CallId: callID, Result: result}
	if a != nil {
		resume.Values = a.proto()
	}
	return &pb.ClientRequest{Kind: &pb.ClientRequest_ResumeCall{ResumeCall: resume}}
}

func resumeLookup(answer *pb.ResumeNameLookup) *pb.ClientRequest {
	return &pb.ClientRequest{Kind: &pb.ClientRequest_ResumeNameLookup{ResumeNameLookup: answer}}
}

func callError(err error) *pb.ExtFunctionResult {
	return &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_Error{Error: raisedFromError(err)}}
}

// ---------------------------------------------------------------------------
// Automatic resolution (Run, Continue, Resolve)
// ---------------------------------------------------------------------------

// futureResult is one settled AsyncFunctions call.
type futureResult struct {
	id  uint32
	res Result
}

// driver answers suspensions from RunOptions, running AsyncFunctions in
// goroutines and settling their futures when the sandbox blocks on them.
type driver struct {
	s       *Session
	opts    *RunOptions
	results chan futureResult
	done    chan struct{}
	pending map[uint32]bool
	settled map[uint32]Result
}

func newDriver(s *Session, opts *RunOptions) *driver {
	return &driver{
		s:       s,
		opts:    opts,
		results: make(chan futureResult),
		done:    make(chan struct{}),
		pending: map[uint32]bool{},
		settled: map[uint32]Result{},
	}
}

// own makes d the session's driver until close: a run that resolves every
// suspension can wait out an asyncio.sleep in the background. A single
// Resolve cannot, since nothing would settle the sleep afterwards.
func (d *driver) own() *driver {
	d.s.driver = d
	return d
}

// close abandons outstanding goroutines; they exit at their next send, once
// their call has returned. Session.Wait joins those calls.
func (d *driver) close() {
	if d.s.driver == d {
		d.s.driver = nil
	}
	close(d.done)
}

// sleep settles the deferred call id once delay has passed.
func (d *driver) sleep(id uint32, delay time.Duration) {
	d.pending[id] = true
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-d.done:
			return
		}
		select {
		case d.results <- futureResult{id: id}:
		case <-d.done:
		}
	}()
}

func (d *driver) resolve(ctx context.Context, su *Suspension) (*Progress, error) {
	switch su.Kind {
	case SuspensionCall:
		return d.resolveCall(ctx, su)
	case SuspensionLookup:
		return d.resolveLookup(ctx, su)
	case SuspensionOS:
		if d.opts.OS == nil {
			return su.decline(ctx)
		}
		v, err := d.opts.OS.HandleOSCall(ctx, su.OSCall)
		if errors.Is(err, ErrNotHandled) {
			return su.decline(ctx)
		}
		if err != nil {
			return su.raise(ctx, err)
		}
		return su.ret(ctx, v)
	case SuspensionFutures:
		return d.resolveFutures(ctx, su)
	}
	return nil, fmt.Errorf("monty: unknown suspension kind %d", su.Kind)
}

func (d *driver) resolveCall(ctx context.Context, su *Suspension) (*Progress, error) {
	name := su.Call.Name
	switch {
	case su.Object != nil || su.Class != nil || su.ObjectID != "":
		return d.resolveMethod(ctx, su)
	case d.opts.Functions[name] != nil:
		v, err := d.opts.Functions[name](ctx, su.Call)
		if err != nil {
			return su.raise(ctx, err)
		}
		return su.ret(ctx, v)
	case d.opts.AsyncFunctions[name] != nil:
		fn := d.opts.AsyncFunctions[name]
		id, call := su.CallID, su.Call
		d.pending[id] = true
		// Counted here, not in the goroutine: the run can end before it starts.
		d.s.calls.Add(1)
		d.s.running.Add(1)
		go func() {
			v, err := fn(ctx, call)
			d.s.running.Add(-1)
			d.s.calls.Done()
			select {
			case d.results <- futureResult{id: id, res: Result{Value: v, Err: err}}:
			case <-d.done:
			}
		}()
		return su.deferCall(ctx)
	}
	// An undefined name that is called right away arrives as a call, not a
	// lookup: a host class in Values is constructed, anything else is not
	// callable.
	if v, ok := d.opts.Values[name]; ok {
		if cl, ok := v.(*HostClass); ok {
			d.s.store.registerClass(cl)
			su.Class = cl
			su.Call.Name = "__call__"
			return d.resolveMethod(ctx, su)
		}
		return su.raise(ctx, Raise("TypeError", "'%s' object is not callable", pythonTypeName(v)))
	}
	return su.decline(ctx)
}

// pythonTypeName names a Go value the way Python would in a TypeError.
func pythonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "int"
	case float32, float64:
		return "float"
	case string:
		return "str"
	case []byte:
		return "bytes"
	case *HostObject:
		if o := v.(*HostObject); o.Class != nil {
			return o.Class.Name
		}
	}
	return "object"
}

func (d *driver) resolveMethod(ctx context.Context, su *Suspension) (*Progress, error) {
	name := su.Call.Name
	if strings.HasPrefix(name, "_") && name != "__call__" {
		return su.decline(ctx)
	}
	switch {
	case su.Class != nil && name == "__call__":
		if su.Class.Init == nil {
			return su.raise(ctx, Raise("TypeError", "cannot instantiate host class '%s'", su.Class.Name))
		}
		obj, err := su.Class.Init(ctx, su.Call)
		if err != nil {
			return su.raise(ctx, err)
		}
		return su.ret(ctx, obj)
	case su.Class != nil:
		fn := su.Class.ClassMethods[name]
		if fn == nil {
			return su.decline(ctx)
		}
		v, err := fn(ctx, su.Call)
		if err != nil {
			return su.raise(ctx, err)
		}
		return su.ret(ctx, v)
	case su.Object != nil:
		m := su.Object.Class.Methods[name]
		if m == nil {
			return su.decline(ctx)
		}
		v, err := m(ctx, su.Object, su.Call)
		if err != nil {
			return su.raise(ctx, err)
		}
		return su.ret(ctx, v)
	}
	return su.raise(ctx, Raise("RuntimeError",
		"no host object registered for method call '%s' (id %s): host objects do not survive a dump", name, su.ObjectID))
}

func (d *driver) resolveLookup(ctx context.Context, su *Suspension) (*Progress, error) {
	name := su.Name
	if su.Object != nil || su.Class != nil || su.ObjectID != "" {
		if strings.HasPrefix(name, "_") {
			return su.decline(ctx)
		}
		if su.Object != nil && su.Object.Class.Lazy != nil {
			v, err := su.Object.Class.Lazy(ctx, su.Object, name)
			if errors.Is(err, ErrUndefined) {
				return su.decline(ctx)
			}
			if err != nil {
				return su.raise(ctx, err)
			}
			return su.ret(ctx, v)
		}
		return su.decline(ctx)
	}
	if d.opts.Functions[name] != nil || d.opts.AsyncFunctions[name] != nil {
		return su.ret(ctx, FunctionRef{Name: name})
	}
	if v, ok := d.opts.Values[name]; ok {
		return su.ret(ctx, v)
	}
	return su.decline(ctx)
}

func (d *driver) resolveFutures(ctx context.Context, su *Suspension) (*Progress, error) {
	results := map[uint32]Result{}
	for _, id := range su.PendingCallIDs {
		if r, ok := d.settled[id]; ok {
			results[id] = r
			delete(d.settled, id)
		}
	}
	// Block until at least one of the awaited calls finishes.
	for len(results) == 0 {
		waiting := false
		for _, id := range su.PendingCallIDs {
			if d.pending[id] {
				waiting = true
			}
		}
		if !waiting {
			return su.raise(ctx, Raise("RuntimeError", "sandbox is blocked on futures this run does not own"))
		}
		select {
		case r := <-d.results:
			delete(d.pending, r.id)
			if contains(su.PendingCallIDs, r.id) {
				results[r.id] = r.res
			} else {
				d.settled[r.id] = r.res
			}
		case <-ctx.Done():
			return su.raise(ctx, Raise("RuntimeError", "cancelled while waiting for host calls: %v", ctx.Err()))
		}
	}
	return su.settle(ctx, results)
}

func contains(ids []uint32, id uint32) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
