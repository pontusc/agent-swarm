.PHONY: setup start-minikube stop-minikube
# ---
setup: start-minikube
	@kubectl apply -f .secrets/github-app.yml
	@$(MAKE) -C operator install
	@kubectl apply -k operator/config/samples

# --- Minikube related
start-minikube:
	@./scripts/minikube/start-minikube.sh

stop-minikube:
	@./scripts/minikube/stop-minikube.sh
