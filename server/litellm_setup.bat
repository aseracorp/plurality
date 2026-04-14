@echo off
setlocal

set SCRIPT_DIR=%~dp0
set VENV_DIR=%SCRIPT_DIR%litellm_venv

echo Setting up LiteLLM virtual environment...

if not exist "%VENV_DIR%" (
    python -m venv "%VENV_DIR%"
    echo Created virtual environment at %VENV_DIR%
)

call "%VENV_DIR%\Scripts\activate.bat"
pip install -r "%SCRIPT_DIR%litellm_requirements.txt"

echo.
echo Setup complete. To start LiteLLM locally:
echo   %VENV_DIR%\Scripts\activate.bat
echo   litellm --config %SCRIPT_DIR%litellm_config.yaml --port 4000 --host 127.0.0.1

endlocal
