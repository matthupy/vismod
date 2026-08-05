---
title: Running in Docker
nav_order: 5
---

# Running in Docker

One image, both modes. The `Dockerfile` bundles `ffmpeg`/`ffprobe`, runs
as a non-root user, and defaults to `serve`; `scan` is the same binary
with a different argument.

```sh
docker build -t vismod .
```

## A config file is required

There is no usable env-only configuration, and this trips people up more
than anything else about the image. The `VISMOD_*` overlay only overrides
keys the yaml already sets, so a container with no mounted config comes
up with an empty adapter name and fails fast with `unknown adapter ""`.
That is the intended behaviour — vismod would rather refuse to start than
guess which model you meant — but it means the mount is not optional.

```sh
# one-shot scan (exit code: 0 allow, 1 flag/block, 2 error)
docker run -e VISMOD_MICROSOFT_API_KEY -v "$PWD:/data" \
  vismod scan -c /data/config.yaml /data/clip.mp4

# worker — :9090 serves /metrics /healthz /readyz
docker run -e VISMOD_MICROSOFT_API_KEY -p 9090:9090 -v "$PWD:/data" \
  vismod serve -c /data/config.yaml
```

Credentials stay env-only. They are never read from yaml, so they never
end up in an image layer or a mounted file you forgot about.

## Publishing the intake

`intake_addr` defaults to `127.0.0.1:8080`. Inside a container that
address is reachable only from within the container, so `-p 8080:8080`
appears to work and then nothing answers. To actually publish it, set the
address to `0.0.0.0:8080` in your config and publish the port.

Do that deliberately: the dev intake has **no authentication**. Read
[SECURITY.md](https://github.com/matthupy/vismod/blob/main/SECURITY.md)
before exposing it beyond localhost, and see
[REST intake](rest-api.md) for what the endpoint accepts.

## Multi-replica and production

The [compose stack](https://github.com/matthupy/vismod/blob/main/deploy/compose/README.md)
runs two workers against a durable Redis queue with Prometheus and
Grafana wired up — the fastest way to see the image behave like a real
deployment. For Kubernetes, KEDA and HPA, see
[deploy/README.md](https://github.com/matthupy/vismod/blob/main/deploy/README.md).

One constraint carries into every multi-replica setup: **each replica
needs its own audit volume.** The audit log is a hash chain written by a
single process; two replicas sharing one volume corrupt it silently.
