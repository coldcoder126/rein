#!/bin/sh
# Prepares a release: every module gets the same version.
#   scripts/release.sh v0.3.0
# It sets the version the sub-modules require of the other rein modules, tidies and tests them, and prints the commands
# that commit and tag the release. Nothing is committed, tagged or pushed by the script.
#
# Locally the modules find one another through replace directives, which do not apply to users of the modules;
# they resolve the required versions instead, so all tags must be on the same commit.
set -eu
version=${1:?usage: scripts/release.sh vX.Y.Z}
case "$version" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "version must look like v1.2.3" >&2; exit 1 ;; esac
cd "$(dirname "$0")/.."
export GOWORK=off
root=github.com/coldcoder126/rein

tags=$version
for mod in $(find . -mindepth 2 -name go.mod -not -path './.git/*' | sort); do
	dir=$(dirname "$mod" | sed 's#^\./##')
	for dep in $(cd "$dir" && go mod edit -json | sed -n 's|^.*"Path": "\('"$root"'[^"]*\)".*$|\1|p' | sort -u); do
		[ "$dep" = "$root/$dir" ] || (cd "$dir" && go mod edit -require="$dep@$version")
	done
	(cd "$dir" && go mod tidy)
	# The examples are not a library and are not tagged.
	[ "$dir" = examples ] || tags="$tags $dir/$version"
done
scripts/modules.sh go vet ./...
scripts/modules.sh go test ./...

echo
echo "Ready. Review the diff, then:"
echo "  git commit -am 'Release $version'"
for tag in $tags; do echo "  git tag $tag"; done
echo "  git push origin main $tags"
