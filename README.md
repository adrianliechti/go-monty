# go-monty

Go bindings for [Monty](https://github.com/pydantic/monty), Pydantic's sandboxed
Python interpreter for code written by AI, **without cgo and without native
libraries**. The upstream Rust interpreter is compiled to WebAssembly, embedded
in the package, and executed with [wazero](https://wazero.io), a pure-Go wasm
runtime. One `go get`, any `GOOS/GOARCH` Go supports, `CGO_ENABLED=0`.

```bash
go get github.com/adrianliechti/go-monty
```

## Example

```go
package main

import (
	"context"
	"fmt"
	"log"

	monty "github.com/adrianliechti/go-monty"
)

const code = `
kcal = nutrition('chocolate bar')['kcal']
hours = kcal * 4184 / (bulb_watts * 3600)
print(f'a chocolate bar powers a {bulb_watts} W bulb for {hours:.1f} hours')
round(hours, 1)
`

func main() {
	ctx := context.Background()

	rt, err := monty.NewRuntime(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)

	session, err := rt.NewSession(ctx, monty.SessionOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close(ctx)

	result, err := session.Run(ctx, code, monty.RunOptions{
		Inputs: map[string]any{"bulb_watts": 10},
		Functions: map[string]monty.Function{
			"nutrition": func(ctx context.Context, call *monty.Call) (any, error) {
				return map[string]any{"kcal": 230}, nil
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result) // 26.7
}
```

`nutrition` ran in Go; the sandbox saw only its return value.

## What Monty is (and is not)

Monty runs the glue code a model writes: loops, branching, `json`, `math`,
`datetime`, `re`, `dataclasses`, `asyncio.gather`, and calls to the host
functions you expose. It starts in milliseconds, can be paused and restored,
and enforces memory, time and recursion limits itself. It implements a
[subset of Python](https://pydantic.dev/docs/monty/limitations/): no third-party
packages, no `pip`, no class inheritance, no generators.

## Examples and CLI

| | |
|---|---|
| [`examples/chocolate`](./examples/chocolate) | The example above, with timings. |
| [`examples/tools`](./examples/tools) | Code mode with the `tools` package: Go functions become tools, their signatures become the type-checker stub and the model prompt, async tools overlap under `asyncio.gather`, and a wrong call is rejected before it runs. |
| [`examples/files`](./examples/files) | A real directory mounted read-write next to in-memory scratch space. |
| [`cmd/monty`](./cmd/monty) | `go install github.com/adrianliechti/go-monty/cmd/monty@latest` then `monty script.py`, `monty -e code`, `-mount /data=./dir[:ro|:overlay]`, `-env K=V`, `-typecheck -stubs tools.pyi`, `-timeout`, `-memory`, `-json`. |

## API

### Runtime and sessions

| | |
|---|---|
| `NewRuntime(ctx, opts...)` | Compiles the embedded module once per process. `WithCacheDir` keeps the compiled code on disk (4 s cold, 150 ms cached), `WithInterpreter` avoids native codegen, `WithMemoryLimit` walls each instance, `WithModule` swaps the wasm. |
| `rt.NewSession(ctx, SessionOptions)` | A Python REPL on its own wasm instance (~2 ms). Globals persist across runs. `Limits` bounds execution time, memory, recursion depth and host calls; `TypeCheck` with `TypeCheckStubs` rejects unsupported or mistyped code before it runs. |
| `rt.NewPool(ctx, PoolOptions)` | Warm instances so `pool.Checkout` returns a session in ~10 µs. Sessions return to the pool on `Close`; the pool recycles instances that aged or grew (wasm memory never shrinks). |
| `session.Run(ctx, code, RunOptions)` | Executes code, answering every host call from `RunOptions`, and returns the value of the last expression statement. |
| `session.Dump(ctx)` / `rt.Restore(ctx, state)` | Snapshot a session to bytes and restore it, in this or another process. Works between runs and while paused at a host call. |
| `session.Reset`, `session.ExecutionTime` | Reuse the instance with fresh state; read the cumulative interpreter time `MaxDuration` counts against. |

Errors from `Run` are typed: `*monty.Exception` when Python raised (with
`Type`, `Message`, a rendered `Traceback`, `Frames`, and `Data` for
`UnicodeDecodeError` and `JSONDecodeError`), `*monty.TypingError` when the
type checker rejected the code, and `*monty.WorkerError` when the sandbox
died (hard memory ceiling, wasm stack overflow, context deadline). A dead
session returns `monty.ErrClosed`; create a new one.

### What the code can reach (`RunOptions`)

| | |
|---|---|
| `Inputs` | Bound as globals before the code runs. |
| `Functions` | Go functions callable by name. Each call pauses the sandbox until it returns. Return `monty.Raise("ValueError", ...)` to raise a specific exception. |
| `AsyncFunctions` | Like `Functions`, but each call runs in its own goroutine while the sandbox continues. `await asyncio.gather(f(a), f(b))` runs them concurrently. |
| `Values` | Converted lazily when the code reads the name. May hold `*HostObject` and `*HostClass`. |
| `OS` | Filesystem, environment and clock requests; see below. |
| `Stdout`, `Stderr` | Receive `print()` output as it happens, not at the end of the run. |

The Go ↔ Python value mapping is documented in [`value.go`](./value.go):
`nil`/`None`, `int64`, `*big.Int`, `float64`, `string`, `[]byte`, `[]any`,
`map[string]any`, structs (as dicts, `json` tags honoured), `monty.Tuple`,
`monty.Set`, `monty.Dict`, `time.Time`, `time.Duration`, `monty.Date`, and so on.

### Tools from ordinary Go functions

The [`tools`](./tools) package binds and converts arguments by reflection and
derives the Python stub from the Go signature, so registering a tool is one
line and the checker gets the contract for free. Structs become `TypedDict`s,
pointer parameters are optional, a leading `context.Context` and a trailing
`error` are understood, and `AddAsync` tools get `async def` stubs.

```go
reg := tools.New()
reg.Add("list_customers", "Customers, optionally filtered by country.", listCustomers, "country")
reg.AddAsync("get_temperature", "Current temperature in °C.", getTemperature, "city")

session, _ := rt.NewSession(ctx, monty.SessionOptions{TypeCheck: true, TypeCheckStubs: reg.Stubs()})
result, err := session.Run(ctx, code, reg.Options(monty.RunOptions{}))
prompt := reg.Prompt() // the same declarations with docstrings, for the model
```

### Host objects

`monty.HostClass` and `monty.HostObject` put a Go-backed object, or class, in
front of the sandbox: `Attrs` cross eagerly, `Methods` and `ClassMethods` run
your Go code when called, `Lazy` serves attributes on read, and `Init` lets
the code construct instances. An object returned by the code comes back as
the same `*HostObject`. Host objects do not travel with a dump.

### Pausing a run: durable agent workflows

`session.Start` runs code until it completes or pauses at a host call and
returns a `Progress`. The `Suspension` tells you what the code wants (a
function call, a name, an OS request, or the results of deferred calls) and
you answer it whenever you like: `Return`, `Raise`, `Decline`, `Defer` (hand
back a future and settle it later), or `Resolve` from `RunOptions`. Or
`Dump` it, persist the bytes, and `Restore` in another process tomorrow,
after a slow tool has finished or a human has approved the action.

```go
p, _ := session.Start(ctx, "approve('deploy prod')", monty.StartOptions{})
state, _ := p.Suspension.Dump(ctx)            // persist while waiting for a human
// ... later, anywhere:
restored, _ := rt.Restore(ctx, state)
p, _ = restored.Pending().Return(ctx, "approved")
```

### Filesystem, environment and clock

Sandbox code sees no filesystem unless you mount one. `monty.FS` serves
`pathlib` and `open()` calls under a sandbox path from any `io/fs.FS`; writes
work when the `fs.FS` also implements the small optional interfaces in
[`fs.go`](./fs.go). Implementations shipped:

- `monty.DirFS` for a real host directory, backed by `os.Root` so `..` and symlinks cannot escape it.
- `monty.MemFS` for in-memory scratch space.
- `monty.NewOverlay(base)` for copy-on-write over any `fs.FS`: the base is never modified, `Upper()` and `Deleted()` expose what the code changed.
- `monty.ReadOnly(fsys)` to strip write capability; `FS.Quota` to cap bytes written.

```go
data, _ := monty.DirFS("./data")
_, err := session.Run(ctx, code, monty.RunOptions{
	OS: monty.Handlers(
		monty.NewFS("/data", data),
		monty.NewFS("/tmp", monty.NewMemFS(nil)),
		monty.NewFS("/repo", monty.NewOverlay(repoFS)),
		monty.Env(map[string]string{"LANG": "en"}),
		monty.Clock(time.Now),
	),
})
```

`Handlers` tries each handler in turn; anything outside every mount raises
`PermissionError`. Write your own `OSHandler` for anything else (see `OSCall`).

### Sharp edges

A context deadline on `Run` is a hard stop that kills the instance; prefer
`Limits.MaxDuration`, which raises `TimeoutError` inside the sandbox and keeps
the session usable. Keep `MaxRecursionDepth` near its default of 1000:
unwinding an exception through tens of thousands of frames is slow, and the
time limit is only checked between frames. If a host function panics, the
panic propagates to the caller after the feed has been aborted inside the
sandbox, so the session stays usable.

## How it works

```
Go ── protobuf frame ──▶ wazero ──▶ monty_dispatch()  (rust/src/lib.rs)
                                        │
                                   monty-proto Child   (upstream state machine)
                                        │
Go ◀── monty.event import ◀─────────────┘  each print and turn-ending event, as it happens
```

Monty ships a transport-agnostic protocol child (the same one behind
`monty subprocess` and the browser build) that consumes one protobuf request
and emits the events of that turn. `rust/` wraps it in a small core wasm
module with a C ABI; Go drives it over the
[`monty.v1` schema](./internal/pb/monty.proto) using length-prefixed frames,
the same wire format as the subprocess protocol, and receives events through
a single wasm import so output streams. Suspensions (host calls, name
lookups, OS calls, futures) come back as events and Go answers them.

Every session is its own wasm instance, so a crashed worker cannot touch
another session or the Go process. `Limits.MaxMemory` is enforced by Monty's
own allocator inside the module; the wasm linear memory is the hard wall
behind it.

## Building the wasm module

The module is checked in, so consumers only need Go. To rebuild it against a
different upstream commit:

```bash
rustup target add wasm32-wasip1
# edit the rev in rust/Cargo.toml and MONTY_REV in the Makefile
make fetch-proto generate   # refresh the schema and regenerate Go code
make build-wasm             # cargo build, copy to internal/wasm/monty.wasm
make test
```

The module is ~19 MB, most of it the type checker (ty) and its typeshed stubs.

## License

MIT. The embedded module is built from Monty, MIT licensed by Pydantic
Services Inc; see `internal/wasm/LICENSE.monty`.
