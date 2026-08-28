#!/bin/sh
# Install this checkout as the `lc` command without needing a published wheel.
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

if command -v uv >/dev/null 2>&1; then
    exec uv tool install --force "$repo_dir"
fi

if command -v pipx >/dev/null 2>&1; then
    exec pipx install --force "$repo_dir"
fi

exec python3 -m pip install --user --upgrade "$repo_dir"
