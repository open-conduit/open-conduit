GO ?= go
PYTHON ?= python3
CONFIG_PYTHON ?= .venv-contracts/bin/python
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD)
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build fmt fmt-check vet test boundaries config-check config-deps check cross-build smoke drill

build:
	$(GO) build ./...
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/open-conduit ./cmd/open-conduit
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/os ./cmd/os

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$(gofmt -l $$(git ls-files '*.go') $$(git ls-files --others --exclude-standard '*.go'))" || { gofmt -l $$(git ls-files '*.go') $$(git ls-files --others --exclude-standard '*.go'); exit 1; }

vet:
	$(GO) vet ./...
	$(GO) vet -tags faultinject ./internal/bootstrap ./cmd/open-conduit

test:
	$(GO) test -race ./...

boundaries:
	$(GO) run ./scripts/check-boundaries

# Explicit bootstrap; ordinary check never installs packages or uses the network.
config-deps:
	$(PYTHON) -m venv .venv-contracts
	.venv-contracts/bin/python -m pip install -r scripts/requirements-config-contract.txt

config-check:
	$(CONFIG_PYTHON) scripts/check-config-contract.py

check: fmt-check vet test boundaries config-check build smoke

smoke: build
	bin/open-conduit --help
	bin/open-conduit version
	bin/os --help
	bin/os version
	@bin/open-conduit run >/dev/null 2>&1; result=$$?; test $$result -eq 2
	@bin/os emit >/dev/null 2>&1; result=$$?; test $$result -eq 2

# Build evidence only; cross compilation is not runtime/platform verification.
cross-build:
	@set -eu; for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do \
		target_os=$${target%/*}; target_arch=$${target#*/}; \
		CGO_ENABLED=0 GOOS=$$target_os GOARCH=$$target_arch $(GO) build ./...; \
		mkdir -p dist/$$target_os-$$target_arch; \
		for binary in open-conduit os; do \
			CGO_ENABLED=0 GOOS=$$target_os GOARCH=$$target_arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$target_os-$$target_arch/$$binary ./cmd/$$binary; \
		done; \
	done

# Stage 02 gate evidence: real daemon kill/restart drills (also part of test).
drill:
	$(GO) test -race -count=1 -v -run 'TestDaemon|TestServeCommand|TestToolbeltTypeOnlyWait|TestDeclarationSurvivesRecovery' ./integration
