#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VENV_DIR="$SCRIPT_DIR/litellm_venv"

if [ ! -d "$VENV_DIR" ]; then
    echo "No venv found at $VENV_DIR. Run litellm_setup.sh first."
    exit 1
fi

if ! command -v uv >/dev/null 2>&1; then
    export PATH="$HOME/.local/bin:$PATH"
fi

echo "Upgrading all packages in the LiteLLM venv (in place)..."

# Upgrade every package already installed in the venv to its latest version,
# without touching litellm_requirements.txt.
uv pip install --python "$VENV_DIR/bin/python" --upgrade \
    $(uv pip freeze --python "$VENV_DIR/bin/python" | cut -d= -f1)

echo "LiteLLM venv upgrade complete."
uv pip show --python "$VENV_DIR/bin/python" litellm | grep -i -E "^(Name|Version)"
