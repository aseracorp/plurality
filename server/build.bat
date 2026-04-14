@echo off
REM Create build directory if it doesn't exist
if not exist build mkdir build
REM Clean build directory
if exist build\* del /Q build\*

REM Build for Windows
echo Building for Windows...
set GOOS=windows
set GOARCH=amd64
go build -o build\Plurality.exe src\index.go src\stripe.go

if errorlevel 1 goto :error

REM Copy LiteLLM files needed at runtime
if not exist build\litellm mkdir build\litellm
copy litellm_config.yaml build\litellm\
copy litellm_proxy.py build\litellm\
copy litellm_requirements.txt build\litellm\
copy litellm_setup.bat build\litellm\

echo Success!
