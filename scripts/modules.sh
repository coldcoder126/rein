#!/bin/sh
# Runs a command in every module of the repository, stopping at the first failure.
#   scripts/modules.sh go test ./...
# The modules use replace directives for one another, so GOWORK is turned off.
set -eu
cd "$(dirname "$0")/.."
export GOWORK=off
for mod in $(find . -name go.mod -not -path './.git/*' | sort); do
	dir=$(dirname "$mod")
	echo "== $dir"
	(cd "$dir" && "$@")
done
