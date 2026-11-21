#!/usr/bin/env bash
set -euo pipefail

# PR checks and platform releases must compile the same catalog bytes from Git.
for name in models.json codex_client_models.json devin_models.json; do
  path="internal/registry/models/$name"
  expected=$(git rev-parse --verify "HEAD:$path")
  actual=$(git hash-object --no-filters -- "$path")
  if [[ "$actual" != "$expected" ]]; then
    echo "Embedded model catalog differs from HEAD: $path" >&2
    exit 1
  fi
  printf 'Embedded catalog %s %s\n' "$name" "$actual"
done

go run -mod=readonly ./cmd/validate_codex_models --file internal/registry/models/codex_client_models.json
