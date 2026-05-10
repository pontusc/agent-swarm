#!/bin/bash
set -euo pipefail

if ! command -v minikube > /dev/null 2>&1; then
  cat << EOF
Minikube not installed!
EOF
  exit 1
fi

PROFILE="${OVERRIDE_PROFILE:=agent-swarm}"

if minikube status --profile="$PROFILE" > /dev/null; then
  echo "🛑  Cluster '$PROFILE' already running"
  exit 0
fi

ADDONS="${OVERRIDE_ADDONS:=registry}"

minikube start \
  --profile="$PROFILE"

# -- Make sure profile (context for minikube cli) is updated
minikube profile "$PROFILE"

for ADDON in $ADDONS; do
  minikube addons enable "$ADDON"
done

# -- Start image registry proxy
echo "🏃  Starting registry proxy"
docker run -d --name minikube-registry-proxy --network=host alpine ash -c "apk add socat && socat TCP-LISTEN:5000,reuseaddr,fork TCP:$(minikube ip):5000" > /dev/null

echo "🏓  Ready!"
