@echo off
REM Create build directory if it doesn't exist
if not exist build mkdir build
REM Clean build artifacts but preserve user data (build\data, build\users-data).
for %%F in (build\*) do (
    del /Q "%%F"
)
for /D %%D in (build\*) do (
    if /I not "%%~nxD"=="data" if /I not "%%~nxD"=="users-data" rmdir /S /Q "%%D"
)

REM Build for Windows
echo Building for Windows...
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=1

REM sqlite-vec CGO: define SQLITE_CORE so sqlite-vec uses its bundled sqlite3.h
REM instead of sqlite3ext.h (which turns sqlite3_auto_extension into an
REM unparseable macro for cgo). Also include mattn/go-sqlite3 and sqlite-vec
REM cgo dirs explicitly so sqlite3.h resolves on all platforms.
for /f "delims=" %%i in ('go list -m -f "{{.Dir}}" github.com/mattn/go-sqlite3') do set MATTN_DIR=%%i
for /f "delims=" %%i in ('go list -m -f "{{.Dir}}" github.com/asg017/sqlite-vec-go-bindings') do set SQLVEC_DIR=%%i\cgo
set CGO_CFLAGS=-DSQLITE_CORE -I%MATTN_DIR% -I%SQLVEC_DIR%

go build -tags "fts5" -o build\Plurality.exe .\src

if errorlevel 1 goto :error

REM Copy LiteLLM runtime code (proxy script + setup helpers). The YAML config
REM is user-editable and lives under data\, not next to the binary.
if not exist build\litellm mkdir build\litellm
copy litellm_proxy.py build\litellm\
copy litellm_requirements.txt build\litellm\
copy litellm_setup.bat build\litellm\

REM Seed user-editable defaults if they don't already exist (skip clobber).
if not exist build\data\presets mkdir build\data\presets
xcopy /Y /D data\presets\*.json build\data\presets\ >nul 2>&1
if not exist build\data\litellm_config.yaml copy data\litellm_config.yaml build\data\

echo Success!
