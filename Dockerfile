# Build stage
FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/tectonic-worker ./cmd/tectonic-worker

# Runtime stage. Two pinned artifacts, no engine source-build: tectonic ships
# fully-static musl binaries per arch, exactly the shape Dockerfile.pandoc-worker
# already uses.
#
# WHAT THIS IMAGE DELIBERATELY DOES NOT CONTAIN
#
#   biber, and perl.  Tectonic launches biber as an EXTERNAL program when a
#     document produces a .bcf, taking its argv from a .run.xml that the
#     document's own packages write. Whether --untrusted gates that path was
#     never established (64-RESEARCH.md records the reading as UNVERIFIED), and
#     with no binary here the question does not arise. That is a stronger
#     guarantee than an answer would have been. biblatex runs on
#     backend=bibtex, which tectonic executes inside its own process.
#     See 64-CONTEXT.md D-07. This absence is the feature; do not "fix" it.
#
#   texlive, tlmgr, any TeX distribution.  The bundle below IS the
#     distribution, and it is a closed file set. A second source of TeX files
#     would give a document somewhere else to resolve from.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      poppler-utils \
 && rm -rf /var/lib/apt/lists/*

# Pinned tectonic 0.17.0, static musl, per arch. Both digests were verified by
# DOWNLOADING each tarball and running shasum against it on 2026-09-04 -- never
# taken from a published checksum page, which is the rule
# Dockerfile.pandoc-worker states for the same reason.
ARG TECTONIC_VERSION=0.17.0
ARG TARGETARCH
ARG TECTONIC_SHA256_AMD64=8533d07f9ccbd7a65824b9e0459041bca34af1eb33daba48f59215593753a3b7
ARG TECTONIC_SHA256_ARM64=b10954a95404f3ab2328d2fa59a5ebab8e657f893fab096f98be8db7c0c979b8
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) TEC_ARCH=x86_64;  TEC_SHA256="${TECTONIC_SHA256_AMD64}" ;; \
      arm64) TEC_ARCH=aarch64; TEC_SHA256="${TECTONIC_SHA256_ARM64}" ;; \
      *) echo "FATAL: unsupported TARGETARCH '${TARGETARCH}' -- only amd64/arm64 are pinned" >&2; exit 1 ;; \
    esac; \
    TARBALL="tectonic-${TECTONIC_VERSION}-${TEC_ARCH}-unknown-linux-musl.tar.gz"; \
    curl -fL --retry 5 --retry-delay 5 -o "/tmp/${TARBALL}" \
      "https://github.com/tectonic-typesetting/tectonic/releases/download/tectonic%40${TECTONIC_VERSION}/${TARBALL}"; \
    echo "${TEC_SHA256}  /tmp/${TARBALL}" | sha256sum -c -; \
    tar -xzf "/tmp/${TARBALL}" -C /usr/local/bin tectonic; \
    chmod 0755 /usr/local/bin/tectonic; \
    rm -f "/tmp/${TARBALL}"

# The bundle: a closed TeX file set, baked in. THIS IS TEX-02.
#
# Without it tectonic falls back to relay.fullyjustified.net and fetches what
# the document asks for -- which means a client's \usepackage decides what this
# container downloads. internal/convert/tectonic.go refuses to run when the
# path is unset for that reason rather than falling back.
#
# 122 MiB, built by scripts/tectonic-bundle-build.sh from a digest-pinned
# TeXLive tree and published as a release of this repository. Provenance,
# including the two builds that were wrong, is in
# deploy/tectonic-bundle/PROVENANCE.md.
# THE BUNDLE COMES FROM THE BUILD CONTEXT, NOT FROM A URL, AND HERE IS WHY.
#
# The first version curl'd it from this repository's release. That fails with a
# 404: the repository is PRIVATE, so the asset is not anonymously reachable, and
# making the build authenticate would put a credential into every image build --
# including CI's, and including anyone reproducing the image later.
#
# So the artifact is fetched out of band, by a human or a job that already has
# credentials, and placed beside this Dockerfile:
#
#     scripts/tectonic-bundle-fetch.sh
#
# That script is not a convenience wrapper -- it is the fix for what this
# comment used to leave implicit. Until PR #16 the instruction lived only here,
# as prose, so a fresh checkout (and CI's own bake job) failed with
# `"/deploy/tectonic-bundle/octoconv-bundle.ttb": not found` and no hint. A
# documented manual step that nothing runs is a step that does not happen. CI
# now runs it before bake; `docker compose build tectonic-worker` still needs it
# run once by hand.
#
# The script reads the digest below rather than carrying its own copy, and
# verifies the download before the build starts -- so a truncated asset is
# reported as a truncated asset, not as a checksum failure three layers in.
#
# The pin does not weaken: the digest is verified HERE, against the bytes that
# actually land in the image, which is the property that matters. A URL would
# have proved only that something at that URL matched.
ARG TECTONIC_BUNDLE_SHA256=f42d5eed010a826548d346e3161df4b7b0237cad66ac316221c09a3b311502ff
COPY deploy/tectonic-bundle/octoconv-bundle.ttb /opt/tectonic/octoconv-bundle.ttb
RUN set -eu; \
    echo "${TECTONIC_BUNDLE_SHA256}  /opt/tectonic/octoconv-bundle.ttb" | sha256sum -c -; \
    chmod 0444 /opt/tectonic/octoconv-bundle.ttb

COPY --from=build /out/tectonic-worker /usr/local/bin/tectonic-worker

# Runtime assertions, in the spirit of pandoc-worker's version guard: a SHA
# check proves the bytes, not that the right thing landed in this stage.
#
# The second one is the interesting assertion. It fails the BUILD if a biber
# binary is ever present, so the guarantee above stops depending on nobody
# adding one -- an absence that nothing checks is an absence that returns.
RUN /usr/local/bin/tectonic --version | grep -q "Tectonic 0\.17\." \
 || { echo "FATAL: runtime tectonic does not report the pinned 0.17.x version" >&2; exit 1; }
RUN if command -v biber >/dev/null 2>&1 || command -v tectonic-biber >/dev/null 2>&1; then \
      echo "FATAL: a biber binary is present in this image. It must not be (64-CONTEXT.md D-07):" >&2; \
      echo "       tectonic launches biber as an external program with an argv taken from a file" >&2; \
      echo "       the document's own packages write, and whether --untrusted gates that path was" >&2; \
      echo "       never established. The guarantee here is that there is nothing to launch." >&2; \
      exit 1; \
    fi

# A WRITABLE CACHE, AND THE PRE-WARM THAT FILLS IT.
#
# Found by running, not by reading: with USER nobody and no cache directory,
# EVERY job fails with
#
#     error: cannot write format file texput.fmt
#     caused by: Permission denied (os error 13)
#
# Tectonic compiles the LaTeX format on first use and caches it. nobody has no
# home, so the cache resolves somewhere unwritable and tectonic falls back to
# the working directory -- which the worker owns but which is a fresh temp dir
# per job, so even when it succeeds the format would be rebuilt every single
# time. Passing -b does not avoid this: the bundle supplies the SOURCES, the
# format is the compiled artifact built from them.
#
# So: a real directory, owned by nobody, and warmed at build time. The warm-up
# is not an optimisation detail -- an unwarmed cache puts format compilation
# inside the first job's clock, which is the number 64-05 measures.
ENV TECTONIC_CACHE_DIR=/var/cache/tectonic
RUN mkdir -p /var/cache/tectonic && chown nobody:nogroup /var/cache/tectonic
RUN set -eu; \
    d=$(mktemp -d); chown nobody:nogroup "$d"; \
    printf '\\documentclass{article}\\begin{document}warm\\end{document}\n' > "$d/warm.tex"; \
    su nobody -s /bin/sh -c "cd $d && TECTONIC_CACHE_DIR=/var/cache/tectonic \
      tectonic -X compile $d/warm.tex --untrusted -b /opt/tectonic/octoconv-bundle.ttb -o $d" \
      > /tmp/warm.log 2>&1 || { echo 'FATAL: the pre-warm compile failed:' >&2; cat /tmp/warm.log >&2; exit 1; }; \
    test -s "$d/warm.pdf" || { echo 'FATAL: pre-warm produced no PDF' >&2; exit 1; }; \
    rm -rf "$d" /tmp/warm.log
# An empty cache here means the two RUNs above ran and achieved nothing, and
# every job would silently pay for format compilation. Assert on the artifact,
# not on the commands having exited 0.
RUN test -n "$(find /var/cache/tectonic -name '*.fmt*' -print -quit)" \
 || { echo 'FATAL: no compiled format in TECTONIC_CACHE_DIR after the pre-warm.' >&2; \
      find /var/cache/tectonic -type f | head -20 >&2; exit 1; }

# Run unprivileged: this worker executes a TeX interpreter over a document that
# is a PROGRAM, which is a stronger reason than any other worker here has.
USER nobody

ENV TECTONIC_BUNDLE_PATH=/opt/tectonic/octoconv-bundle.ttb

ENTRYPOINT ["/usr/local/bin/tectonic-worker"]
