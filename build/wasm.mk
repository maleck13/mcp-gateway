# Wasm build targets

WASM_ROUTER_DIR := cmd/wasm-router
WASM_OUTPUT := bin/mcp-routing.wasm
WASM_IMAGE := ghcr.io/kuadrant/mcp-gateway/mcp-routing-wasm:latest
DEFAULT_WORKLOAD ?= gateway.io/name=mcp-gateway

##@ Wasm

.PHONY: wasm-router
wasm-router: ## Build the Wasm routing filter for Envoy (requires Go 1.24+)
	@echo "Building wasm-router with Go (wasip1/wasm)..."
	@mkdir -p bin
	cd $(WASM_ROUTER_DIR) && \
		go mod tidy && \
		GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o ../../$(WASM_OUTPUT) .
	@echo "Built $(WASM_OUTPUT)"

.PHONY: wasm-router-image
wasm-router-image: wasm-router ## Build OCI image containing the Wasm binary
	@echo "Building Wasm OCI image..."
	cp $(WASM_OUTPUT) $(WASM_ROUTER_DIR)/mcp-routing.wasm
	$(CONTAINER_ENGINE) build $(CONTAINER_ENGINE_EXTRA_FLAGS) \
		-t $(WASM_IMAGE) \
		$(WASM_ROUTER_DIR)
	rm -f $(WASM_ROUTER_DIR)/mcp-routing.wasm
	@echo "Built $(WASM_IMAGE)"

.PHONY: wasm-router-load
wasm-router-load: kind wasm-router-image ## Load Wasm OCI image into Kind cluster
	@echo "Loading Wasm image into Kind cluster..."
	$(call load-image,$(WASM_IMAGE))
	@echo "Loaded $(WASM_IMAGE) into Kind"

.PHONY: wasm-router-configure-controller
wasm-router-configure-controller: ## Configure MCPGatewayExtension to disable router and remove EnvoyFilter
	@echo "Patching MCPGatewayExtension to disable router..."
	kubectl patch mcpgatewayextension mcp-gateway-extension -n mcp-system --type=merge -p '{"metadata":{"annotations":{"kuadrant.io/disable-router":"true"}}}'
	@echo "Waiting for controller to reconcile..."
	@sleep 5

.PHONY: wasm-router-remove-extproc
wasm-router-remove-extproc: ## Remove ext_proc EnvoyFilter to prepare for WASM
	@echo "Removing ext_proc EnvoyFilter..."
	kubectl delete envoyfilter -n gateway-system -l app.kubernetes.io/managed-by=mcp-gateway-controller --ignore-not-found

.PHONY: wasm-router-deploy
wasm-router-deploy: debug-wasm wasm-router-load wasm-router-configure-controller wasm-router-remove-extproc ## Deploy Wasm router (patch gateway + EnvoyFilter)
	@echo "Patching gateway deployment with Wasm init container..."
	@GATEWAY_DEPLOY=$$(kubectl get deploy -n gateway-system -l $(DEFAULT_WORKLOAD) -o jsonpath='{.items[0].metadata.name}' 2>/dev/null); \
	if [ -n "$$GATEWAY_DEPLOY" ]; then \
		kubectl patch deployment $$GATEWAY_DEPLOY -n gateway-system --patch-file config/wasm/gateway-patch.yaml; \
		echo "Waiting for gateway rollout..."; \
		kubectl rollout status deployment/$$GATEWAY_DEPLOY -n gateway-system --timeout=120s; \
	else \
		echo "Warning: No gateway deployment found, skipping patch"; \
	fi
	@echo "Deploying Wasm router EnvoyFilter..."
	kubectl apply -k config/wasm/
	@echo "Wasm router deployed"
	kubectl rollout restart deployment -n gateway-system -l gateway.io/name=mcp-gateway

.PHONY: wasm-router-undeploy
wasm-router-undeploy: ## Remove Wasm router and restore ext_proc mode
	@echo "Removing Wasm router EnvoyFilter..."
	kubectl delete -k config/wasm/ --ignore-not-found
	@echo "Restoring MCPGatewayExtension annotations..."
	kubectl patch mcpgatewayextension mcp-gateway-extension -n mcp-system --type=json -p '[{"op":"remove","path":"/metadata/annotations/kuadrant.io~1disable-router"}]' 2>/dev/null || true
	@echo "Wasm router removed. Restart gateway to remove init container: kubectl rollout restart deployment -n gateway-system -l gateway.io/name=mcp-gateway"
	kubectl rollout restart deployment -n gateway-system -l gateway.io/name=mcp-gateway

.PHONY: wasm-router-clean
wasm-router-clean: ## Clean wasm build artifacts
	rm -f $(WASM_OUTPUT)
	rm -f $(WASM_ROUTER_DIR)/mcp-routing.wasm

.PHONY: wasm-router-deps
wasm-router-deps: ## Download wasm-router dependencies
	cd $(WASM_ROUTER_DIR) && go mod download

.PHONY: wasm-router-tidy
wasm-router-tidy: ## Tidy wasm-router dependencies
	cd $(WASM_ROUTER_DIR) && go mod tidy

.PHONY: debug-wasm
debug-wasm: ## Enable WASM debug logging in Envoy gateway
	@echo "Enabling WASM debug logging..."
	@PODS=$$(kubectl get pods -n gateway-system -l gateway.istio.io/managed=istio.io-gateway-controller -o jsonpath='{range .items[*]}{.metadata.name} {end}' 2>/dev/null); \
	if [ -z "$$PODS" ]; then \
		echo "Error: No gateway pods found in gateway-system"; \
		exit 1; \
	fi; \
	for POD in $$PODS; do \
		echo "Enabling WASM debug on pod: $$POD"; \
		kubectl exec -n gateway-system $$POD -- curl -s -X POST "http://localhost:15000/logging?wasm=debug" > /dev/null; \
	done
	@echo "WASM debug logging enabled. Use 'make debug-wasm-off' to disable."

.PHONY: debug-wasm-off
debug-wasm-off: ## Disable WASM debug logging in Envoy gateway
	@echo "Disabling WASM debug logging..."
	@PODS=$$(kubectl get pods -n gateway-system -l gateway.istio.io/managed=istio.io-gateway-controller -o jsonpath='{range .items[*]}{.metadata.name} {end}' 2>/dev/null); \
	if [ -z "$$PODS" ]; then \
		echo "Error: No gateway pods found in gateway-system"; \
		exit 1; \
	fi; \
	for POD in $$PODS; do \
		echo "Disabling WASM debug on pod: $$POD"; \
		kubectl exec -n gateway-system $$POD -- curl -s -X POST "http://localhost:15000/logging?wasm=info" > /dev/null; \
	done
	@echo "WASM debug logging disabled."

.PHONY: local-env-setup-wasm
local-env-setup-wasm: ## Setup local environment with WASM router using latest Istio (istioctl)
	@echo "========================================="
	@echo "Starting MCP Gateway WASM Environment"
	@echo "========================================="
	"$(MAKE)" tools
	"$(MAKE)" kind-create-cluster
	"$(MAKE)" build-and-load-image
	"$(MAKE)" gateway-api-install
	"$(MAKE)" istio-install-istioctl-latest
	"$(MAKE)" metallb-install
	"$(MAKE)" deploy-namespaces
	"$(MAKE)" deploy-gateway
	"$(MAKE)" deploy
	"$(MAKE)" add-jwt-key
	"$(MAKE)" deploy-test-servers
	"$(MAKE)" deploy-example
	@echo ""
	@echo "========================================="
	@echo "Deploying WASM Router"
	@echo "========================================="
	"$(MAKE)" wasm-router-deploy
	@echo ""
	@echo "========================================="
	@echo "WASM Environment Ready"
	@echo "========================================="
	@echo "The gateway is now using WASM-based routing."
	@echo "MCPGatewayExtension has annotation: kuadrant.io/disable-router=true"
	@echo "Istio installed via istioctl (latest version)"
