---
title: Configuration and environment
nav_order: 3
---

# Configuration and environment

vismod reads configuration from three layers, lowest precedence first:

1. **Built-in defaults** — compiled in, enough to boot nothing useful on
   their own.
2. **A yaml file** — `-c config.yaml`. This is the real configuration
   surface; [`config.example.yaml`](https://github.com/matthupy/vismod/blob/main/config.example.yaml)
   is the annotated version of every key.
3. **`VISMOD_*` environment variables** — an overlay on top of the file,
   plus the only place secrets are ever read from.

## The env overlay

A yaml key maps to an environment variable by uppercasing it and
replacing dots with underscores, under the `VISMOD` prefix:

| yaml key | environment variable |
|---|---|
| `queue.workers` | `VISMOD_QUEUE_WORKERS` |
| `queue.redis.addr` | `VISMOD_QUEUE_REDIS_ADDR` |
| `ffmpeg.max_frames` | `VISMOD_FFMPEG_MAX_FRAMES` |
| `adapter.options.endpoint` | `VISMOD_ADAPTER_OPTIONS_ENDPOINT` |

**The overlay can only override keys your yaml already sets.** This is
the part that surprises people. Setting `VISMOD_QUEUE_WORKERS=16` changes
the worker count if and only if `queue.workers` appears in the file; if
the key is absent, the variable is ignored and the built-in default
stands. There is consequently no env-only configuration — a process with
no config file comes up with an empty adapter name and fails fast with
`unknown adapter ""`, which is the intended behaviour rather than a bug
to work around. Mount or pass a file.

The practical consequence: put every key you intend to tune per
environment in the yaml, even at its default value, so the variable has
something to override.

## Secrets

Secrets are the exception to the rule above. They are read straight from
the environment, never through the yaml layer, so they work whether or
not any related key appears in the file — and they can never end up in a
config file, an image layer, or a git history.

| Variable | What it is | When it's required |
|---|---|---|
| `VISMOD_MICROSOFT_API_KEY` | Azure AI Content Safety key | `adapter.name: microsoft` with `auth_mode: key` |
| `VISMOD_MICROSOFT_ACCESS_TOKEN` | Static Entra bearer token | `adapter.name: microsoft` with `auth_mode: entra` |
| `VISMOD_HIVE_API_TOKEN` | Hive API token | `adapter.name: hive` |
| `VISMOD_QUEUE_REDIS_PASSWORD` | Redis `AUTH` password | Only if your Redis requires auth |
| `VISMOD_UI_USER` / `VISMOD_UI_PASSWORD` | Operator dashboard basic-auth credentials | `ui.auth: basic` |

Two adapters take no secret variable. `google` authenticates with
Application Default Credentials — `GOOGLE_APPLICATION_CREDENTIALS`, or
whatever ADC resolves on the host — and `shieldgemma` is an endpoint you
run yourself with no credential at all. A `shieldgemma` endpoint URL
carrying userinfo is rejected at boot, on the same principle: credentials
do not go in yaml.

Missing a required secret is a boot failure with the variable named in
the error, not a runtime surprise on the first job.

The webhook sink has no credential of its own today. Its URL must not
carry userinfo, and there is no header-token setting — if you need an
authenticated destination, put something in front of it.

## Using a `.env` file

Nothing in vismod reads `.env` directly; the binary only sees process
environment. Docker Compose is what loads the file, via `env_file: .env`
on each service, which is why the compose stack is where `.env` shows up:

```sh
cp deploy/compose/env.example .env
$EDITOR .env                      # set VISMOD_MICROSOFT_API_KEY
```

`.env` is gitignored, and so is `deploy/compose/config.compose.yaml`.
Keep it that way — the split exists so that the file you commit and the
file holding your keys are never the same file.

Running the binary directly, export the variables instead:

```sh
export VISMOD_MICROSOFT_API_KEY=<key>
./vismod -c config.yaml serve
```

For a real deployment, prefer whatever your platform gives you for
secrets — Kubernetes secrets mounted as env, a cloud secret manager —
over a file on disk. See
[deploy/README.md](https://github.com/matthupy/vismod/blob/main/deploy/README.md).

## What's excluded from the config hash

Every result envelope carries a `config_hash` over the verdict-affecting
configuration: adapter name, model version, and the resolved threshold
map. Secrets, log level, and network addresses are deliberately excluded,
so rotating a key or moving a port doesn't invalidate the audit trail's
continuity — but retuning a threshold does, which is the point. See
[Audit log](audit-log.md).

## Related

- [`config.example.yaml`](https://github.com/matthupy/vismod/blob/main/config.example.yaml)
  — every key, annotated
- [Supported models](models.md) — per-adapter auth modes
- [Running in Docker](docker.md) — the required config mount
- [SECURITY.md](https://github.com/matthupy/vismod/blob/main/SECURITY.md)
  — trust boundaries and why secrets are env-only
