# vismod — one image, both modes:
#   worker:  docker run <img>                      (default CMD "serve")
#   one-shot: docker run -v /data:/data <img> scan /data/clip.mp4
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/vismod/vismod/internal/cli.Version=${VERSION} -X github.com/vismod/vismod/internal/cli.Commit=${COMMIT}" \
    -o /out/vismod ./cmd/vismod

# Runtime MUST include ffmpeg + ffprobe (frame extraction is a hard
# external-binary dependency), so distroless/static is insufficient.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && ffmpeg -version >/dev/null && ffprobe -version >/dev/null

# Non-root. /home/vismod is the working dir (audit log default path);
# /tmp holds the per-job frame-extraction WorkDirs. Both are declared
# volumes so a read-only-rootfs deployment (recommended) can mount
# writable tmpfs/emptyDir over them.
RUN useradd --system --uid 10001 --create-home vismod
USER vismod
WORKDIR /home/vismod
ENV TMPDIR=/tmp
VOLUME ["/tmp", "/home/vismod"]

COPY --from=build /out/vismod /vismod

# Liveness probe for docker/compose; Kubernetes should use httpGet
# /healthz + /readyz probes directly (readiness carries backpressure).
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/vismod", "healthcheck"]

# SIGTERM triggers graceful drain (stop intake, finish in-flight within
# queue.drain_timeout, leave the rest durable-unacked in Redis).
STOPSIGNAL SIGTERM

ENTRYPOINT ["/vismod"]
CMD ["serve"]
