GO ?= go
PYTHON ?= python3
export PYTHONDONTWRITEBYTECODE := 1

.PHONY: help build release test test-smux race vet fmt fmt-check docs-check tooling-test check uplink-smoke

help:
	@echo 'build         Build the client and server for the current platform in build/'
	@echo 'release       Build static Linux amd64/arm64 artifacts in dist/latest/'
	@echo 'check         Check formatting/docs/tooling, test both modules, and run vet'
	@echo 'race          Run root and patched smux tests under the race detector'
	@echo 'uplink-smoke  Run the static production-stack simulation control'

build:
	mkdir -p build
	$(GO) build -trimpath -buildvcs=false -o build/dnstt-turbo-client ./dnstt-client
	$(GO) build -trimpath -buildvcs=false -o build/dnstt-turbo-server ./dnstt-server

release:
	GO_BIN="$(GO)" $(PYTHON) scripts/build-release.py --output dist/latest --replace

test:
	$(GO) test ./... -count=1 -timeout=180s
	$(MAKE) test-smux

test-smux:
	cd third_party/smux && $(GO) test ./... -count=1 -timeout=180s

race:
	$(GO) test -race ./... -count=1 -timeout=240s
	cd third_party/smux && $(GO) test -race ./... -count=1 -timeout=240s

vet:
	$(GO) vet ./...
	cd third_party/smux && $(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	$(PYTHON) scripts/check-format.py

docs-check:
	$(PYTHON) scripts/check-docs.py

tooling-test:
	$(PYTHON) -m unittest discover -s scripts/tests -p 'test_*.py'

check: fmt-check docs-check tooling-test test vet

uplink-smoke:
	GO_BIN="$(GO)" $(PYTHON) tests/uplink/run.py --mode performance --protocol-only --case regression-static
