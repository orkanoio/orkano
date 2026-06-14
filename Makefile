GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION   := v1.3.0
SQLC_VERSION          := v1.31.1
ENVTEST_K8S_VERSION   := 1.36.0
MODULES               := . api
BIN                   := $(CURDIR)/bin

.DEFAULT_GOAL := all
.PHONY: all lint test build vulncheck

all: lint test build

$(BIN)/golangci-lint:
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
		| sh -s -- -b $(BIN) $(GOLANGCI_LINT_VERSION)

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

.PHONY: generate manifests validate-examples sqlc

$(BIN)/sqlc:
	@mkdir -p $(BIN)
	@os=$$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$$(uname -m); \
	case $$arch in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; esac; \
	ver=$(SQLC_VERSION:v%=%); \
	url="https://github.com/sqlc-dev/sqlc/releases/download/$(SQLC_VERSION)/sqlc_$${ver}_$${os}_$${arch}.tar.gz"; \
	echo "downloading $$url" && curl -sSfL "$$url" | tar -xz -C $(BIN) sqlc

sqlc: $(BIN)/sqlc
	cd internal/db && $(BIN)/sqlc generate

generate:
	go tool controller-gen object paths=./api/...

manifests:
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd

validate-examples: manifests
	hack/validate-examples.sh
