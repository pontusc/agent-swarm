#!/bin/bash
set -euo pipefail

# Script cleans up everything related to the local cluster

echo "💀  Killing registry proxy..."
if ! docker stop minikube-registry-proxy > /dev/null 2>&1; then
  echo "🔍  Did not find any registry proxy containers"
else
  docker rm minikube-registry-proxy > /dev/null
fi

minikube delete
