#!/usr/bin/env bash
# tectonic-bundle-fetch.sh -- put the TeX bundle into the build context.
#
# WHY THIS EXISTS
#
# Dockerfile.tectonic-worker COPYs deploy/tectonic-bundle/octoconv-bundle.ttb
# out of the build context, and that file is gitignored: it is 122 MiB of build
# output, published as a release asset rather than committed. The Dockerfile
# explains why it is not fetched from a URL inside the build (the repository is
# private, so the asset needs credentials, and putting credentials in an image
# build puts them in every image build).
#
# What that left behind, and what this script fixes: a fresh checkout could not
# build the image AT ALL. `docker compose up -d --build` and CI's bake job both
# failed on a missing COPY source, with an error that says nothing about where
# the file comes from. Found in review (PR #16, codex P1) -- the out-of-band
# fetch was documented in a comment, and a documented manual step that nothing
# runs is a step that does not happen.
#
# The digest is still verified INSIDE the image build, against the bytes that
# actually land there. This script verifies it too, before the build starts, so
# a truncated or wrong download is reported here rather than as a checksum
# failure three layers into a Docker build.
#
# USAGE
#
#   scripts/tectonic-bundle-fetch.sh          # fetch if missing or wrong
#   scripts/tectonic-bundle-fetch.sh --check  # verify only, never download
#
# Requires `gh` authenticated against a repository the release belongs to. In
# CI, GITHUB_TOKEN with contents:read is enough for a release in the same repo.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DEST="deploy/tectonic-bundle/octoconv-bundle.ttb"
DOCKERFILE="Dockerfile.tectonic-worker"
PROVENANCE="deploy/tectonic-bundle/PROVENANCE.md"

# The expected digest is READ FROM THE DOCKERFILE, never restated here. Two
# copies of a checksum are two checksums that can disagree, and the one that
# matters is the one the image build enforces.
EXPECTED="$(sed -n 's/^ARG TECTONIC_BUNDLE_SHA256=\([0-9a-f]\{64\}\).*/\1/p' "$DOCKERFILE" | head -1)"
if [ -z "$EXPECTED" ]; then
  echo "error: could not read ARG TECTONIC_BUNDLE_SHA256 from $DOCKERFILE" >&2
  echo "       (this script must not carry its own copy of the digest)" >&2
  exit 1
fi

# Release tag and asset pattern come from the provenance document, for the same
# reason: it is where they are already recorded and reviewed.
TAG="$(sed -n 's/.*releases\/tag\/\([A-Za-z0-9._-]*\)).*/\1/p' "$PROVENANCE" | head -1)"
[ -n "$TAG" ] || TAG="tectonic-bundle-2023.1"

digest_of() { shasum -a 256 "$1" 2>/dev/null | cut -d' ' -f1; }

if [ -f "$DEST" ]; then
  GOT="$(digest_of "$DEST")"
  if [ "$GOT" = "$EXPECTED" ]; then
    echo "bundle present and matches $DOCKERFILE's pin ($EXPECTED)"
    exit 0
  fi
  echo "bundle present but digest MISMATCHES the pin:" >&2
  echo "  have $GOT" >&2
  echo "  want $EXPECTED" >&2
  if [ "${1:-}" = "--check" ]; then exit 1; fi
  echo "re-downloading..." >&2
  rm -f "$DEST"
elif [ "${1:-}" = "--check" ]; then
  echo "error: $DEST is absent. Run this script without --check to fetch it," >&2
  echo "       or build the bundle yourself with scripts/tectonic-bundle-build.sh." >&2
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "error: gh is required to download the bundle from a private repository's release." >&2
  echo "       Alternatives: build it (scripts/tectonic-bundle-build.sh, hours), or place" >&2
  echo "       the asset at $DEST by hand." >&2
  exit 69
fi

echo "downloading the bundle from release $TAG ..."
gh release download "$TAG" --repo tileforge/octoconv --pattern '*.ttb' --output "$DEST" --clobber

GOT="$(digest_of "$DEST")"
if [ "$GOT" != "$EXPECTED" ]; then
  echo "error: downloaded bundle does not match the pin in $DOCKERFILE" >&2
  echo "  have $GOT" >&2
  echo "  want $EXPECTED" >&2
  echo "The release asset changed, or the download was truncated. NOT usable." >&2
  rm -f "$DEST"
  exit 1
fi
echo "ok: $DEST ($(wc -c < "$DEST" | tr -d ' ') bytes, sha256 $GOT)"
