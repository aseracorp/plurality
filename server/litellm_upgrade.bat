@echo off
setlocal enabledelayedexpansion

set SCRIPT_DIR=%~dp0
set VENV_DIR=%SCRIPT_DIR%litellm_venv
set PYTHON=%VENV_DIR%\Scripts\python.exe

if not exist "%VENV_DIR%" (
    echo No venv found at %VENV_DIR%. Run litellm_setup.bat first.
    exit /b 1
)

where uv >nul 2>nul
if errorlevel 1 set "PATH=%USERPROFILE%\.local\bin;%PATH%"

echo Upgrading all packages in the LiteLLM venv (in place)...

REM Collect installed package names, then upgrade them all to latest,
REM without touching litellm_requirements.txt.
set PKGS=
for /f "delims==" %%p in ('uv pip freeze --python "%PYTHON%"') do set PKGS=!PKGS! %%p

uv pip install --python "%PYTHON%" --upgrade%PKGS%

echo.
echo LiteLLM venv upgrade complete.
uv pip show --python "%PYTHON%" litellm | findstr /i /b "Name Version"

endlocal
