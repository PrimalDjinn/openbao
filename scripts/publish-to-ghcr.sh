#!/bin/bash

# publish-to-ghcr.sh
# Build OpenBao locally and publish the Docker image to GitHub Container Registry (GHCR).

set -e

# --- Configuration ---
# Try to detect the repository owner from git remote
GIT_URL=$(git remote get-url origin 2>/dev/null || echo "")
if [[ -z "$GIT_URL" ]]; then
    echo "Error: Not a git repository or no origin remote found."
    exit 1
fi

# Extract owner and repo name from SSH or HTTPS URLs and lowercase them
REPO_OWNER=$(echo "$GIT_URL" | sed -E 's/.*[:\/](.*)\/.*(\.git)?/\1/' | sed 's/\.git//' | tr '[:upper:]' '[:lower:]')
REPO_NAME=$(echo "$GIT_URL" | sed -E 's/.*[:\/].*\/(.*)(\.git)?/\1/' | sed 's/\.git//' | tr '[:upper:]' '[:lower:]')

# Default image name
IMAGE_BASE="$REPO_OWNER/$REPO_NAME"

# --- Interactivity ---
echo "----------------------------------------------------"
echo " OpenBao Docker Hub Publisher"
echo "----------------------------------------------------"
echo "Detected Repo Owner: $REPO_OWNER"
echo "Default Image: $IMAGE_BASE"
echo -n "Enter tag [latest]: "
read TAG
TAG=${TAG:-latest}

FULL_IMAGE="$IMAGE_BASE:$TAG"

echo "Targeting: $FULL_IMAGE"
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
# We use 'make bin' which runs scripts/build.sh with UI tags
make bin

if [[ ! -f "bin/bao" ]]; then
    echo "Error: Build failed, bin/bao not found."
    exit 1
fi

echo "==> Phase 2: Building Docker image..."
# Use the root Dockerfile which is used for production releases
docker build --build-arg BIN_NAME=bin/bao -t "$FULL_IMAGE" .

# --- Publication ---
echo "==> Phase 3: Pushing to GHCR..."
if ! docker push "$FULL_IMAGE"; then
    echo "----------------------------------------------------"
    echo "Push failed (likely authentication). Please log in to GHCR."
    echo "Note: Use a Personal Access Token (PAT) with 'write:packages' scope."
    echo "----------------------------------------------------"
    echo -n "GHCR Username [$REPO_OWNER]: "
    read USERNAME
    USERNAME=${USERNAME:-$REPO_OWNER}
    
    echo -n "GHCR PAT/Password: "
    read -s PASSWORD
    echo
    
    if [[ -z "$PASSWORD" ]]; then
        echo "Error: Password cannot be empty."
        exit 1
    fi
    
    echo "$PASSWORD" | docker login ghcr.io -u "$USERNAME" --password-stdin
    
    echo "==> Retrying push..."
    docker push "$FULL_IMAGE"
fi

echo "----------------------------------------------------"
echo "SUCCESS!"
echo "Image published: https://$FULL_IMAGE"
echo "----------------------------------------------------"
