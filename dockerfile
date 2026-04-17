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
FROM golang:1.25-alpine AS go_builder

# Install build dependencies (gcc/musl-dev for CGO/SQLite, sqlite-dev for sqlite3ext.h used by sqlite-vec, Python for LiteLLM venv)
RUN apk add --no-cache bash git gcc musl-dev sqlite-dev python3 py3-pip py3-virtualenv

# Copy the Go app source
WORKDIR /app
COPY server/ ./server/
WORKDIR /app/server

# Build the Go application
RUN chmod +x build.sh
RUN CGO_CFLAGS="-I$(go list -m -f '{{.Dir}}' github.com/mattn/go-sqlite3)" ./build.sh

# Stub out pyroscope-io (needs Rust/cargo to build, not needed at runtime)
RUN mkdir -p /tmp/dummy-pyroscope && \
    printf '[project]\nname = "pyroscope-io"\nversion = "99.0.0"\n' > /tmp/dummy-pyroscope/pyproject.toml && \
    echo 'pyroscope-io>=99.0.0' > /tmp/pip-constraints.txt

# Build LiteLLM venv inside the litellm subfolder
RUN python3 -m venv --copies build/litellm/litellm_venv && \
    build/litellm/litellm_venv/bin/pip install --no-cache-dir /tmp/dummy-pyroscope && \
    PIP_CONSTRAINT=/tmp/pip-constraints.txt \
    build/litellm/litellm_venv/bin/pip install --no-cache-dir -r litellm_requirements.txt

# Download Lightpanda headless browser (stateful MCP server for interactive browsing)
RUN curl -L -o build/lightpanda https://github.com/lightpanda-io/browser/releases/download/nightly/lightpanda-x86_64-linux && \
    chmod +x build/lightpanda

# Stage 3: Create the final image
FROM alpine:latest

# Install runtime dependencies (Python needed for LiteLLM proxy;
# nodejs/npm needed for npx-based MCP servers).
RUN apk add --no-cache ca-certificates python3 nodejs npm

# Create a non-root user to run the app
RUN adduser -D appuser
USER appuser

WORKDIR /app

# Copy the compiled Go binary and litellm files from the builder stage
COPY --from=go_builder /app/server/build/ /app/

# Copy the Flutter web build to the static directory
RUN mkdir -p /app/web
COPY --from=flutter_builder /app/client/build/web /app/web

# Create the users-data directory for per-user SQLite DBs and attachments
RUN mkdir -p /app/users-data
VOLUME /app/users-data

# Global server config: mcp.json + skills/
RUN mkdir -p /app/data
VOLUME /app/data

# Expose the port the server listens on
EXPOSE 8090

# Run the server
CMD ["/app/Plurality"]
