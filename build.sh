#!/usr/bin/env bash
# Build registry.tailb8bcc6.ts.net/video-summarizer:<version> for the NAS
# (amd64) and, with --push, publish it to the homelab registry and move :latest
# to it. The deployment tracks :latest, so a redeploy picks it up.
#
#   ./build.sh [--push] [patch|minor|major|vX.Y.Z]
#
# The version defaults to the latest vX.Y.Z git tag with its patch bumped. A
# push also tags HEAD with it and pushes the tag, so each published version
# names the commit it was built from; that needs a clean working tree.
set -euo pipefail

usage="usage: build.sh [--push] [patch|minor|major|vX.Y.Z]"
push=false
bump=patch
for arg in "$@"; do
	case $arg in
	--push) push=true ;;
	patch | minor | major | v[0-9]*.[0-9]*.[0-9]*) bump=$arg ;;
	*) echo "$usage" >&2; exit 2 ;;
	esac
done
cd "$(dirname "$0")"

if $push && [[ -n $(git status --porcelain) ]]; then
	echo "build.sh: uncommitted changes; commit them before pushing" >&2
	exit 1
fi

git fetch --quiet --tags origin
latest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n1)
if [[ $bump == v* ]]; then
	version=$bump
else
	IFS=. read -r major minor patch <<<"${latest#v}"
	major=${major:-0} minor=${minor:-0} patch=${patch:-0}
	case $bump in
	major) version=v$((major + 1)).0.0 ;;
	minor) version=v$major.$((minor + 1)).0 ;;
	patch) version=v$major.$minor.$((patch + 1)) ;;
	esac
fi
if git rev-parse --quiet --verify "refs/tags/$version" >/dev/null; then
	echo "build.sh: $version is already tagged" >&2
	exit 1
fi
repo=registry.tailb8bcc6.ts.net/video-summarizer
image=$repo:$version
echo "building $image (latest tag: ${latest:-none})"

go test ./...
docker build --platform linux/amd64 -t "$image" .
if $push; then
	docker push "$image"
	git tag "$version"
	git push --quiet origin "refs/tags/$version"
	# :latest follows the highest version, so an explicit older one (a
	# backport) does not move it back.
	if [[ $(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n1) == "$version" ]]; then
		docker tag "$image" "$repo:latest"
		docker push "$repo:latest"
	fi
	echo "pushed $image; redeploy the stack to pick it up"
fi
