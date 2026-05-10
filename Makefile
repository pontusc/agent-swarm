# --- Variables
MINIKUBE_REGISTRY ?= localhost:5000
OPERATOR_IMAGE_REPO ?= agent-swarm/operator
OPERATOR_IMAGE_TAG := latest
OPERATOR_IMG ?= $(MINIKUBE_REGISTRY)/$(OPERATOR_IMAGE_REPO):$(OPERATOR_IMAGE_TAG)
OPERATOR_NAMESPACE ?= agent-swarm-system
OPERATOR_DEPLOYMENT ?= agent-swarm-controller-manager

.PHONY: setup cluster-setup cluster-clean start-minikube stop-minikube build-and-push redeploy deploy-operator apply-github-secret apply-samples

# --- Deploy
setup: build-and-push apply-github-secret deploy-operator apply-samples

build-and-push:
	@$(MAKE) -C operator docker-build IMG=$(OPERATOR_IMG)
	@docker push $(OPERATOR_IMG)

redeploy: build-and-push
	@kubectl -n $(OPERATOR_NAMESPACE) rollout restart deployment/$(OPERATOR_DEPLOYMENT)
	@kubectl -n $(OPERATOR_NAMESPACE) rollout status deployment/$(OPERATOR_DEPLOYMENT) --timeout=180s

deploy-operator:
	@$(MAKE) -C operator deploy IMG=$(OPERATOR_IMG)
	@kubectl wait --for=condition=Established crd/repositories.agentswarm.dev --timeout=60s
	@kubectl wait --for=condition=Established crd/issues.agentswarm.dev --timeout=60s
	@kubectl -n $(OPERATOR_NAMESPACE) rollout status deployment/$(OPERATOR_DEPLOYMENT) --timeout=180s

apply-github-secret:
	@kubectl apply -f .secrets/github-app.yml

apply-samples:
	@kubectl apply -k operator/config/samples

cluster-setup: start-minikube setup

cluster-clean:
	@kubectl delete -k operator/config/samples --ignore-not-found=true || true
	@kubectl delete -f .secrets/github-app.yml --ignore-not-found=true || true
	@$(MAKE) -C operator undeploy ignore-not-found=true
	@$(MAKE) -C operator uninstall ignore-not-found=true

# --- Minikube related
start-minikube:
	@./scripts/minikube/start-minikube.sh

stop-minikube:
	@./scripts/minikube/stop-minikube.sh
