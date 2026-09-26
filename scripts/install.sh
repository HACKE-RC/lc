#!/bin/sh
# Install this checkout as the `lc` command into $(go env GOBIN) or $(go env GOPATH)/bin.
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

if ! command -v go >/dev/null 2>&1; then
    echo "install.sh: Go 1.25+ is required (https://go.dev/dl/)" >&2
    exit 1
fi

cd "$repo_dir"
exec go install -trimpath -ldflags "-s -w" .
