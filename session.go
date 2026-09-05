package monty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/go-monty/internal/pb"
)

// Limits bounds a session. Zero values mean unlimited, except
// MaxRecursionDepth and MaxSuspensions, which default to 1000.
type Limits struct {
	// MaxDuration caps the time the interpreter spends executing bytecode.
	// Time spent in host functions does not count. Exceeding it raises
	// TimeoutError in the sandbox.
	MaxDuration time.Duration
	// MaxMemory caps the sandbox heap in bytes. Exceeding it raises
	// MemoryError; a burst past the ceiling above it kills the worker.
	MaxMemory uint64
	// GCInterval is the number of allocations between cycle collections.
	GCInterval uint64
	// MaxRecursionDepth caps Python call depth (RecursionError).
	MaxRecursionDepth uint64
	// MaxSuspensions caps host calls (functions, name lookups, OS calls) over
	// the life of the session, as upstream does. Exceeding it aborts the
	// current run with a RuntimeError; Reset starts the count over.
	MaxSuspensions uint64
}

func (l *Limits) proto() *pb.ResourceLimits {
	if l == nil {
		return nil
	}
	out := &pb.ResourceLimits{}
	if l.MaxDuration > 0 {
		v := uint64(l.MaxDuration / time.Microsecond)
		out.MaxDurationMicros = &v
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
	}
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
	// each call pausing the sandbox until it returns. Their names stay bound
	// in the session's globals for later runs.
	Functions map[string]Function
	// AsyncFunctions are callable like Functions but run in their own
	// goroutines while the sandbox continues. Code that needs the result
	// (awaiting it, or using it) waits for it; asyncio.gather over several
	// calls runs them concurrently.
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
	return StartOptions{Inputs: o.Inputs, Stdout: o.Stdout, Stderr: o.Stderr, SkipTypeCheck: o.SkipTypeCheck}
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
	executionTime  time.Duration
	pending        *Suspension
	stdout         io.Writer
	stderr         io.Writer
	closed         bool
	// release returns the worker to a Pool instead of closing it.
	release func(ctx context.Context, w *worker)
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
func (r *Runtime) Restore(ctx context.Context, state []byte) (*Session, error) {
	s, err := r.newSession(ctx)
	if err != nil {
		return nil, err
	}
	s.stdout, s.stderr = os.Stdout, os.Stderr
	events, err := s.send(ctx, &pb.ParentRequest{Kind: &pb.ParentRequest_Load{Load: &pb.Load{State: state}}})
	if err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	last := events[len(events)-1]
	s.noteBudget(last)
	if _, ok := last.Kind.(*pb.ChildEvent_Ok); ok {
		return s, nil
	}
	if _, err := s.progress(ctx, events); err != nil {
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
	return &Session{rt: r, w: w, store: newObjectStore(), maxSuspensions: defaultMaxSuspensions, stdout: os.Stdout, stderr: os.Stderr}
}

func (s *Session) codec() *codec { return &codec{store: s.store} }

// send runs one turn, streaming prints to the session's current writers.
func (s *Session) send(ctx context.Context, req *pb.ParentRequest) ([]*pb.ChildEvent, error) {
	return s.w.send(ctx, req, func(p *pb.Print) {
		w := s.stdout
		if p.GetStream() == pb.PrintStream_PRINT_STREAM_STDERR {
			w = s.stderr
		}
		if w != nil {
			_, _ = io.WriteString(w, p.GetText())
		}
	})
}

func (s *Session) configure(ctx context.Context, opts SessionOptions) error {
	if opts.Limits != nil && opts.Limits.MaxSuspensions > 0 {
		s.maxSuspensions = opts.Limits.MaxSuspensions
	} else {
		s.maxSuspensions = defaultMaxSuspensions
	}
	events, err := s.send(ctx, &pb.ParentRequest{Kind: &pb.ParentRequest_Configure{Configure: opts.proto()}})
	if err != nil {
		return err
	}
	return s.expectOk(events, "Configure")
}

func (s *Session) expectOk(events []*pb.ChildEvent, what string) error {
	last := events[len(events)-1]
	s.noteBudget(last)
	switch k := last.Kind.(type) {
	case *pb.ChildEvent_Ok:
		return nil
	case *pb.ChildEvent_Error:
		return s.exception(k.Error.GetException())
	case *pb.ChildEvent_FatalError:
		return &WorkerError{Message: k.FatalError.GetMessage(), Stderr: s.w.stderr.String()}
	}
	return &WorkerError{Message: fmt.Sprintf("unexpected reply to %s: %T", what, last.Kind)}
}

func (s *Session) exception(e *pb.RaisedException) *Exception {
	return exceptionFromProto(e)
}

func (s *Session) noteBudget(ev *pb.ChildEvent) {
	if ev.MaxSuspensions != nil && *ev.MaxSuspensions > 0 {
		s.maxSuspensions = *ev.MaxSuspensions
	}
	if ev.TotalExecutionMicros > 0 {
		s.executionTime = time.Duration(ev.TotalExecutionMicros) * time.Microsecond
	}
}

// ExecutionTime is the cumulative time the interpreter has spent executing
// this session's code. It excludes time spent in host functions, accumulates
// across runs, survives Dump and Restore, and is what Limits.MaxDuration
// counts against.
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
	c := s.codec()
	inputs := make([]*pb.NamedValue, 0, len(opts.Inputs))
	for name, v := range opts.Inputs {
		obj, err := c.encode(v)
		if err != nil {
			return nil, fmt.Errorf("monty: input %q: %w", name, err)
		}
		inputs = append(inputs, &pb.NamedValue{Name: name, Value: obj})
	}
	req := &pb.ParentRequest{Kind: &pb.ParentRequest_Feed{Feed: &pb.Feed{
		Code:          code,
		Inputs:        inputs,
		SkipTypeCheck: opts.SkipTypeCheck,
	}}}
	events, err := s.send(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.progress(ctx, events)
}

// Run executes code in the session, answering every host call from opts,
// and returns the value of the code's last expression statement (nil when
// there is none), or an error: *Exception when the code raised,
// *TypingError when type checking rejected it, or *WorkerError when the
// sandbox died.
func (s *Session) Run(ctx context.Context, code string, opts RunOptions) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.start(ctx, code, opts.start())
	if err != nil {
		return nil, err
	}
	return s.drive(ctx, p, &opts)
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
	return s.drive(ctx, &Progress{Suspension: s.pending}, &opts)
}

// drive resolves suspensions from opts until the run completes.
func (s *Session) drive(ctx context.Context, p *Progress, opts *RunOptions) (any, error) {
	d := newDriver(s, opts)
	defer d.close()
	// A paused child must always get an answer, or the next run would break
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
// the session paused when the turn ended in a host call.
func (s *Session) progress(ctx context.Context, events []*pb.ChildEvent) (*Progress, error) {
	last := events[len(events)-1]
	s.noteBudget(last)
	s.pending = nil
	su := &Suspension{s: s}
	switch k := last.Kind.(type) {
	case *pb.ChildEvent_Complete:
		v, err := s.codec().decode(k.Complete.GetValue())
		if err != nil {
			return nil, err
		}
		return &Progress{Value: v}, nil
	case *pb.ChildEvent_Error:
		return nil, s.exception(k.Error.GetException())
	case *pb.ChildEvent_TypingError:
		return nil, &TypingError{Diagnostics: k.TypingError.GetDiagnostics()}
	case *pb.ChildEvent_FatalError:
		s.w.dead = true
		return nil, &WorkerError{Message: k.FatalError.GetMessage(), Stderr: s.w.stderr.String()}
	case *pb.ChildEvent_FunctionCall:
		fc := k.FunctionCall
		su.Kind = SuspensionCall
		su.CallID = fc.GetCallId()
		su.Call = &Call{Name: fc.GetFunctionName()}
		c := s.codec()
		var err error
		if su.Call.Args, err = c.decodeList(&pb.ObjectList{Items: fc.GetArgs()}); err != nil {
			return nil, s.protocolError("decoding call arguments: %v", err)
		}
		if su.Call.Kwargs, err = c.decodeStringDict(&pb.Dict{Pairs: fc.GetKwargs()}); err != nil {
			return nil, s.protocolError("decoding call keyword arguments: %v", err)
		}
		s.resolveReceiver(su, fc.GetObjectId())
	case *pb.ChildEvent_NameLookup:
		su.Kind = SuspensionLookup
		su.Name = k.NameLookup.GetName()
		s.resolveReceiver(su, k.NameLookup.GetObjectId())
	case *pb.ChildEvent_OsCall:
		call, err := osCallFromProto(k.OsCall)
		if err != nil {
			return nil, s.protocolError("decoding OS call: %v", err)
		}
		su.Kind = SuspensionOS
		su.CallID = k.OsCall.GetCallId()
		su.OSCall = call
	case *pb.ChildEvent_ResolveFutures:
		su.Kind = SuspensionFutures
		su.PendingCallIDs = k.ResolveFutures.GetPendingCallIds()
	default:
		return nil, &WorkerError{Message: fmt.Sprintf("unexpected event %T", last.Kind)}
	}
	s.pending = su
	if su.Kind != SuspensionFutures {
		s.suspensions++
		if s.maxSuspensions > 0 && s.suspensions > s.maxSuspensions {
			return su.answer(ctx, abortFeed("RuntimeError", fmt.Sprintf("suspension limit %d exceeded", s.maxSuspensions)))
		}
	}
	return &Progress{Suspension: su}, nil
}

// protocolError reports a malformed suspension: the child is paused with a
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
	events, err := s.send(ctx, &pb.ParentRequest{Kind: &pb.ParentRequest_Dump{Dump: &pb.Dump{}}})
	if err != nil {
		return nil, err
	}
	last := events[len(events)-1]
	if k, ok := last.Kind.(*pb.ChildEvent_DumpResult); ok {
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
	s.suspensions = 0
	s.store = newObjectStore()
	events, err := s.send(ctx, &pb.ParentRequest{Kind: &pb.ParentRequest_Reset_{Reset_: &pb.Reset{}}})
	if err != nil {
		return err
	}
	return s.expectOk(events, "Reset")
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
	switch su.Kind {
	case SuspensionLookup:
		obj, err := su.s.codec().encode(value)
		if err != nil {
			return su.raise(ctx, Raise("TypeError", "value for %q: %v", su.Name, err))
		}
		return su.answer(ctx, resumeLookup(&pb.ResumeNameLookup{Kind: &pb.ResumeNameLookup_Value{Value: obj}}))
	case SuspensionCall, SuspensionOS:
		var obj *pb.MontyObject
		var err error
		if su.Kind == SuspensionOS {
			obj, err = encodeOSResult(su.OSCall, value)
		} else {
			obj, err = su.s.codec().encode(value)
		}
		if err != nil {
			return su.raise(ctx, Raise("TypeError", "host returned an unsupported value: %v", err))
		}
		return su.answer(ctx, resumeCall(su.CallID, &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_ReturnValue{ReturnValue: obj}}))
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
		return su.answer(ctx, resumeCall(su.CallID, callError(err)))
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
		return su.answer(ctx, resumeCall(su.CallID, &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_NotFound{NotFound: su.Call.Name}}))
	case SuspensionOS:
		return su.answer(ctx, resumeCall(su.CallID, &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_NotHandled{NotHandled: unit()}}))
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
	return su.answer(ctx, resumeCall(su.CallID, &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_Future{Future: su.CallID}}))
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
	c := su.s.codec()
	out := make([]*pb.FutureResult, 0, len(results))
	for id, r := range results {
		var res *pb.ExtFunctionResult
		if r.Err != nil {
			res = callError(r.Err)
		} else if obj, err := c.encode(r.Value); err != nil {
			res = callError(Raise("TypeError", "host returned an unsupported value: %v", err))
		} else {
			res = &pb.ExtFunctionResult{Kind: &pb.ExtFunctionResult_ReturnValue{ReturnValue: obj}}
		}
		out = append(out, &pb.FutureResult{CallId: id, Result: res})
	}
	return su.answer(ctx, &pb.ParentRequest{Kind: &pb.ParentRequest_ResumeFutures{ResumeFutures: &pb.ResumeFutures{Results: out}}})
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
func (su *Suspension) answer(ctx context.Context, req *pb.ParentRequest) (*Progress, error) {
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
	return su.s.progress(ctx, events)
}

func abortFeed(excType, message string) *pb.ParentRequest {
	return &pb.ParentRequest{Kind: &pb.ParentRequest_AbortFeed{AbortFeed: &pb.AbortFeed{
		Exception: &pb.RaisedException{ExcType: excType, Message: &message},
	}}}
}

func resumeCall(callID uint32, result *pb.ExtFunctionResult) *pb.ParentRequest {
	return &pb.ParentRequest{Kind: &pb.ParentRequest_ResumeCall{ResumeCall: &pb.ResumeCall{
		CallId: callID, Result: result,
	}}}
}

func resumeLookup(answer *pb.ResumeNameLookup) *pb.ParentRequest {
	return &pb.ParentRequest{Kind: &pb.ParentRequest_ResumeNameLookup{ResumeNameLookup: answer}}
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

// close abandons outstanding goroutines; they exit at their next send.
func (d *driver) close() { close(d.done) }

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
		go func() {
			v, err := fn(ctx, call)
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
