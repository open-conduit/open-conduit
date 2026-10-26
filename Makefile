GO ?= go
PYTHON ?= python3
CONFIG_PYTHON ?= .venv-contracts/bin/python
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD)
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build fmt fmt-check vet test boundaries config-check config-deps docs-check check cross-build smoke drill protocol-prototype packages certify

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

# The README and docs index: example configs pass offline validation (what
# config check runs) and every open-conduit command shown is in the help.
docs-check:
	$(GO) test -count=1 -run 'TestDocs' ./integration

check: fmt-check vet test boundaries config-check docs-check build smoke

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

# 07-01 transport evidence: JSON-RPC over stdio versus go-plugin/gRPC on one
# workload. A separate module keeps go-plugin and gRPC out of go.mod; it
# downloads those modules on first run, so it is not part of check.
protocol-prototype:
	cd prototypes/transport && $(GO) test -count=1 -v ./...

# 07-03 extension packages: each first-party subprocess package and example
# as an installable directory (manifest, binary, certification fixture).
PACKAGES_DIR ?= dist/packages
EXTENSION_PACKAGES = slack:./packages/slack/cmd/slack-extension shell:./packages/shell/cmd/shell-extension notes:./protocol/examples/notes/cmd/notes-extension runner:./protocol/examples/runner/cmd/runner-extension

packages:
	@set -eu; for spec in $(EXTENSION_PACKAGES); do \
		name=$${spec%%:*}; pkg=$${spec#*:}; dir=$(PACKAGES_DIR)/$$name; \
		rm -rf $$dir; mkdir -p $$dir; \
		$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/$$name-extension $$pkg; \
		$$dir/$$name-extension package-manifest > $$dir/open-conduit-extension.json; \
		$$dir/$$name-extension package-certification > $$dir/certification.json; \
		echo "packaged $$dir"; \
	done
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(PACKAGES_DIR)/shell/os ./cmd/os

# 07-03 certification: the public runner over the two first-party workloads
# and the examples (reports beside the packages), then the negative
# controls, which must each fail their criterion.
certify: build packages
	@set -eu; for name in slack shell notes runner; do \
		bin/open-conduit extension certify $(PACKAGES_DIR)/$$name --report $(PACKAGES_DIR)/$$name.certification.json; \
	done
	$(GO) test -count=1 -v -run 'TestCertification' ./integration
