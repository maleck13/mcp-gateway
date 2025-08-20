.PHONY: kuadrant-install
kuadrant-install: $(HELM) 
	$(HELM) install \
 kuadrant-operator kuadrant/kuadrant-operator \
 --create-namespace \
 --wait \
 --timeout=600s \
 --namespace kuadrant-system

.PHONY: kuadrant-remove
kuadrant-uninstall: $(HELM) 
	$(HELM) uninstall \
 kuadrant-operator \
 --namespace kuadrant-system 

.PHONY: kuadrant-configure
kuadrant-configure:
	$(KUSTOMIZE) build config/kuadrant | kubectl apply -f -