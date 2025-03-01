@echo off
REM Create build directory if it doesn't exist
if not exist build mkdir build
REM Clean build directory
if exist build\* del /Q build\*

REM Build for Windows
echo Building for Windows...
set GOOS=windows
set GOARCH=amd64
go build -o build\Plurality.exe src\index.go

if errorlevel 1 goto :error

echo Success!
