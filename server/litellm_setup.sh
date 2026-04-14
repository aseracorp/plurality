#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VENV_DIR="$SCRIPT_DIR/litellm_venv"

echo "Setting up LiteLLM virtual environment..."

if [ ! -d "$VENV_DIR" ]; then
    python3 -m venv "$VENV_DIR"
    echo "Created virtual environment at $VENV_DIR"
fi

source "$VENV_DIR/bin/activate"
pip install --no-cache-dir -q -r "$SCRIPT_DIR/litellm_requirements.txt"

echo "LiteLLM setup complete."

# When run locally (not in Docker), print usage instructions
if [ -z "$DOCKER_BUILD" ]; then
    echo ""
    echo "To start LiteLLM locally:"
    echo "  source $VENV_DIR/bin/activate"
    echo "  litellm --config $SCRIPT_DIR/litellm_config.yaml --port 4000 --host 127.0.0.1"
fi
