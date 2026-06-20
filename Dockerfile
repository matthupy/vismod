# syntax=docker/dockerfile:1

# vismod — multi-stage image (§I). One image, both modes (serve / scan).
#
# 🔴 BUILD CONTEXT = PARENT DIRECTORY of vismod, NOT the vismod dir itself.
# go.mod declares `replace github.com/matthupy/videosift => ../videosift`
# (§B: videosift is co-developed, tracked-latest, never pinned), so the sibling
# checkout must be inside the build context. Build from the directory that holds
# BOTH repos as siblings (the same layout CI uses):
#
#   parent/
#     vismod/      <- this Dockerfile
#     videosift/   <- git clone https://github.com/matthupy/videosift
#
#   docker build -f vismod/Dockerfile -t vismod:dev parent/

# ---- Stage 1: builder ------------------------------------------------------
# Pin to satisfy videosift's `go 1.26.3` toolchain (§B/§I) — not a loose 1.x.
FROM golang:1.26-bookworm AS builder

WORKDIR /src

# Copy both modules (the replace target must be present to resolve).
COPY videosift/ ./videosift/
COPY vismod/ ./vismod/

WORKDIR /src/vismod

# Warm the module cache (build context already carries the local replace).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# Static binary: CGO off (no libc at runtime), trimmed + stripped.
# videosift still execs ffmpeg/ffprobe at runtime — bundled in stage 2.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags='-s -w' \
        -o /out/vismod ./cmd/vismod

# ---- Stage 2: runtime ------------------------------------------------------
# 🔴 MUST bundle ffmpeg+ffprobe — videosift execs them (§B/§I). distroless/static
# is insufficient. debian-slim matches the builder libc (CGO_ENABLED=0 makes the
# musl/glibc choice irrelevant, but slim keeps ffmpeg packaging simple).
FROM debian:bookworm-slim AS runtime

# ffmpeg provides both ffmpeg and ffprobe. Clean apt lists to keep the image lean.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Non-root runtime user; owns the ephemeral frame workdir.
RUN useradd --system --uid 10001 --home-dir /home/vismod --create-home vismod \
    && mkdir -p /var/lib/vismod/frames \
    && chown -R vismod:vismod /var/lib/vismod

COPY --from=builder /out/vismod /vismod

# frames.workdir: a writable, non-root-owned, ephemeral path so a read-only
# rootfs container can still create the videosift WorkDir. Declare it a VOLUME
# (override with tmpfs/emptyDir in production).
ENV VISMOD_FRAMES_WORKDIR=/var/lib/vismod/frames
VOLUME ["/var/lib/vismod/frames"]

USER vismod

# Liveness: hit /healthz on metrics.addr (default :9090).
EXPOSE 9090
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/vismod", "healthcheck"]

# Graceful drain on SIGTERM (§D.3).
STOPSIGNAL SIGTERM

# One image, both modes: default to serve; one-shot via
#   docker run <img> scan /data/clip.mp4
ENTRYPOINT ["/vismod"]
CMD ["serve"]
