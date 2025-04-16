#!/bin/bash

# Create build directory if it doesn't exist
mkdir -p build
rm -rf build/*

# Build for Linux
echo "Building for Linux..."
if ! GOOS=linux GOARCH=amd64 go build -o build/Plurality src/index.go src/stsripe.go; then
  echo "Linux build failed. Exiting..."
  exit 1
fi

# Build for Windows
# echo "Building for Windows..."
# GOOS=windows GOARCH=amd64 go build -o build/Plurality.exe src/index.go

echo "Build complete. Binaries are in the 'build' directory."
echo "Linux binary: build/Plurality"