#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
test_root=$(mktemp -d /tmp/xira-taskfile-clean-test.XXXXXX)
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

mkdir -p "$test_root/bin"
printf '%s\n' \
  '#!/bin/sh' \
  'echo "unexpected go invocation: $*" >&2' \
  'exit 97' > "$test_root/bin/go"
chmod +x "$test_root/bin/go"

if ! output=$(cd "$repo_root" && PATH="$test_root/bin:$PATH" task --dry clean 2>&1); then
  printf '%s\n' "$output" >&2
  echo "task clean must not invoke Go before it can remove a broken Go cache" >&2
  exit 1
fi

case "$output" in
  *'GOPATH="/tmp/xira-go" go clean -modcache'*) ;;
  *)
    printf '%s\n' "$output" >&2
    echo "task clean must use Go's read-only-aware cleaner on the Task-owned GOPATH only" >&2
    exit 1
    ;;
esac

case "$output" in
  *'/tmp/xira-go'*) ;;
  *)
    printf '%s\n' "$output" >&2
    echo "task clean must remove the Task-managed module cache /tmp/xira-go" >&2
    exit 1
    ;;
esac
