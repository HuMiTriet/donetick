#!/bin/bash
# Build the self-hosted frontend, embed it in the backend, and cross-compile
# a linux/arm64 binary at ./donetick (next to this script).
set -euo pipefail

# This script lives in the backend repo; ROOT is the parent folder that holds
# both donetick/ and donetick-frontend/.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "Building the frontend..."
cd "$ROOT/donetick-frontend"
npm ci
npx vite build --mode selfhosted

echo "Building the backend..."
cd "$ROOT/donetick"
rm -rf ./frontend/dist
cp -r "$ROOT/donetick-frontend/dist" ./frontend/dist

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w \
  -X donetick.com/core/config.Version=$(git describe --tags --always --dirty) \
  -X donetick.com/core/config.Commit=$(git rev-parse --short HEAD) \
  -X donetick.com/core/config.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -buildvcs=false -o donetick .

