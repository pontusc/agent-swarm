.PHONY: setup start-minikube stop-minikube
# ---
setup: start-minikube

# --- Minikube related
start-minikube:
	@./scripts/minikube/start-minikube.sh

stop-minikube:
	@./scripts/minikube/stop-minikube.sh
