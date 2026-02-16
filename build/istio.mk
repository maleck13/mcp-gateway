# Istio

SAIL_VERSION = 1.27.0
ISTIO_NAMESPACE = istio-system
ISTIO_VERSION = 1.27.0

# istioctl tool
ISTIOCTL = bin/istioctl
$(ISTIOCTL):
	mkdir -p bin
	curl -sL https://istio.io/downloadIstio | ISTIO_VERSION=$(ISTIO_VERSION) TARGET_ARCH=$(ARCH) sh -
	mv istio-$(ISTIO_VERSION)/bin/istioctl bin/
	rm -rf istio-$(ISTIO_VERSION)

istioctl-impl: $(ISTIOCTL)
	@echo "istioctl installed at: $(ISTIOCTL)"
	@echo "Version: $$($(ISTIOCTL) version --remote=false)"

.PHONY: istioctl-latest
istioctl-latest: ## Download latest istioctl to bin/
	@echo "Downloading latest istioctl..."
	@mkdir -p bin
	@curl -sL https://istio.io/downloadIstio | TARGET_ARCH=$(ARCH) sh -
	@ISTIO_DIR=$$(ls -d istio-* 2>/dev/null | head -1); \
	if [ -n "$$ISTIO_DIR" ]; then \
		mv $$ISTIO_DIR/bin/istioctl bin/; \
		rm -rf $$ISTIO_DIR; \
		echo "istioctl installed at: bin/istioctl"; \
		bin/istioctl version --remote=false; \
	else \
		echo "Error: Failed to download istioctl"; \
		exit 1; \
	fi

.PHONY: istio-install-istioctl
istio-install-istioctl: $(ISTIOCTL) ## Install Istio using istioctl (minimal profile)
	@echo "Installing Istio using istioctl..."
	$(ISTIOCTL) install --set profile=minimal --set meshConfig.accessLogFile=/dev/stdout -y
	@echo "Waiting for Istio to be ready..."
	kubectl -n $(ISTIO_NAMESPACE) wait --for=condition=Available deployment/istiod --timeout=300s
	@echo "Istio installed successfully"

.PHONY: istio-install-istioctl-latest
istio-install-istioctl-latest: istioctl-latest ## Install latest Istio using istioctl
	@echo "Installing Istio using istioctl..."
	bin/istioctl install --set profile=minimal --set meshConfig.accessLogFile=/dev/stdout -y
	@echo "Waiting for Istio to be ready..."
	kubectl -n $(ISTIO_NAMESPACE) wait --for=condition=Available deployment/istiod --timeout=300s
	@echo "Istio installed successfully"

.PHONY: istio-install
istio-install: $(HELM) # Install Istio using Sail operator
	$(HELM) upgrade --install sail-operator \
		--create-namespace \
		--namespace $(ISTIO_NAMESPACE) \
		--wait \
		--timeout=300s \
		https://github.com/istio-ecosystem/sail-operator/releases/download/$(SAIL_VERSION)/sail-operator-$(SAIL_VERSION).tgz
	kubectl apply -f config/istio/istio.yaml
	kubectl -n $(ISTIO_NAMESPACE) wait --for=condition=Ready istio/default --timeout=300s

.PHONY: istio-uninstall
istio-uninstall: $(HELM) # Uninstall Istio and Sail operator
	- kubectl delete -f config/istio/istio.yaml
	$(HELM) uninstall sail-operator -n $(ISTIO_NAMESPACE)
	- kubectl delete namespace $(ISTIO_NAMESPACE)
