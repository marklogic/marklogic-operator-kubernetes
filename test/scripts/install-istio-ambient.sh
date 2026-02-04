#!/bin/bash
# Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

set -e

echo "===== Installing ISTIO with Ambient Mode ====="

ISTIO_VERSION="${ISTIO_VERSION:-1.24.1}"

# Download and extract istioctl if not already available
if ! command -v istioctl &> /dev/null; then
    echo "Downloading istioctl version ${ISTIO_VERSION}..."
    curl -L "https://istio.io/downloadIstio" | ISTIO_VERSION=${ISTIO_VERSION} sh -
    export PATH="$PWD/istio-${ISTIO_VERSION}/bin:$PATH"
fi

echo "Installing ISTIO base components..."
istioctl install --set profile=ambient --skip-confirmation

echo "Verifying ISTIO installation..."
kubectl wait --for=condition=ready pod -l app=istiod -n istio-system --timeout=300s
kubectl wait --for=condition=ready pod -l app=ztunnel -n istio-system --timeout=300s

echo "===== ISTIO Ambient Mode installed successfully ====="
