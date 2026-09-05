// Package wasm embeds the Monty worker compiled to WebAssembly.
//
// monty.wasm is built from ../../rust (see the Makefile target build-wasm). It
// contains the upstream Monty interpreter, whose license is in LICENSE.monty.
package wasm

import _ "embed"

// Module is the core wasm module driving one Monty protocol child.
//
//go:embed monty.wasm
var Module []byte
