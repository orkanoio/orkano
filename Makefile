GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION   := v1.3.0
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
	@for m in $(MODULES); do \
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
