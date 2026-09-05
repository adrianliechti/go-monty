package monty

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
	"google.golang.org/protobuf/proto"

	"github.com/adrianliechti/go-monty/internal/pb"
)

// maxFrameLen mirrors monty-proto's MAX_FRAME_LEN (256 MiB).
const maxFrameLen = 256 * 1024 * 1024

const (
	outcomeContinue = 0
	outcomeShutdown = 1
	outcomeFatal    = 2
)

// hostModule is the wasm import module the worker streams events through.
const hostModule = "monty"

// collectorKey carries the per-call event collector to the host import.
type collectorKey struct{}

// eventCollector receives the framed events of one turn as they happen.
type eventCollector struct {
	onPrint func(*pb.Print)
	events  []*pb.ChildEvent
	err     error
}

// instantiateHostModule registers the "monty" import module on rt.
func instantiateHostModule(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder(hostModule).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, m api.Module, ptr, length uint32) {
			c, _ := ctx.Value(collectorKey{}).(*eventCollector)
			if c == nil || c.err != nil {
				return
			}
			frame, ok := m.Memory().Read(ptr, length)
			if !ok {
				c.err = &WorkerError{Message: "event frame out of bounds"}
				return
			}
			if length < 4 || binary.LittleEndian.Uint32(frame) != length-4 {
				c.err = &WorkerError{Message: "malformed event frame"}
				return
			}
			ev := &pb.ChildEvent{}
			if err := proto.Unmarshal(frame[4:], ev); err != nil {
				c.err = &WorkerError{Message: "undecodable event frame", Err: err}
				return
			}
			if p, ok := ev.Kind.(*pb.ChildEvent_Print); ok {
				if c.onPrint != nil {
					c.onPrint(p.Print)
				}
				return
			}
			c.events = append(c.events, ev)
		}).
		Export("event").
		Instantiate(ctx)
	return err
}

// worker is one instantiated wasm module holding one Monty protocol child.
// Not safe for concurrent use; Session serialises access.
type worker struct {
	mod      api.Module
	alloc    api.Function
	dispatch api.Function
	stderr   *tailBuffer
	dead     bool
	// runs counts the sessions this instance has served (pool recycling).
	runs int
}

func newWorker(mod api.Module) (*worker, error) {
	w := &worker{mod: mod}
	for name, dst := range map[string]*api.Function{
		"monty_alloc":    &w.alloc,
		"monty_dispatch": &w.dispatch,
	} {
		fn := mod.ExportedFunction(name)
		if fn == nil {
			return nil, fmt.Errorf("monty: module does not export %s", name)
		}
		*dst = fn
	}
	if mod.Memory() == nil {
		return nil, errors.New("monty: module does not export its memory")
	}
	return w, nil
}

func (w *worker) versions(ctx context.Context) (version string, protocol uint32, err error) {
	pv, err := w.mod.ExportedFunction("monty_protocol_version").Call(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("monty: read protocol version: %w", err)
	}
	ptr, err := w.mod.ExportedFunction("monty_version_ptr").Call(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("monty: read version: %w", err)
	}
	n, err := w.mod.ExportedFunction("monty_version_len").Call(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("monty: read version: %w", err)
	}
	b, ok := w.mod.Memory().Read(uint32(ptr[0]), uint32(n[0]))
	if !ok {
		return "", 0, errors.New("monty: version string out of bounds")
	}
	return string(b), uint32(pv[0]), nil
}

// memorySize is the current linear memory size in bytes.
func (w *worker) memorySize() uint64 {
	return uint64(w.mod.Memory().Size())
}

func (w *worker) close(ctx context.Context) error {
	w.dead = true
	return w.mod.Close(ctx)
}

// send runs one protocol turn. Print events go to onPrint as they happen;
// the turn's remaining events are returned. A returned error other than an
// encoding error means the worker is dead.
func (w *worker) send(ctx context.Context, req *pb.ParentRequest, onPrint func(*pb.Print)) ([]*pb.ChildEvent, error) {
	if w.dead {
		return nil, ErrClosed
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("monty: encode request: %w", err)
	}
	if len(body) > maxFrameLen {
		return nil, fmt.Errorf("monty: request of %d bytes exceeds the %d byte frame limit", len(body), maxFrameLen)
	}
	frame := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)

	res, err := w.alloc.Call(ctx, uint64(len(frame)))
	if err != nil {
		return nil, w.died("allocating request buffer", err)
	}
	ptr := uint32(res[0])
	if !w.mod.Memory().Write(ptr, frame) {
		w.dead = true
		return nil, &WorkerError{Message: "request buffer out of bounds"}
	}

	c := &eventCollector{onPrint: onPrint}
	res, err = w.dispatch.Call(context.WithValue(ctx, collectorKey{}, c), uint64(ptr), uint64(len(frame)))
	if err != nil {
		return nil, w.died("running request", err)
	}
	if c.err != nil {
		w.dead = true
		return nil, c.err
	}
	if uint32(res[0]) != outcomeContinue {
		w.dead = true
	}
	if len(c.events) == 0 {
		w.dead = true
		return nil, &WorkerError{Message: "turn ended without a terminal event", Stderr: w.stderr.String()}
	}
	return c.events, nil
}

// died classifies a failed wasm call: a context cancellation, an explicit
// exit, or a trap (OOM abort, stack overflow, panic).
func (w *worker) died(during string, err error) error {
	w.dead = true
	msg := "failed " + during
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case sys.ExitCodeContextCanceled:
			return &WorkerError{Message: msg, Stderr: w.stderr.String(), Err: context.Canceled}
		case sys.ExitCodeDeadlineExceeded:
			return &WorkerError{Message: msg, Stderr: w.stderr.String(), Err: context.DeadlineExceeded}
		}
		msg = fmt.Sprintf("%s (exit code %d)", msg, exit.ExitCode())
	}
	return &WorkerError{Message: msg, Stderr: w.stderr.String(), Err: err}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = t.buf[:0]
}

func (t *tailBuffer) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.buf), "")
}
