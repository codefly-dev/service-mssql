#!/bin/bash

# Script to build and push the mssql-alembic Docker image to Docker Hub
# Requires: docker login (with access to codeflydev organization)

set -e

echo "Building and pushing codeflydev/mssql-alembic:latest..."

# Check if logged into Docker
if ! docker info | grep -q "Username"; then
    echo "Error: Not logged into Docker Hub. Please run 'docker login' first."
    exit 1
fi

# Ensure buildx is set up
docker buildx create --use --name multiarch 2>/dev/null || docker buildx use multiarch || true

# Build and push multi-platform image
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t codeflydev/mssql-alembic:latest \
  -f migrations/Dockerfile.alembic \
  --push \
  migrations/

echo "Successfully pushed codeflydev/mssql-alembic:latest to Docker Hub!"

