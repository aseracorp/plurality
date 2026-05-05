#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VENV_DIR="$SCRIPT_DIR/litellm_venv"
PYTHON_VERSION=3.13

echo "Setting up LiteLLM virtual environment..."

# Ensure uv is available. uv manages its own Python toolchain so we don't
# depend on whatever Python the host happens to ship (e.g. CachyOS currently
# defaults to 3.14, which has no prebuilt orjson wheel).
if ! command -v uv >/dev/null 2>&1; then
    echo "uv not found, installing via https://astral.sh/uv/install.sh..."
    curl -LsSf https://astral.sh/uv/install.sh | sh
    export PATH="$HOME/.local/bin:$PATH"
fi

if [ ! -d "$VENV_DIR" ]; then
    uv venv --python "$PYTHON_VERSION" "$VENV_DIR"
    echo "Created virtual environment at $VENV_DIR"
fi

uv pip install --python "$VENV_DIR/bin/python" -r "$SCRIPT_DIR/litellm_requirements.txt"

echo "LiteLLM setup complete."

# When run locally (not in Docker), print usage instructions
if [ -z "$DOCKER_BUILD" ]; then
    echo ""
    echo "To start LiteLLM locally:"
    echo "  source $VENV_DIR/bin/activate"
    echo "  litellm --config $SCRIPT_DIR/../data/litellm_config.yaml --port 4000 --host 127.0.0.1"
fi
