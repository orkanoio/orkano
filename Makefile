GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION   := v1.3.0
SQLC_VERSION          := v1.31.1
ENVTEST_K8S_VERSION   := 1.36.0
MODULES               := . api
BIN                   := $(CURDIR)/bin

# Host platform, normalized to the os/arch tokens the release artifacts use.
HOST_OS   := $(shell uname -s | tr '[:upper:]' '[:lower:]')
HOST_ARCH := $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')

# Pinned sha256 of the toolchain release artifacts (the .tar.gz the recipes
# curl) — the supply-chain guard for the two binaries Make downloads. Renovate
# does NOT bump these: when you bump a *_VERSION above, re-capture THAT tool's
# four platform digests. golangci-lint publishes golangci-lint-<ver>-checksums.txt;
# sqlc publishes no checksums.txt as of v1.31.1 — check its release page first,
# and if one still isn't there, hash each sqlc_<ver>_<os>_<arch>.tar.gz yourself.
GOLANGCI_LINT_SHA256_linux_amd64  := 8df580d2670fed8fa984aac0507099af8df275e665215f5c7a2ae3943893a553
GOLANGCI_LINT_SHA256_linux_arm64  := 44cd40a8c76c86755375adfeea52cfd3533cb43d7bd647771e0ae065e166df3a
GOLANGCI_LINT_SHA256_darwin_amd64 := f6f06d94b6241521c53d15450c5209b028270bf966f842afb11c030c79f5bc16
GOLANGCI_LINT_SHA256_darwin_arm64 := a9c54498731b3128f79e090be6110f3e5fffccc617b08142ed244d4126c73f29
SQLC_SHA256_linux_amd64  := 497ae4fcdfa64c5b0c311ffe4c2bd991e43991e82e5367792ed78bc2dca27354
SQLC_SHA256_linux_arm64  := b7cae247740d0c51a1e657479e5b2d21e6fef428f596682a01bc55bf4ab8a23d
SQLC_SHA256_darwin_amd64 := c5af76772e3785d21663a62697056b383f07629979b1bd25b93872e73dbd519b
SQLC_SHA256_darwin_arm64 := 21602158c99eb1f2bae197a66abfb1941d1e9e50b23125bb193349c6b1acc71e

.DEFAULT_GOAL := all
.PHONY: all lint test build vulncheck

all: lint test build

# Download the pinned release tarball and verify its sha256 before extracting —
# no `curl | sh` of an unpinned install.sh. (The official install.sh checksums
# the binary, but it was itself fetched from a mutable HEAD.)
$(BIN)/golangci-lint:
	@mkdir -p $(BIN)
	@want="$(GOLANGCI_LINT_SHA256_$(HOST_OS)_$(HOST_ARCH))"; \
	[ -n "$$want" ] || { echo "no pinned golangci-lint sha256 for $(HOST_OS)/$(HOST_ARCH)" >&2; exit 1; }; \
	ver=$(GOLANGCI_LINT_VERSION:v%=%); dir="golangci-lint-$${ver}-$(HOST_OS)-$(HOST_ARCH)"; \
	tmp=$$(mktemp -d) || { echo "mktemp failed" >&2; exit 1; }; trap 'rm -rf "$$tmp"' EXIT; \
	url="https://github.com/golangci/golangci-lint/releases/download/$(GOLANGCI_LINT_VERSION)/$${dir}.tar.gz"; \
	echo "downloading $$url"; curl -sSfL "$$url" -o "$$tmp/gcl.tar.gz"; \
	got=$$(if command -v sha256sum >/dev/null 2>&1; then sha256sum "$$tmp/gcl.tar.gz"; else shasum -a 256 "$$tmp/gcl.tar.gz"; fi | cut -d' ' -f1); \
	[ "$$got" = "$$want" ] || { echo "golangci-lint checksum mismatch: got $$got want $$want" >&2; exit 1; }; \
	tar -xzf "$$tmp/gcl.tar.gz" -C $(BIN) --strip-components=1 "$${dir}/golangci-lint"

lint: $(BIN)/golangci-lint
	@for m in $(MODULES); do \
		echo "lint $$m" && (cd $$m && $(BIN)/golangci-lint run ./...) || exit 1; \
	done

test:
	@KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" || exit 1; export KUBEBUILDER_ASSETS; \
	for m in $(MODULES); do \
		echo "test $$m" && (cd $$m && go test ./...) || exit 1; \
	done

build:
	@for m in $(MODULES); do \
		echo "build $$m" && (cd $$m && go build ./...) || exit 1; \
	done

vulncheck:
	@for m in $(MODULES); do \
		echo "vulncheck $$m" && (cd $$m && go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...) || exit 1; \
	done

.PHONY: web verify-web

# Build the dashboard SPA into dashboard/web/dist (gitignored, never
# committed). Binaries built with -tags webdist embed it (goreleaser passes
# the tag); a plain go build embeds the committed placeholder page instead,
# so nothing else in this Makefile needs Node.
# --ignore-scripts: a compromised npm package must not execute at install time
# (the release job holds a packages:write token and the cosign identity, AC-03);
# build-time execution is limited to code the Vite build actually imports.
web:
	cd dashboard/web && npm ci --ignore-scripts && npm run build

# web + prove the webdist-tagged embed compiles and vets. The tagged files are
# invisible to make lint/test (golangci-lint runs untagged — the same caveat
# as the imagepins tag), so CI runs this target as its own job.
verify-web: web
	go build -tags webdist ./dashboard/...
	go vet -tags webdist ./dashboard/...
	go test -tags webdist ./dashboard/web/

.PHONY: generate manifests validate-examples sqlc

$(BIN)/sqlc:
	@mkdir -p $(BIN)
	@want="$(SQLC_SHA256_$(HOST_OS)_$(HOST_ARCH))"; \
	[ -n "$$want" ] || { echo "no pinned sqlc sha256 for $(HOST_OS)/$(HOST_ARCH)" >&2; exit 1; }; \
	ver=$(SQLC_VERSION:v%=%); \
	tmp=$$(mktemp -d) || { echo "mktemp failed" >&2; exit 1; }; trap 'rm -rf "$$tmp"' EXIT; \
	url="https://github.com/sqlc-dev/sqlc/releases/download/$(SQLC_VERSION)/sqlc_$${ver}_$(HOST_OS)_$(HOST_ARCH).tar.gz"; \
	echo "downloading $$url"; curl -sSfL "$$url" -o "$$tmp/sqlc.tar.gz"; \
	got=$$(if command -v sha256sum >/dev/null 2>&1; then sha256sum "$$tmp/sqlc.tar.gz"; else shasum -a 256 "$$tmp/sqlc.tar.gz"; fi | cut -d' ' -f1); \
	[ "$$got" = "$$want" ] || { echo "sqlc checksum mismatch: got $$got want $$want" >&2; exit 1; }; \
	tar -xzf "$$tmp/sqlc.tar.gz" -C $(BIN) sqlc

sqlc: $(BIN)/sqlc
	cd internal/db && $(BIN)/sqlc generate

generate:
	go tool controller-gen object paths=./api/...

manifests:
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd

validate-examples: manifests
	hack/validate-examples.sh

.PHONY: verify-image-pins

# Assert every hand-pinned product image (buildjob.DefaultImage/StaticServerImage
# + the postgres catalog images) is a multi-arch index covering linux/amd64+arm64,
# not a single-platform manifest that would silently break the other arch. Needs
# docker buildx and hits the registry, so it is its own target/CI job — kept off
# the make all / make test path.
verify-image-pins:
	go test -tags imagepins ./operator/internal/imagepins/ -run TestProductImagePinsAreMultiArch -count=1 -v

.PHONY: local-loop

# Event-path inner loop: kind + Postgres + receiver + operator (stubbed GitHub);
# one signed push must produce a Build CR (hack/local-loop/run.sh). KEEP=1 leaves
# it up to fire more events; CLEAN=1 also deletes the kind cluster on exit.
local-loop:
	hack/local-loop/run.sh
