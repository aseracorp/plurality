#!/bin/bash

# Create build directory if it doesn't exist
mkdir -p build
rm -rf build/*

# Build for Linux
echo "Building for Linux..."
# sqlite-vec CGO: define SQLITE_CORE so sqlite-vec uses sqlite3.h (not
# sqlite3ext.h, which turns sqlite3_auto_extension into an unparseable macro
# for cgo). Include mattn/go-sqlite3 and the sqlite-vec cgo dir on the include
# path. On systems where proxy.golang.org omits sqlite-vec's bundled sqlite3.h
# from the module zip (observed on alpine/Docker), the system sqlite3.h from
# sqlite-dev is used as a fallback.
MATTN_DIR=$(go list -m -f '{{.Dir}}' github.com/mattn/go-sqlite3)
SQLVEC_DIR=$(go list -m -f '{{.Dir}}' github.com/asg017/sqlite-vec-go-bindings)/cgo
# Alias BSD-style uint types (u_int8_t etc.) to stdint equivalents; musl
# (Alpine) doesn't provide the BSD names, so sqlite-vec.c's typedefs fail
# without these. Valid on glibc too (macro substitution yields self-typedefs).
export CGO_CFLAGS="-DSQLITE_CORE -Du_int8_t=uint8_t -Du_int16_t=uint16_t -Du_int64_t=uint64_t -I${MATTN_DIR} -I${SQLVEC_DIR}"

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