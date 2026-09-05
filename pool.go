package monty

import (
	"context"
	"sync"

	"github.com/adrianliechti/go-monty/internal/pb"
)

// PoolOptions configures a Pool.
type PoolOptions struct {
	// Idle is the number of instantiated, unused workers kept warm.
	// Defaults to 4.
	Idle int
	// MaxRuns is the number of sessions a worker serves before it is
	// discarded. Defaults to 100; 0 keeps workers indefinitely.
	MaxRuns int
	// MaxMemory discards a worker whose linear memory grew past this many
	// bytes, since wasm memory never shrinks. Defaults to 256 MiB.
	MaxMemory uint64
}

// Pool keeps instantiated workers ready so Checkout returns a Session in
// microseconds instead of the ~2 ms a fresh instance takes, and recycles
// instances that have grown or aged. Sessions from a Pool go back to it on
// Close.
type Pool struct {
	rt   *Runtime
	opts PoolOptions

	mu     sync.Mutex
	idle   []*worker
	closed bool
	fill   chan struct{}
}

// NewPool creates a Pool and starts warming Idle workers in the background.
func (r *Runtime) NewPool(ctx context.Context, opts PoolOptions) *Pool {
	if opts.Idle <= 0 {
		opts.Idle = 4
	}
	if opts.MaxRuns == 0 {
		opts.MaxRuns = 100
	}
	if opts.MaxMemory == 0 {
		opts.MaxMemory = 256 << 20
	}
	p := &Pool{rt: r, opts: opts, fill: make(chan struct{}, 1)}
	go p.filler(context.WithoutCancel(ctx))
	p.kick()
	return p
}

// kick asks the filler to top up the idle set.
func (p *Pool) kick() {
	select {
	case p.fill <- struct{}{}:
	default:
	}
}

func (p *Pool) filler(ctx context.Context) {
	for range p.fill {
		for {
			p.mu.Lock()
			need := !p.closed && len(p.idle) < p.opts.Idle
			p.mu.Unlock()
			if !need {
				break
			}
			w, err := p.rt.newWorker(ctx, newTailBuffer(64*1024))
			if err != nil {
				break
			}
			if !warm(ctx, w) {
				_ = w.close(ctx)
				break
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				_ = w.close(ctx)
				break
			}
			p.idle = append(p.idle, w)
			p.mu.Unlock()
		}
	}
}

// warm makes the child build its interpreter and type checker now, which
// the first request otherwise pays for (~2 ms). Reset is valid before a
// session exists and leaves the worker in the no-session state.
func warm(ctx context.Context, w *worker) bool {
	events, err := w.send(ctx, &pb.ParentRequest{Kind: &pb.ParentRequest_Reset_{Reset_: &pb.Reset{}}}, nil)
	if err != nil {
		return false
	}
	_, ok := events[len(events)-1].Kind.(*pb.ChildEvent_Ok)
	return ok
}

// Checkout returns a configured Session on a warm worker, instantiating one
// only when the pool is empty.
func (p *Pool) Checkout(ctx context.Context, opts SessionOptions) (*Session, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	var w *worker
	if n := len(p.idle); n > 0 {
		w = p.idle[n-1]
		p.idle = p.idle[:n-1]
	}
	p.mu.Unlock()
	p.kick()
	if w == nil {
		var err error
		if w, err = p.rt.newWorker(ctx, newTailBuffer(64*1024)); err != nil {
			return nil, err
		}
	}
	s := p.rt.sessionFor(w)
	s.release = p.release
	if err := s.configure(ctx, opts); err != nil {
		s.release = nil
		_ = w.close(ctx)
		return nil, err
	}
	return s, nil
}

// release takes a worker back from a closed Session, resetting it, or
// discards it when it is dead, aged or bloated.
func (p *Pool) release(ctx context.Context, w *worker) {
	healthy := !w.dead &&
		(p.opts.MaxRuns == 0 || w.runs < p.opts.MaxRuns) &&
		w.memorySize() <= p.opts.MaxMemory
	if healthy {
		events, err := w.send(ctx, &pb.ParentRequest{Kind: &pb.ParentRequest_Reset_{Reset_: &pb.Reset{}}}, nil)
		if err != nil {
			healthy = false
		} else if _, ok := events[len(events)-1].Kind.(*pb.ChildEvent_Ok); !ok {
			healthy = false
		}
	}
	if healthy {
		w.stderr.Reset()
		p.mu.Lock()
		if !p.closed && len(p.idle) < p.opts.Idle {
			p.idle = append(p.idle, w)
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
	_ = w.close(ctx)
	p.kick()
}

// Stats reports the pool's idle worker count.
func (p *Pool) Stats() (idle int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// Close discards the idle workers. Sessions still checked out keep working
// and are closed for good when they are closed.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	close(p.fill)
	p.mu.Unlock()
	for _, w := range idle {
		_ = w.close(ctx)
	}
	return nil
}
