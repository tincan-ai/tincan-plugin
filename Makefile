GO ?= go
.PHONY: build plugin test
build:
	python3 scripts/build-mls.py
	mkdir -p bin
	cp .tools/mls/wasm32-wasip1/release/tincan-mls.wasm bin/tincan-mls.wasm
	$(GO) build -trimpath -o bin/tincan ./cmd/tincan
plugin:
	python3 scripts/build-mls.py
	mkdir -p plugins/tincan/bin
	cp .tools/mls/wasm32-wasip1/release/tincan-mls.wasm plugins/tincan/bin/tincan-mls.wasm
	$(GO) build -trimpath -o plugins/tincan/bin/tincan ./cmd/tincan
test:
	$(GO) test -race ./...
	$(GO) vet ./...
	python3 -m unittest discover -s sdk/python -p 'test_*.py'

# Optional native adapter contract tests require Node.
.PHONY: test-adapters
test-adapters:
	python3 -m unittest discover -s sdk/python -p 'test_*.py'
	node --test plugins/tincan/native/openclaw/index.test.mjs
