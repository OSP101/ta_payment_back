#!/usr/bin/env bash
# Preview by default. Add -send only on the meeting day.
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./cmd/ta-accounts -mode send \
  -manifest data/account-imports/legacy-ta-20261111.enc "$@"
