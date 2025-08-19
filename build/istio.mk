##@ Istio

SAIL_VERSION = 0.1.0
ISTIO_NAMESPACE = istio-system

.PHONY: istio-install
istio-install: $(HELM) ## Install Istio using Sail operator
	$(HELM) install sail-operator \
		--create-namespace \
		--namespace $(ISTIO_NAMESPACE) \
		--wait \
		--timeout=300s \
		https://github.com/istio-ecosystem/sail-operator/releases/download/$(SAIL_VERSION)/sail-operator-$(SAIL_VERSION).tgz
	kubectl apply -f config/istio/istio.yaml
	kubectl -n $(ISTIO_NAMESPACE) wait --for=condition=Ready istio/default --timeout=300s

.PHONY: istio-uninstall
istio-uninstall: $(HELM) ## Uninstall Istio and Sail operator
	- kubectl delete -f config/istio/istio.yaml
	$(HELM) uninstall sail-operator -n $(ISTIO_NAMESPACE)
	- kubectl delete namespace $(ISTIO_NAMESPACE)