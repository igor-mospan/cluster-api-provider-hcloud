#!/bin/bash

set -euo pipefail

set -x

kind create cluster --name capi-hetzner || true

# Install cert-manager
kubectl create namespace cert-manager --dry-run -o yaml | kubectl apply -f -
kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v0.13.1/cert-manager.yaml

# Install cluster api components
kubectl apply \
  -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v0.3.0-rc.3/core-components.yaml \
  -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v0.3.0-rc.3/cluster-api-components.yaml \
  -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v0.3.0-rc.3/control-plane-components.yaml \
  -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v0.3.0-rc.3/bootstrap-components.yaml
