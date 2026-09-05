// Package monty runs Python code written by a model in a sandbox, with no
// cgo and no native libraries: the upstream Monty interpreter
// (https://github.com/pydantic/monty) is compiled to WebAssembly, embedded in
// this package, and executed with wazero.
//
// Sandboxed code has no filesystem, environment or network. It reaches the
// host only through the Go functions and the optional OSHandler you pass to
// Session.Run, and it is bounded by the memory, time and recursion limits in
// SessionOptions.
//
//	rt, _ := monty.NewRuntime(ctx)
//	defer rt.Close(ctx)
//
//	s, _ := rt.NewSession(ctx, monty.SessionOptions{})
//	defer s.Close(ctx)
//
//	out, err := s.Run(ctx, "kcal * 2", monty.RunOptions{
//	    Inputs: map[string]any{"kcal": 230},
//	})
package monty
