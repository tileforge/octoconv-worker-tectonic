# tectonic engine output, captured from a real run

These two files are what tectonic actually printed, byte for byte. They are not
transcriptions, not reconstructions, and not composed to match a parser.

| | |
|---|---|
| image | `octoconv-tectonic-worker` (`Dockerfile.tectonic-worker`), arm64 |
| engine | Tectonic 0.17.0 |
| bundle | `/opt/tectonic/octoconv-bundle.ttb`, baked into the image |
| user | `nobody` |
| network | `--network none` |
| date | 2026-09-04 |

Command, per document:

```
tectonic -X compile /w/<doc>.tex --untrusted \
    -b /opt/tectonic/octoconv-bundle.ttb -o /tmp -p
```

Both runs exited **1**. That number was measured on the `docker run` itself,
not through a pipe: the first attempt piped into `tail` and read tail's `0`,
which would have made "the engine exits 0 on a broken document" a finding that
was never true.

## `missing-package.txt`

Source document: `\documentclass{article}` + `\usepackage{pgfplots}`. pgfplots
is not in the bundle. This is the file `tectonicMissingPackage` parses, and the
reason the parser was written after the capture rather than before it: this
project's own retrospective records four bypasses of a single bound produced by
predicting a format instead of testing against one.

The line that matters:

```
! ! LaTeX Error: File `pgfplots.sty' not found..
```

A **file** name, not a package name. `pgfplots.sty` → `pgfplots` is the
parser's whole job, and the `.sty` suffix is what makes it safe: a missing
`.def`, `.cfg` or `.tex` file is NOT a package the client can remove, and the
parser must decline to name one rather than guess.

## `undefined-control-sequence.txt`

Source document: `\documentclass{article}` + `Hello
\thisControlSequenceDoesNotExist{x}`. The negative control for the parser —
a terminal compilation failure with no missing file at all, which must produce
the generic engine error and no package name.

## Noise that is present in BOTH, including successful runs

```
Fontconfig error: Cannot load default config file: No such file: (null)
```

It appears on every run in this image, including ones that produce a PDF. A
parser that treated any line containing "error" as the cause would report it.
