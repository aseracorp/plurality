@echo off
setlocal

set SCRIPT_DIR=%~dp0
set VENV_DIR=%SCRIPT_DIR%litellm_venv
set PYTHON_VERSION=3.13

echo Setting up LiteLLM virtual environment...

REM Ensure uv is available. uv manages its own Python toolchain so we don't
REM depend on whatever Python the host happens to ship.
where uv >nul 2>nul
if errorlevel 1 (
    echo uv not found, installing via https://astral.sh/uv/install.ps1...
    powershell -ExecutionPolicy ByPass -c "irm https://astral.sh/uv/install.ps1 | iex"
    set "PATH=%USERPROFILE%\.local\bin;%PATH%"
)

if not exist "%VENV_DIR%" (
    uv venv --python %PYTHON_VERSION% "%VENV_DIR%"
    echo Created virtual environment at %VENV_DIR%
)

uv pip install --python "%VENV_DIR%\Scripts\python.exe" -r "%SCRIPT_DIR%litellm_requirements.txt"

echo.
echo Setup complete. To start LiteLLM locally:
echo   %VENV_DIR%\Scripts\activate.bat
echo   litellm --config %SCRIPT_DIR%litellm_config.yaml --port 4000 --host 127.0.0.1

endlocal
