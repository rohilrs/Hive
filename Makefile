GO          ?= go
INSTALL_DIR ?= $(HOME)/bin
SMOKE_DIR   ?= scripts/smoke

.PHONY: build install test test-short smoke fmt vet tidy doctor clean help

help:    ## Show this help (default target)
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-12s %s\n", $$1, $$2}'

build:   ## Build the hive binary to ./hive
	$(GO) build -o hive ./cmd/hive

install: build  ## Install hive to $(INSTALL_DIR) (default ~/bin)
	@mkdir -p $(INSTALL_DIR)
	install -m 0755 hive $(INSTALL_DIR)/hive
	@echo "Installed $(INSTALL_DIR)/hive"

test:    ## Run the full test suite
	$(GO) test ./... -count=1 -timeout 180s

test-short:  ## Skip long tests (adapter, daemon, scavenger)
	$(GO) test ./... -count=1 -short -timeout 60s

smoke:   ## Smoke-test against a real daemon (requires claude on PATH)
	bash $(SMOKE_DIR)/preflight.sh
	bash $(SMOKE_DIR)/start-daemon.sh
	bash $(SMOKE_DIR)/teardown.sh

fmt:     ## gofmt the codebase
	$(GO) fmt ./...

vet:     ## go vet
	$(GO) vet ./...

tidy:    ## go mod tidy
	$(GO) mod tidy

doctor:  ## Run hive doctor against the local daemon (requires install)
	$(INSTALL_DIR)/hive doctor

clean:   ## Remove build artifacts (NOT ~/.hive)
	rm -f hive
	@echo "Removed ./hive; ~/.hive/ untouched (use 'rm -rf ~/.hive' to nuke state)."

.DEFAULT_GOAL := help
