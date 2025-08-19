##@ Additional Cluster Commands

.PHONY: cluster-setup
cluster-setup: ## Install just the cluster dependencies (without creating Kind cluster)
	$(MAKE) gateway-api-install
	$(MAKE) istio-install
	$(MAKE) metallb-install