#!/bin/bash

# build-local.sh
# Build OpenBao locally (UI + Binary) and create a local Docker image.
# Usage: ./scripts/build-local.sh [tag]

set -e

TAG=${1:-latest}
IMAGE_NAME="ciao-local"
FULL_IMAGE="${IMAGE_NAME}:${TAG}"

echo "----------------------------------------------------"
echo " OpenBao Local Builder"
echo "----------------------------------------------------"
echo "Target Image: $FULL_IMAGE"
echo "----------------------------------------------------"

# --- Prerequisites ---
for cmd in docker make go; do
    if ! command -v $cmd &> /dev/null; then
        echo "Error: $cmd is not installed."
        exit 1
    fi
done

# --- Build Process ---
echo "==> Phase 1: Building UI assets (using Docker to ensure Node 20 compatibility)..."
docker run --rm -v "$(pwd):/src" -w /src/ui -e CI=true node:20-alpine sh -c "corepack enable && pnpm install && pnpm build && chown -R $(id -u):$(id -g) /src"

echo "==> Phase 2: Building OpenBao binary..."
make bin

if [[ ! -f "bin/bao" ]]; then
    echo "Error: Build failed, bin/bao not found."
    exit 1
fi

echo "==> Phase 3: Building local Docker image..."
docker build --build-arg BIN_NAME=bin/bao -t "$FULL_IMAGE" .

echo "----------------------------------------------------"
echo "SUCCESS!"
echo "Local image created: $FULL_IMAGE"
echo "----------------------------------------------------"
