#!/usr/bin/env bash
# Run the same checks as CI locally.
#
# Bash Strict Mode: https://github.com/guettli/bash-strict-mode
trap 'echo -e "\nWarning: a command failed at ${BASH_SOURCE[0]}:$LINENO" >&2; exit 3' ERR
set -Eeuo pipefail

cd "$(dirname "$0")"

go build ./...
go test ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run

./scripts/update-readme.sh
if ! git diff --quiet -- README.md; then
	echo "README usage block was stale and has been regenerated -- commit README.md." >&2
	exit 1
fi

echo "OK"
