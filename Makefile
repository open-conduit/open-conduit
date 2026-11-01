GO ?= go
PYTHON ?= python3
CONFIG_PYTHON ?= .venv-contracts/bin/python
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD)
LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(COMMIT)
# Per-package test timeout. The integration package (real daemon drills) takes
# about 8 minutes on hosted runners under -race when run in one process.
TEST_TIMEOUT ?= 20m
# test runs the integration package in this many processes at once
# (scripts/test-integration.sh). Its tests mostly wait on daemons, so the
# shards overlap well even on a 4-vCPU hosted runner.
INTEGRATION_SHARDS ?= 4

.PHONY: build fmt fmt-check vet test boundaries config-check config-deps docs-check container-test gateway-image check cross-build smoke smoke-run drill bench-history protocol-prototype packages certify release-snapshot ts-deps ts-check ts-packages

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
	$(GO) test -race -timeout $(TEST_TIMEOUT) $$($(GO) list ./... | grep -v '/integration$$')
	GO='$(GO)' TEST_TIMEOUT='$(TEST_TIMEOUT)' scripts/test-integration.sh $(INTEGRATION_SHARDS)

boundaries:
	$(GO) run ./scripts/check-boundaries

# Explicit bootstrap; ordinary check never installs packages or uses the network.
config-deps:
	$(PYTHON) -m venv .venv-contracts
	.venv-contracts/bin/python -m pip install -r scripts/requirements-config-contract.txt

config-check:
	$(CONFIG_PYTHON) scripts/check-config-contract.py

# The README, docs index, CONTRIBUTING and SECURITY: example configs pass
# offline validation (what config check runs), every open-conduit command
# shown is in the help and every make target shown is in this Makefile.
docs-check:
	$(GO) test -timeout $(TEST_TIMEOUT) -count=1 -run 'TestDocs' ./integration

# Real isolation tests for the Docker container provider, on the Docker
# context OPEN_CONDUIT_DOCKER_CONTEXT names (the CLI's current one when
# unset): the container CI job on Linux, and by hand on OrbStack or Docker
# Desktop. A PR that touches the provider posts this run's output.
#
# The tests build the gateway image themselves. CONTAINER_TEST_RUN narrows
# them, and OPEN_CONDUIT_CONTAINER_FORWARD=tcp forces the toolbelt forward
# macOS uses (the CI job's second pass).
CONTAINER_TEST_RUN ?= TestDocker
container-test:
	OPEN_CONDUIT_CONTAINER_TESTS=1 $(GO) test -count=1 -v -timeout $(TEST_TIMEOUT) -run '$(CONTAINER_TEST_RUN)' ./integration/ ./extensions/docker/...

# The gateway image for this machine's Docker (the context
# OPEN_CONDUIT_DOCKER_CONTEXT names, the CLI's current one when unset),
# from the gateway and os built for its architecture. It prints the image's
# ID, which OPEN_CONDUIT_GATEWAY_IMAGE takes in a build without a released
# gateway. A release builds and pushes both architectures
# (scripts/publish-gateway.sh) and pins the digest in the binary.
DOCKER_CONTEXT_FLAG = $(if $(OPEN_CONDUIT_DOCKER_CONTEXT),--context $(OPEN_CONDUIT_DOCKER_CONTEXT))
gateway-image:
	@set -eu; arch=$$(docker $(DOCKER_CONTEXT_FLAG) version --format '{{.Server.Arch}}'); \
		for binary in open-conduit-gateway os; do \
			CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/gateway/linux-$$arch/$$binary ./cmd/$$binary; \
		done; \
		docker $(DOCKER_CONTEXT_FLAG) build -q --build-arg TARGETARCH=$$arch -f extensions/docker/gateway/Dockerfile -t open-conduit-gateway:dev dist/gateway

check: fmt-check vet test boundaries config-check docs-check build smoke

smoke: build smoke-run

# The smoke tests over the binaries in SMOKE_BIN (release-snapshot points it
# at an extracted release archive).
SMOKE_BIN ?= bin
smoke-run:
	$(SMOKE_BIN)/open-conduit --help
	$(SMOKE_BIN)/open-conduit version
	$(SMOKE_BIN)/open-conduit --version
	$(SMOKE_BIN)/os --help
	$(SMOKE_BIN)/os version
	$(SMOKE_BIN)/os --version
	@$(SMOKE_BIN)/open-conduit run >/dev/null 2>&1; result=$$?; test $$result -eq 2
	@$(SMOKE_BIN)/os emit >/dev/null 2>&1; result=$$?; test $$result -eq 2

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
	$(GO) test -race -timeout $(TEST_TIMEOUT) -count=1 -v -run 'TestDaemon|TestServeCommand|TestToolbeltTypeOnlyWait|TestDeclarationSurvivesRecovery' ./integration

# #36 history growth: routing, the idle routing pass, Poll and the Work list
# against 1k, 10k and 50k retained events on real SQLite. Not part of check.
bench-history:
	$(GO) test -timeout $(TEST_TIMEOUT) -count=1 -run '^$$' -bench 'BenchmarkHistory' -benchtime 20x -benchmem ./integration

# 07-01 transport evidence: JSON-RPC over stdio versus go-plugin/gRPC on one
# workload. A separate module keeps go-plugin and gRPC out of go.mod; it
# downloads those modules on first run, so it is not part of check.
protocol-prototype:
	cd prototypes/transport && $(GO) test -timeout $(TEST_TIMEOUT) -count=1 -v ./...

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
	@# setup lives in github.com/open-conduit/extension-setup (go.mod tool)
	@# and is laid out as the release archive bundles it.
	GO='$(GO)' LDFLAGS='$(LDFLAGS)' scripts/bundle-packages.sh $(PACKAGES_DIR)/.bundled
	rm -rf $(PACKAGES_DIR)/setup && mv $(PACKAGES_DIR)/.bundled/*/extensions/setup $(PACKAGES_DIR)/setup && rm -rf $(PACKAGES_DIR)/.bundled

# The TypeScript SDK (sdk/typescript) and its example package
# (protocol/examples/weather). Installing their npm dependencies uses the
# network, so it is explicit, as config-deps is; ts-check typechecks and
# unit-tests both, and ts-packages bundles the example into the package
# layout certify runs (installing its dependencies first if ts-deps hasn't).
# ts-check needs Node 22.18 or later.
TS_DIRS = sdk/typescript protocol/examples/weather

ts-deps:
	@set -eu; for d in $(TS_DIRS); do (cd $$d && npm ci --no-audit --no-fund); done

ts-check:
	@set -eu; for d in $(TS_DIRS); do (cd $$d && npm run --silent typecheck && npm test --silent); done

ts-packages:
	cd protocol/examples/weather && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && node scripts/package.mjs $(abspath $(PACKAGES_DIR))/weather

# 07-03 certification: the public runner over the two first-party workloads
# and the examples, the TypeScript one included (reports beside the
# packages), then the negative controls, which must each fail their
# criterion.
certify: build packages ts-packages
	@set -eu; for name in slack shell notes runner setup weather; do \
		bin/open-conduit extension certify $(PACKAGES_DIR)/$$name --report $(PACKAGES_DIR)/$$name.certification.json; \
	done
	$(GO) test -timeout $(TEST_TIMEOUT) -count=1 -v -run 'TestCertification' ./integration

# 09-03 release packaging without publishing: GoReleaser --snapshot builds
# every platform's archive, the checksums and the Homebrew cask into
# dist/release (.goreleaser.yaml). Then this host's archive is extracted and
# smoke-tested, its binaries must report the snapshot version, and the cask
# must link only open-conduit and state no license. CI runs it on pull
# requests; the release workflow runs it before publishing.
GORELEASER ?= goreleaser
RELEASE_DIR = dist/release
release-snapshot:
	$(GORELEASER) release --snapshot --clean
	@set -eu; host=$$($(GO) env GOOS)_$$($(GO) env GOARCH); \
		test $$(ls $(RELEASE_DIR)/open-conduit_*.tar.gz | wc -l) -eq 4; \
		test $$(wc -l < $(RELEASE_DIR)/checksums.txt) -eq 4; \
		(cd $(RELEASE_DIR) && if command -v sha256sum >/dev/null 2>&1; then sha256sum -c checksums.txt; else shasum -a 256 -c checksums.txt; fi); \
		archive=$$(ls $(RELEASE_DIR)/open-conduit_*_$$host.tar.gz); \
		version=$$(basename $$archive .tar.gz); version=$${version#open-conduit_}; version=$${version%_$$host}; \
		rm -rf $(RELEASE_DIR)/smoke; mkdir -p $(RELEASE_DIR)/smoke; tar -xzf $$archive -C $(RELEASE_DIR)/smoke; \
		$(RELEASE_DIR)/smoke/open-conduit --version | grep -q "^open-conduit $$version "; \
		$(RELEASE_DIR)/smoke/os --version | grep -q "^os $$version "; \
		$(RELEASE_DIR)/smoke/open-conduit extension list | grep -q '^  setup .*(bundled with open-conduit)'; \
		cask=$(RELEASE_DIR)/homebrew/Casks/open-conduit.rb; \
		grep -q '^  binary "open-conduit"$$' $$cask; \
		test $$(grep -c '^  binary ' $$cask) -eq 1; \
		! grep -qi 'license' $$cask; \
		echo "snapshot $$version: 4 archives, checksums verified, setup bundled, cask links open-conduit only"
	$(MAKE) --no-print-directory smoke-run SMOKE_BIN=$(RELEASE_DIR)/smoke
