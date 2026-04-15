#!/bin/bash

# Create build directory if it doesn't exist
mkdir -p build
rm -rf build/*

# Build for Linux
echo "Building for Linux..."
# sqlite-vec CGO needs sqlite3.h from mattn/go-sqlite3
MATTN_DIR=$(go list -m -f '{{.Dir}}' github.com/mattn/go-sqlite3)
export CGO_CFLAGS="-I${MATTN_DIR}"

if ! CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -tags "fts5" -o build/Plurality src/index.go src/stripe.go; then
  echo "Linux build failed. Exiting..."
  exit 1
fi

# Build for Windows
# echo "Building for Windows..."
# GOOS=windows GOARCH=amd64 go build -o build/Plurality.exe src/index.go

# Copy LiteLLM files needed at runtime
mkdir -p build/litellm
cp litellm_config.yaml litellm_proxy.py litellm_requirements.txt litellm_setup.sh build/litellm/

echo "Build complete. Binaries are in the 'build' directory."
echo "Linux binary: build/Plurality"