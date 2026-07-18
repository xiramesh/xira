#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
test_root=$(mktemp -d /tmp/xira-taskfile-build-test.XXXXXX)
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

binary="$test_root/bin/xira"
expected_version=$(awk -F '"' '/^[[:space:]]*Version[[:space:]]*=/ { print $2; exit }' "$repo_root/apps/xira/internal/version/version.go")
expected_commit=$(git -C "$repo_root" rev-parse --short HEAD)

(cd "$repo_root" && task build BIN_DIR="$test_root/bin" BIN="$binary")
output=$("$binary" version)

case "$output" in
  "xira $expected_version commit=$expected_commit date="*) ;;
  *)
    echo "task build metadata mismatch: $output" >&2
    exit 1
    ;;
esac

case "$output" in
  *'date=unknown' | *'date=')
    echo "task build must inject a non-empty build date: $output" >&2
    exit 1
    ;;
esac

go tool nm "$binary" > "$test_root/nm.txt"
if ! grep -q ' main\.main$' "$test_root/nm.txt"; then
  echo "task build must retain debug symbols" >&2
  exit 1
fi
