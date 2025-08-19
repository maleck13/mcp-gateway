# Platform detection
OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
ARCH := $(shell uname -m | tr '[:upper:]' '[:lower:]')
ifeq ($(ARCH),x86_64)
    ARCH = amd64
endif
ifeq ($(ARCH),aarch64)
    ARCH = arm64
endif

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: build clean router broker all

# Build all binaries
all: router broker

# Build the router (ext-proc service)
router:
	go build -o bin/mcp-router ./cmd/mcp-router

# Build the broker (simple HTTP server)
broker:
	go build -o bin/mcp-broker ./cmd/mcp-broker

# Build both binaries
build: all

# Clean build artifacts
clean:
	rm -rf bin/

# Run the router
run-router: router
	./bin/mcp-router

# Run the broker
run-broker: broker
	./bin/mcp-broker

# Download dependencies
deps:
	go mod download

# Update dependencies
update:
	go mod tidy
	go get -u ./...

# Lint

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: golangci-lint
golangci-lint:
	golangci-lint run ./...

.PHONY: lint
lint: fmt vet golangci-lint

.PHONY: local-env-setup
local-env-setup: ## Setup local Kind cluster with Istio, Gateway API, and MetalLB
	$(MAKE) kind-delete-cluster
	$(MAKE) kind-create-cluster
	$(MAKE) gateway-api-install
	$(MAKE) istio-install
	$(MAKE) metallb-install
	$(MAKE) deploy-namespaces
	$(MAKE) deploy-gateway

.PHONY: local-env-teardown
local-env-teardown: ## Tear down the local Kind cluster
	$(MAKE) kind-delete-cluster

.PHONY: dev
dev: ## Setup cluster for local development (binaries run on host)
	$(MAKE) dev-setup
	@echo ""
	@echo "Ready for local development! Run these in separate terminals:"
	@echo "  1. make run-router"
	@echo "  2. make run-broker"
	@echo "  3. make dev-gateway-forward"
	@echo ""
	@echo "Then test with: make dev-test"

##@ Inspection

.PHONY: urls
urls: ## Show all available service URLs
	@$(MAKE) -s -f build/inspect.mk urls-impl

.PHONY: status
status: ## Show status of all MCP components
	@$(MAKE) -s -f build/inspect.mk status-impl

.PHONY: inspect-mock
inspect-mock: ## Open MCP Inspector for mock server
	@$(MAKE) -s -f build/inspect.mk inspect-mock-impl

-include build/*.mk
