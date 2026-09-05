# Upstream Monty commit the wasm module and the protobuf schema are built from.
# Keep in sync with rust/Cargo.toml.
MONTY_REV := 302e0f27ec8516689ba90793cd0381149e5f4076
MONTY_URL := https://raw.githubusercontent.com/pydantic/monty/$(MONTY_REV)

GO_PACKAGE := github.com/adrianliechti/go-monty

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: build-wasm
build-wasm: ## Build internal/wasm/monty.wasm from rust/ (needs rustup target wasm32-wasip1)
	cd rust && cargo build --release --target wasm32-wasip1
	cp rust/target/wasm32-wasip1/release/monty_go_wasm.wasm internal/wasm/monty.wasm
	@ls -la internal/wasm/monty.wasm

.PHONY: fetch-proto
fetch-proto: ## Refresh internal/pb/monty.proto from the pinned upstream commit
	curl -fsSL $(MONTY_URL)/crates/monty-proto/proto/monty/v1/monty.proto -o internal/pb/monty.proto
	curl -fsSL $(MONTY_URL)/LICENSE -o internal/wasm/LICENSE.monty

.PHONY: generate
generate: ## Regenerate internal/pb/monty.pb.go (needs protoc and protoc-gen-go)
	protoc -I internal/pb \
		--go_out=internal/pb --go_opt=paths=source_relative \
		--go_opt=Mmonty.proto=$(GO_PACKAGE)/internal/pb \
		internal/pb/monty.proto

.PHONY: test
test: ## Run the Go tests
	CGO_ENABLED=0 go test ./...

.PHONY: bench
bench: ## Run the benchmarks
	CGO_ENABLED=0 go test -run '^$$' -bench . -benchmem ./

.PHONY: lint
lint: ## gofmt and go vet
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...

.PHONY: cli
cli: ## Build the monty CLI into ./bin
	CGO_ENABLED=0 go build -o bin/monty ./cmd/monty

.PHONY: example
example: ## Run the example programs
	CGO_ENABLED=0 go run ./examples/chocolate
	CGO_ENABLED=0 go run ./examples/tools
	CGO_ENABLED=0 go run ./examples/files
