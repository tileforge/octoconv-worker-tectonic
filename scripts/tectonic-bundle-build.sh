#!/usr/bin/env bash
# Build the reduced TeX bundle cmd/tectonic-worker ships with (64-01, TEX-02/TEX-03).
#
# WHAT THIS PRODUCES
#   deploy/tectonic-bundle/MANIFEST.txt   what the bundle actually contains
#   <build-dir>/output/…ttb               the bundle itself
#   a printed SHA-256 and size for both
#
# WHY A SCRIPT AND NOT A DOCUMENTED PROCEDURE
#   The bundle is an artifact the image pins by digest. If the way it is built
#   lives in prose, the next rebuild is a different bundle for reasons nobody
#   can reconstruct. This is the reproduction.
#
# THE THING THAT MAKES THIS NON-TRIVIAL, STATED UP FRONT
#   tectonic-texlive-bundles selects by EXCLUSION: bundle.toml carries `ignore`
#   regexes, and everything not ignored is included. internal/convert/
#   texpackages.go is an ALLOWLIST. Rust's regex crate has no negative
#   lookahead, so "keep only these" cannot be written as one pattern.
#
#   So this script enumerates the LaTeX package directories present in the
#   TeXLive tree and emits an ignore rule for every directory that is not in
#   the allowlist. That inversion is the whole reason this file is more than
#   three lines, and it is why the MANIFEST it writes is authoritative: it
#   records what survived, not what was asked for.
#
#   Consequence worth knowing before reading a diff: adding a package to the Go
#   list SHRINKS the generated ignore list. The two move in opposite directions.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="${BUILD_DIR:-${REPO_ROOT}/.tectonic-bundle-build}"
BUNDLES_REPO="${BUNDLES_REPO:-${BUILD_DIR}/tectonic-texlive-bundles}"

# Pinned, and pinned by CONTENT rather than by tag. Both values come from the
# upstream bundle spec this build is derived from; the hash is verified before
# a single byte is unpacked.
TEXMF_URL="https://ftp.math.utah.edu/pub/tex/historic/systems/texlive/2023/texlive-20230313-texmf.tar.xz"
# Observed locally on 2026-09-04 by downloading the file and running shasum
# against it -- never copied from a published checksum page, which is
# Dockerfile.pandoc-worker's own rule. Override the variable to build against a
# different TeXLive; leave it and a corrupted or substituted tarball stops the
# build before a byte is unpacked.
TEXMF_XZ_SHA256="${TEXMF_XZ_SHA256:-4c4dc77a025acaad90fb6140db2802cdb7ca7a9a2332b5e3d66aa77c43a81253}"
TEXMF_TAR="texlive-20230313-texmf.tar"
TEXMF_ROOT_DIR="texlive-20230313-texmf/texmf-dist"

die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }
say() { printf '\n=== %s\n' "$*"; }

say "prerequisites"
# pv is a hard dependency of the upstream builder, not of this script -- named
# here so a missing one fails in ten seconds rather than forty minutes in.
for bin in cargo git curl xz shasum patch diff pv; do
  command -v "$bin" >/dev/null || die "$bin is not on PATH (upstream builder requires cargo, bash, pv, GNU patch and diff)"
done

mkdir -p "$BUILD_DIR"

say "the declared package set, read from Go rather than restated here"
# The allowlist has ONE home. A copy in this script would be a second list to
# forget, which is the defect the three-way test exists to catch -- so the
# script asks Go for it instead of carrying its own.
# A `while read` loop rather than `mapfile`: mapfile is a bash 4 builtin and
# macOS ships bash 3.2 as /bin/bash, so the mapfile version died on the first
# real run of this script. A build script that only works on the maintainer's
# shell is a build script that does not reproduce.
ALLOW=()
while IFS= read -r line; do
  [ -n "$line" ] && ALLOW+=("$line")
done < <(cd "$REPO_ROOT" && go run ./scripts/cmd/texpackages)
[ "${#ALLOW[@]}" -gt 0 ] || die "the declared package set came back empty"
printf '  %s\n' "${ALLOW[@]}"

say "TeXLive texmf tarball"
cd "$BUILD_DIR"
if [ ! -f "$TEXMF_TAR" ]; then
  if [ ! -f "$(basename "$TEXMF_URL")" ]; then
    echo "  downloading ~3.6 GiB compressed; this is the expensive step"
    curl -fL --retry 5 --retry-delay 5 -o "$(basename "$TEXMF_URL")" "$TEXMF_URL"
  fi
  observed="$(shasum -a 256 "$(basename "$TEXMF_URL")" | cut -d' ' -f1)"
  if [ -n "$TEXMF_XZ_SHA256" ]; then
    [ "$observed" = "$TEXMF_XZ_SHA256" ] || die "texmf tarball digest mismatch: got $observed, want $TEXMF_XZ_SHA256"
    echo "  digest verified: $observed"
  else
    echo "  OBSERVED DIGEST (record it, then set TEXMF_XZ_SHA256 to enforce): $observed"
  fi
  echo "  decompressing"
  xz -dk "$(basename "$TEXMF_URL")"
fi

say "upstream builder"
# PINNED, for the same reason the texmf tarball above is. The bundle's contents
# depend on this repository's builder code, its bundles/texlive2023
# specification, its patches and its include files -- so following the default
# branch means the advertised reproduction can execute different code next
# month and produce a different bundle while every digest recorded here still
# looks satisfied. It is also the supply-chain half: an upstream compromise
# would enter this build unchecked. Found in review (PR #16, codex round 2).
#
# Checked out by SHA and verified after the fact, because `git clone --branch`
# accepts only refs, and a ref can move.
BUNDLES_REV="${BUNDLES_REV:-4b111c1f32492ca36d8656b670d3acab8d4bc066}"
if [ ! -d "$BUNDLES_REPO" ]; then
  git clone --filter=blob:none https://github.com/tectonic-typesetting/tectonic-texlive-bundles "$BUNDLES_REPO"
  git -C "$BUNDLES_REPO" checkout --detach "$BUNDLES_REV"
fi
observed_rev="$(git -C "$BUNDLES_REPO" rev-parse HEAD)"
[ "$observed_rev" = "$BUNDLES_REV" ] || die "upstream builder is at $observed_rev, want $BUNDLES_REV (delete $BUNDLES_REPO to re-clone)"
echo "  pinned at $BUNDLES_REV"

say "compiling the allowlist into ignore rules"
# Enumerate what is actually in the tree, then exclude every LaTeX package
# directory the allowlist does not name. Enumerating rather than assuming is
# the point: an ignore list written from a package index would silently miss a
# directory the tarball happens to contain under another name.
SPEC_DIR="$BUILD_DIR/octoconv-bundle"
mkdir -p "$SPEC_DIR"
tar -tf "$TEXMF_TAR" "${TEXMF_ROOT_DIR}/tex/latex" 2>/dev/null \
  | sed -n "s|^${TEXMF_ROOT_DIR}/tex/latex/\([^/]*\)/.*|\1|p" \
  | sort -u > "$SPEC_DIR/present.txt"
present_count=$(wc -l < "$SPEC_DIR/present.txt" | tr -d ' ')
[ "$present_count" -gt 0 ] || die "found no tex/latex package directories in $TEXMF_TAR -- the root_dir assumption is wrong"
echo "  package directories present in the tree: $present_count"

# Keep the allowlist AND the dependency closure it resolves to. The closure is
# a MEASURED file, not a list written here.
#
# A hand-written keep list of nine "obviously needed" directories produced a
# 112 MB bundle that could not typeset a single document: graphics-cfg and
# graphics-def were absent because "graphics" was named and they are separate
# directories. Guessing dependency directories by name is whack-a-mole;
# deploy/tectonic-bundle/keep-packages.txt records what the declared set
# actually resolves to -- 18 declared packages need 67 directories.
grep -v '^#' "$REPO_ROOT/deploy/tectonic-bundle/keep-packages.txt" | grep -v '^$' > "$SPEC_DIR/keep.txt"
[ -s "$SPEC_DIR/keep.txt" ] || die "deploy/tectonic-bundle/keep-packages.txt is empty or all comments"
printf '%s\n' "${ALLOW[@]}" >> "$SPEC_DIR/keep.txt"
sort -u -o "$SPEC_DIR/keep.txt" "$SPEC_DIR/keep.txt"
comm -23 "$SPEC_DIR/present.txt" "$SPEC_DIR/keep.txt" > "$SPEC_DIR/drop.txt"
echo "  latex packages: keeping $(wc -l < "$SPEC_DIR/keep.txt" | tr -d ' '), dropping $(wc -l < "$SPEC_DIR/drop.txt" | tr -d ' ')"

# FONTS ARE THE BUNDLE, AND THE FIRST VERSION OF THIS SCRIPT MISSED THAT.
#
# Excluding 3093 of 3116 tex/latex package directories removed 87530 files and
# produced a 1.4 GB .ttb -- against 2.69 GiB for the full bundle. Measured on
# the result: fonts/ was 2.6 GB of a 2.8 GB tree (93%), tex/ was 109 MB (4%).
# The exclusion effort had been aimed at the four percent.
#
# The families to keep are measured, in deploy/tectonic-bundle/keep-fonts.txt,
# for the same reason the package closure is. A hand-written four-family list
# omitted fontmapping, and the bundle then failed with "Font mapping
# tex-text.tec not found" on the first document it was given.
tar -tf "$TEXMF_TAR" 2>/dev/null \
  | sed -n "s|^${TEXMF_ROOT_DIR}/fonts/[^/]*/[^/]*/\([^/]*\)/.*|\1|p" \
  | sort -u > "$SPEC_DIR/fonts-present.txt"
grep -v '^#' "$REPO_ROOT/deploy/tectonic-bundle/keep-fonts.txt" | grep -v '^$' | sort -u > "$SPEC_DIR/fonts-keep.txt"
[ -s "$SPEC_DIR/fonts-keep.txt" ] || die "deploy/tectonic-bundle/keep-fonts.txt is empty or all comments"
comm -23 "$SPEC_DIR/fonts-present.txt" "$SPEC_DIR/fonts-keep.txt" > "$SPEC_DIR/fonts-drop.txt"
echo "  font families: keeping $(wc -l < "$SPEC_DIR/fonts-keep.txt" | tr -d ' '), dropping $(wc -l < "$SPEC_DIR/fonts-drop.txt" | tr -d ' ')"

say "bundle spec, DERIVED from upstream's rather than authored here"
# Copying upstream's texlive2023 spec and adding our exclusions to it is
# deliberate. Writing a bundle.toml from scratch would silently drop their
# patches/ and include/ trees -- files the format build needs which are not in
# the texmf tarball at all -- and the failure would surface as a document that
# will not typeset, for reasons unrelated to anything we chose.
rm -rf "$SPEC_DIR/spec"
cp -R "$BUNDLES_REPO/bundles/texlive2023" "$SPEC_DIR/spec"
ln -sf "$BUILD_DIR/$TEXMF_TAR" "$SPEC_DIR/spec/$TEXMF_TAR"
python3 "$REPO_ROOT/scripts/tectonic-bundle-ignore.py" "$SPEC_DIR/spec/bundle.toml" "$SPEC_DIR/drop.txt" "$SPEC_DIR/fonts-drop.txt"

say "select + pack"
(cd "$BUNDLES_REPO/builder" && cargo run --release -- --build-dir "$BUILD_DIR/out" "$SPEC_DIR/spec/bundle.toml")

CONTENT="$(find "$BUILD_DIR/out" -type d -name content -print -quit)"
[ -n "$CONTENT" ] || die "select produced no content directory -- nothing to write a manifest from"

say "manifest"
# content/FILES is the builder's own record of what the bundle CONTAINS, which
# is the only thing the three-way test can honestly compare the Go list to.
# Reduced to sorted paths so a hash change in an unrelated file is not a
# manifest diff.
# doc/output.md documents a content/FILES listing, and this builder does not
# write one -- those docs describe the older build.sh flow. The content tree
# itself is the record, so the manifest is a walk of it. Verified by looking
# rather than trusting the documentation.
{
  echo "# Generated by scripts/tectonic-bundle-build.sh -- do not edit by hand."
  echo "# Source: a walk of the select job's content/ tree."
  echo "# Bundle content hash: $(cat "$CONTENT/SHA256SUM" 2>/dev/null || echo unknown)"
  (cd "$CONTENT" && find . -type f | sed "s|^\./||" | sort)
} > "$REPO_ROOT/deploy/tectonic-bundle/MANIFEST.txt"
echo "  $(grep -vc "^#" "$REPO_ROOT/deploy/tectonic-bundle/MANIFEST.txt") paths"

# The ADMISSION set (TEX-03), derived from the same walk in the same run.
#
# It is NOT the declared/supported set. convert.TeXPackages is what OctoConv
# documents and promises; this is what the engine can actually RESOLVE, which
# is a strictly larger set (the dependency closure, plus everything the LaTeX
# format itself carries). cmd/api refuses a \usepackage that is not in here,
# because that is the one that cannot possibly typeset -- gating on the
# declared 18 was measured against this bundle and would have refused twelve
# packages that compile perfectly, five of them in pandoc's own default
# template.
#
# Written HERE, beside the manifest, so the two cannot come from different
# builds. convert.TestResolvableSetMatchesTheManifest holds them equal and
# names this script when they drift.
grep -o "[^/]*\.sty$" "$REPO_ROOT/deploy/tectonic-bundle/MANIFEST.txt" \
  | sort -u > "$REPO_ROOT/internal/convert/texresolvable.txt"
echo "  $(wc -l < "$REPO_ROOT/internal/convert/texresolvable.txt" | tr -d ' ') resolvable .sty files"

say "artifact"
TTB="$(find "$BUILD_DIR/out" -name "*.ttb" -print -quit)"
if [ -n "$TTB" ]; then
  # Computed locally from the file we built, never copied from a published
  # checksum page -- Dockerfile.pandoc-worker's own rule, which applies at least
  # as strongly to an artifact we publish ourselves.
  printf "  %-8s %s\n" size "$(du -h "$TTB" | cut -f1)"
  printf "  %-8s %s\n" sha256 "$(shasum -a 256 "$TTB" | cut -d" " -f1)"
  printf "  %-8s %s\n" path "$TTB"
else
  echo "  no .ttb found -- the pack job did not run, or names its output differently"
fi
