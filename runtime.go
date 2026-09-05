package monty

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/adrianliechti/go-monty/internal/wasm"
)

// protocolVersion is the monty-proto wire version this package speaks. The
// embedded module must serve it.
const protocolVersion = 2

// Runtime holds the compiled sandbox module. It is safe for concurrent use;
// create one per process and derive Sessions from it.
type Runtime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	version  string

	mu     sync.Mutex
	closed bool
}

type runtimeConfig struct {
	module      []byte
	cacheDir    string
	interpreter bool
	memoryLimit uint32 // pages
}

// RuntimeOption configures NewRuntime.
type RuntimeOption func(*runtimeConfig)

// WithModule replaces the embedded wasm module, e.g. with a custom build of
// rust/ against a different upstream Monty revision.
func WithModule(module []byte) RuntimeOption {
	return func(c *runtimeConfig) { c.module = module }
}

// WithCacheDir caches the compiled module on disk so later NewRuntime calls in
// other processes skip compilation (which takes a few seconds for the 19 MB
// module). The directory is created if missing.
func WithCacheDir(dir string) RuntimeOption {
	return func(c *runtimeConfig) { c.cacheDir = dir }
}

// WithInterpreter forces wazero's interpreter instead of its native compiler.
// Slower, but works on every platform Go supports.
func WithInterpreter() RuntimeOption {
	return func(c *runtimeConfig) { c.interpreter = true }
}

// WithMemoryLimit caps each session's linear memory in bytes (rounded up to
// 64 KiB pages, at most 4 GiB). Independent of Limits.MaxMemory, which the
// sandbox enforces itself and reports as a MemoryError; this is the hard wall
// behind it.
func WithMemoryLimit(bytes uint64) RuntimeOption {
	return func(c *runtimeConfig) {
		pages := (bytes + 65535) / 65536
		if pages > 65536 {
			pages = 65536
		}
		c.memoryLimit = uint32(pages)
	}
}

// NewRuntime compiles the sandbox module.
func NewRuntime(ctx context.Context, opts ...RuntimeOption) (*Runtime, error) {
	cfg := runtimeConfig{module: wasm.Module}
	for _, opt := range opts {
		opt(&cfg)
	}

	var rcfg wazero.RuntimeConfig
	if cfg.interpreter {
		rcfg = wazero.NewRuntimeConfigInterpreter()
	} else {
		rcfg = wazero.NewRuntimeConfig()
	}
	rcfg = rcfg.WithCloseOnContextDone(true)
	if cfg.memoryLimit > 0 {
		rcfg = rcfg.WithMemoryLimitPages(cfg.memoryLimit)
	}
	if cfg.cacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(cfg.cacheDir)
		if err != nil {
			return nil, fmt.Errorf("monty: compilation cache: %w", err)
		}
		rcfg = rcfg.WithCompilationCache(cache)
	}

	rt := wazero.NewRuntimeWithConfig(ctx, rcfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("monty: instantiate wasi: %w", err)
	}
	if err := instantiateHostModule(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("monty: instantiate host module: %w", err)
	}
	compiled, err := rt.CompileModule(ctx, cfg.module)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("monty: compile module: %w", err)
	}
	r := &Runtime{rt: rt, compiled: compiled}

	// Instantiate once to read the versions and fail early on a bad module.
	w, err := r.newWorker(ctx, io.Discard)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	version, proto, err := w.versions(ctx)
	_ = w.close(ctx)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	if proto != protocolVersion {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("monty: module speaks protocol version %d, this package speaks %d", proto, protocolVersion)
	}
	r.version = version
	return r, nil
}

// Version is the upstream Monty version embedded in the module.
func (r *Runtime) Version() string { return r.version }

// Close releases the runtime and every Session created from it.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.rt.Close(ctx)
}

func (r *Runtime) newWorker(ctx context.Context, stderr io.Writer) (*worker, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	mcfg := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions().
		WithSysNanotime().
		WithSysWalltime().
		WithRandSource(rand.Reader).
		WithStderr(stderr)
	mod, err := r.rt.InstantiateModule(ctx, r.compiled, mcfg)
	if err != nil {
		return nil, fmt.Errorf("monty: instantiate module: %w", err)
	}
	w, err := newWorker(mod)
	if err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	if tb, ok := stderr.(*tailBuffer); ok {
		w.stderr = tb
	}
	return w, nil
}
