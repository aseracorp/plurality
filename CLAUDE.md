# Plurality Development Guide

## Build Commands
- **Server (Go)**:
  - Linux/Mac: `./server/build.sh`
  - Windows: `server/build.bat`
- **Client (Flutter)**:
  - Run app: `cd client && flutter run`
  - Run tests: `cd client && flutter test`
  - Single test: `cd client && flutter test test/widget_test.dart`
  - Generate code: `cd client && flutter pub run build_runner build`

## Lint & Format
- **Go**: `cd server && go fmt ./...`
- **Dart**: `cd client && dart format lib/ test/`
- **Lint**: `cd client && flutter analyze`

## Code Style Guidelines
- **Naming**: PascalCase for classes, camelCase for variables/functions
- **Types**: Use strong typing everywhere possible
- **Imports**: Group imports (Flutter, project, third-party)
- **Error Handling**: Go - return with context (`fmt.Errorf`), Dart - try/catch with meaningful messages
- **Comments**: Document public APIs and complex logic
- **API Routes**: Go functions with `API_` prefix expose HTTP endpoints

## State Management
- Flutter: Use Riverpod for state management
- Go: Pass context between layers for request state

## File Organization
- Group by feature/domain, not by type
- Keep related code in same package/directory