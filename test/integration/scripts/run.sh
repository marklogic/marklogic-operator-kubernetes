#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
# Preserve SCENARIO=... and delegate discovery and selection to the Go catalog.
exec go run ./test/integration/cmd/integration-runner "$@"
