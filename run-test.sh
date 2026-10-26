#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")"

if [[ ! -f .env ]]; then
  printf 'Create .env with GITHUB_TOKEN and GITLAB_TOKEN first.\n' >&2
  exit 1
fi

set -a
source ./.env
set +a

: "${GITHUB_TOKEN:?Set GITHUB_TOKEN in .env}"
: "${GITLAB_TOKEN:?Set GITLAB_TOKEN in .env}"

exec ./test-run/bin/deplens-sweep --config sweep-test.yaml
