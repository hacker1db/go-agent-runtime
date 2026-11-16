.DEFAULT_GOAL := check
GO ?= go

.PHONY: check tidy fmt vet lint test vulncheck examples cover

check: tidy fmt vet lint test vulncheck examples ## Run everything CI runs

tidy: ## Fail if go.mod/go.sum are not tidy
	$(GO) mod tidy -diff

fmt: ## Fail if any file is not gofmt/goimports clean
	golangci-lint fmt --diff

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

test: ## Race-enabled, shuffled tests
	$(GO) test -race -shuffle=on -count=1 ./...

cover:
	$(GO) test -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

vulncheck:
	govulncheck ./...

examples: ## Every example must run with no configuration
	@set -e; for d in examples/*/; do echo "== $$d"; $(GO) run ./$$d >/dev/null; done
