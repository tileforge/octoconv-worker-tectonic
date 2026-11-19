# Tectonic bundle — provenance

The reduced TeX distribution `cmd/tectonic-worker` typesets with. It exists so
the worker needs no network at run time: with a bundle baked in, a client's
`\usepackage` cannot decide what gets downloaded.

## The artifact

| | |
|---|---|
| release | [`tectonic-bundle-2023.1`](https://github.com/tileforge/octoconv/releases/tag/tectonic-bundle-2023.1) |
| asset | `octoconv-tectonic-bundle-2023.1.ttb` |
| size | 122 MiB (2.69 GiB unreduced — a 95% reduction) |
| sha256 | `f42d5eed010a826548d346e3161df4b7b0237cad66ac316221c09a3b311502ff` |
| files | 9701 |
| built | 2026-09-04 |

The digest was computed locally from the file that was built and tested, then
**re-verified against the downloaded asset** — `Dockerfile.pandoc-worker`'s rule
that a checksum is never taken from a published page applies at least as
strongly to an artifact we publish ourselves, where the temptation to trust it
is greater.

## The input

| | |
|---|---|
| source | `texlive-20230313-texmf.tar.xz` |
| from | <https://ftp.math.utah.edu/pub/tex/historic/systems/texlive/2023/> |
| sha256 | `4c4dc77a025acaad90fb6140db2802cdb7ca7a9a2332b5e3d66aa77c43a81253` |
| size | 3.6 GiB compressed, 6.9 GiB unpacked |

`scripts/tectonic-bundle-build.sh` verifies this digest before unpacking a byte.

## The builder, and one honest gap

| | |
|---|---|
| repository | <https://github.com/tectonic-typesetting/tectonic-texlive-bundles> |
| pinned revision | `4b111c1f32492ca36d8656b670d3acab8d4bc066` |

The bundle's contents depend on that repository's code, its
`bundles/texlive2023` specification, its `include/` and `patches/` trees — not
only on the texmf tarball. The build script pins and verifies the revision:
following the default branch means the reproduction recipe can run different
code later and produce a different bundle while every digest here still looks
satisfied, and it leaves an upstream compromise unchecked. Added in review
(PR #16, codex round 2).

**The gap:** the 122 MiB asset above was built on 2026-09-04 from an unpinned
clone, and that working tree no longer exists, so the revision it used cannot be
recovered — the pin is upstream's HEAD as of 2026-09-05. Rebuilding at the pin
may therefore not reproduce this asset byte for byte. What ships is defined by
the sha256 in the table, enforced inside the image build; the pin is what makes
the NEXT build reproducible.

## How the reduction is decided

The spec is **derived from upstream's** `bundles/texlive2023` rather than
written from scratch: a hand-written `bundle.toml` would silently drop
upstream's `include/` and `patches/` trees, which the LaTeX format build needs
and which are not in the texmf tarball at all.

tectonic-texlive-bundles selects by **exclusion**, and
`internal/convert/texpackages.go` is an allowlist, so the build enumerates what
is present in the tarball and emits an ignore rule for everything the keep lists
do not name — 3070 package rules and 561 font rules on this build.

The keep lists are **measured, not written**:

- `keep-packages.txt` — 67 directories. The 18 declared packages resolve to
  them; the list comes from compiling a document that uses the whole declared
  set against the full bundle and mapping every file tectonic resolved to its
  directory.
- `keep-fonts.txt` — 13 directories. Fonts are 93% of an unreduced bundle
  (2.6 GB of 2.8 GB), so this list is what determines the size.

## Sizes, measured rather than asserted

| | files | size |
|---|---|---|
| floor: LaTeX format + `article` | 354 | 44.47 MiB |
| the whole declared package set adds | 102 | 2.44 MiB |
| of which `pgf`/`tikz` | 55 | 0.91 MiB |

64-CONTEXT.md D-01 expected `tcolorbox`'s dependency on `pgf`/`tikz` to be "a
real size cost". It is about 2% of the floor. What costs is fonts.

## Rebuilding

```sh
scripts/tectonic-bundle-build.sh          # writes MANIFEST.txt, prints size and sha256
```

Then publish the `.ttb` as a new release under a bumped tag, update the table
above, and update the pin in `Dockerfile.tectonic-worker`. Changing the declared
set means regenerating the keep lists first — a name added to
`texpackages.go` alone will be excluded from the bundle by rules generated from
a stale closure, and the three-way test in `internal/convert` is what fails.

## Two builds that were wrong, kept because they explain the shape

A first build excluded only `tex/latex` packages: 1.4 GB, because that tree is
4% of the artifact. A second added a hand-written four-family font list: 112 MB,
and it **typeset nothing** — `graphics-cfg`, `graphics-def` and the font
mapping files were missing, because a keep list written by name misses exactly
the directories nobody thinks to name. Full detail in
`.planning/phases/64-tectonic-ninth-engine-class/evidence/64-01-floor-and-tikz-cost.txt`.
