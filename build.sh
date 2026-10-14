#!/usr/bin/env bash
# Build ghcr.io/setevik/video-summarizer:<version> for the NAS (amd64) and,
# with --push, publish it. The version is the only argument that matters:
# the deployment pins it, so bump both together.
#
#   ./build.sh v0.1.0 [--push]
set -euo pipefail

version=${1:?usage: build.sh <version> [--push]}
image=ghcr.io/setevik/video-summarizer:$version
cd "$(dirname "$0")"

go test ./...
docker build --platform linux/amd64 -t "$image" .
if [[ ${2:-} == --push ]]; then
	docker push "$image"
fi
