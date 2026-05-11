#!/bin/bash

# Create build directory if it doesn't exist. We deliberately do NOT wipe
# build/ wholesale — build/data/ holds user-owned config (user.json,
# config.json, mcp.json, presets, skills) and must survive rebuilds.
mkdir -p build
find build -mindepth 1 -maxdepth 1 ! -name 'data' ! -name 'users-data' -exec rm -rf {} +

# Build for Linux. GOOS/GOARCH can be overridden by the caller (e.g. the
# Dockerfile passes buildx's TARGETOS/TARGETARCH for multi-arch builds);
# default to linux/amd64 for plain local builds.
GOOS=${GOOS:-linux}
GOARCH=${GOARCH:-amd64}
echo "Building for ${GOOS}/${GOARCH}..."
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

if ! CGO_ENABLED=1 GOOS=$GOOS GOARCH=$GOARCH go build -tags "fts5" -o build/Plurality ./src; then
  echo "Build for ${GOOS}/${GOARCH} failed. Exiting..."
  exit 1
fi

# Build for Windows
# echo "Building for Windows..."
# GOOS=windows GOARCH=amd64 go build -o build/Plurality.exe src/index.go

# Copy LiteLLM runtime code (proxy script + setup helpers). The YAML config is
# user-editable and lives under data/, not next to the binary.
mkdir -p build/litellm
cp litellm_proxy.py litellm_requirements.txt litellm_setup.sh build/litellm/

# Seed user-editable defaults if they don't already exist (cp -n: never clobber).
mkdir -p build/data/presets
cp -n data/presets/*.json build/data/presets/ 2>/dev/null || true
cp -n data/litellm_config.yaml build/data/litellm_config.yaml 2>/dev/null || true

echo "Build complete. Binaries are in the 'build' directory."
echo "${GOOS}/${GOARCH} binary: build/Plurality"