# Multi-stage build for efficiency

# Stage 1: Build the Flutter web app
FROM dart:stable AS flutter_builder

# Install Flutter
RUN apt-get update && apt-get install -y curl git unzip xz-utils zip libglu1-mesa

# Get Stable Branch
RUN git clone https://github.com/flutter/flutter.git /flutter && \
  git -C /flutter checkout stable
ENV PATH="/flutter/bin:${PATH}"

# Copy the Flutter app source
WORKDIR /app
COPY client/ ./client/
WORKDIR /app/client


# Build the Flutter web app
RUN flutter pub get
RUN flutter build web --release

# Stage 2: Build the Go server
FROM golang:1.25 AS go_builder

# Install build dependencies. libsqlite3-dev provides /usr/include/sqlite3.h, which
# sqlite-vec-go-bindings@v0.1.7-alpha.2 needs at CGO compile time — the Go
# module zip served by proxy.golang.org does not include sqlite3.h (only
# lib.go, sqlite-vec.c, sqlite-vec.h), so the system header is used as a
# fallback. mattn/go-sqlite3 still provides the actual sqlite implementation.
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash curl git gcc libc6-dev libsqlite3-dev python3 python3-pip python3-venv && \
    rm -rf /var/lib/apt/lists/*

# Copy the Go app source
WORKDIR /app
COPY server/ ./server/
WORKDIR /app/server

# Build the Go application (build.sh sets its own CGO_CFLAGS)
RUN chmod +x build.sh
RUN ./build.sh

# Copy litellm requirements for installation in final stage
RUN mkdir -p build/litellm && cp litellm_requirements.txt build/litellm/

# Download Lightpanda headless browser (stateful MCP server for interactive browsing)
RUN curl -L -o build/lightpanda https://github.com/lightpanda-io/browser/releases/download/nightly/lightpanda-x86_64-linux && \
    chmod +x build/lightpanda

# Stage 3: Create the final image
FROM debian:bookworm-slim

# Install runtime dependencies (Python needed for LiteLLM proxy;
# nodejs/npm needed for npx-based MCP servers).
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates python3 python3-venv python3-pip nodejs npm && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy the compiled Go binary and litellm files from the builder stage
COPY --from=go_builder /app/server/build/ /app/

# Build LiteLLM venv using runtime Python (avoids glibc version mismatch)
# Stub out pyroscope-io (needs Rust/cargo to build, not needed at runtime)
RUN mkdir -p /tmp/dummy-pyroscope && \
    printf '[project]\nname = "pyroscope-io"\nversion = "99.0.0"\n' > /tmp/dummy-pyroscope/pyproject.toml && \
    echo 'pyroscope-io>=99.0.0' > /tmp/pip-constraints.txt && \
    python3 -m venv /app/litellm/litellm_venv && \
    /app/litellm/litellm_venv/bin/pip install --no-cache-dir /tmp/dummy-pyroscope && \
    PIP_CONSTRAINT=/tmp/pip-constraints.txt \
    /app/litellm/litellm_venv/bin/pip install --no-cache-dir -r /app/litellm/litellm_requirements.txt && \
    rm -rf /tmp/dummy-pyroscope /tmp/pip-constraints.txt

# Copy the Flutter web build to the static directory
RUN mkdir -p /app/web
COPY --from=flutter_builder /app/client/build/web /app/web

# Create directories for volumes
RUN mkdir -p /app/users-data /app/data

# Declare volumes
VOLUME /app/users-data
VOLUME /app/data

# Expose the port the server listens on
EXPOSE 8090

# Run the server
CMD ["/app/Plurality"]
