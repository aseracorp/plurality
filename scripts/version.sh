#!/usr/bin/env sh
#
# Sync the bundled web version asset with the single source of truth (the
# Flutter pubspec version). Flutter auto-generates the SERVED /version.json from
# pubspec on every build; the BUNDLED assets/version.json is a static asset that
# Flutter only copies. If the two drift, the web app's update check
# (lib/utils/index.dart -> checkVersion) reloads forever. Running this keeps the
# bundled asset equal to the pubspec version so the check works.
#
# Run automatically:
#   - during the Docker build, before `flutter build web` (see dockerfile)
#   - on git pre-commit, which also stages the result (see .git/hooks/pre-commit)
#
# Safe to run by hand from anywhere: `sh scripts/version.sh`.

set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
ROOT_DIR=$(cd "$SCRIPT_DIR/.." && pwd)

PUBSPEC="$ROOT_DIR/client/pubspec.yaml"
TARGET="$ROOT_DIR/client/assets/version.json"

# pubspec line looks like:  version: 2.0.4+19
# The served /version.json carries only the part before "+", so match that.
VERSION=$(grep -m1 -E '^version:' "$PUBSPEC" | sed -E 's/^version:[[:space:]]*//; s/\+.*$//')

if [ -z "${VERSION:-}" ]; then
  echo "version.sh: could not read version from $PUBSPEC" >&2
  exit 1
fi

printf '{\n  "version": "%s"\n}\n' "$VERSION" > "$TARGET"
echo "version.sh: $TARGET -> $VERSION"
