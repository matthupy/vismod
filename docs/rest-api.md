---
title: REST intake
nav_order: 10
---

# Submitting jobs over HTTP

`vismod serve` exposes one endpoint — `POST /jobs` — on `intake_addr`.
It accepts a local file path or, once you enable it, an allow-listed
`https` URL.

⚠️ **This intake is dev/demo scope.** It has **no authentication, no
authorization, and no per-caller rate limiting**, and it binds only when
`intake_addr` is set (default `127.0.0.1:8080`, loopback-only). For
production, put your own authenticated API in front of it and keep
`intake_addr` on loopback or a private network — or enqueue onto Redis
directly and leave `intake_addr` unset. Read
[SECURITY.md](https://github.com/matthupy/vismod/blob/main/SECURITY.md)
before exposing it anywhere.

## The request/response model, in one paragraph

`POST /jobs` is **asynchronous**. It validates the job, enqueues it, and
returns `202 Accepted` with a `job_id`. **The verdict is not in the
response.** A worker picks the job up, scans it, and writes the result
envelope to every configured `output.sinks` entry (stdout, a JSONL file,
a webhook). You read the verdict from a sink, correlated by `job_id` —
see [Result envelope](result-envelope.md). Any example that appears to
return a verdict from `POST /jobs` is wrong about how vismod works.

## Request body

```json
{"kind":"file","ref":"/data/clip.mp4","media_type":"video",
 "workflows":["interval","keyframe"],"dedup_threshold":8,
 "metadata":{"ticket":"T-4417","tenant":"acme"}}
```

| Field | Required | Meaning |
|---|---|---|
| `kind` | yes | `"file"` or `"url"`. Anything else is `400` |
| `ref` | yes | `file`: a path (resolved to absolute). `url`: an allow-listed `https` URL |
| `media_type` | no | `"image"` or `"video"`. Inferred from the extension when omitted (`.mp4 .mov .mkv .webm .avi .m4v .mpg .mpeg .ts` → video, everything else → image). For a url the extension is read from the **path only**, since a query string can carry an extension that is not the asset's |
| `workflows` | no | Extraction workflows for video, any number; frames are the **union**. Names must exist in config: `scene-detect`, `keyframe`, `interval` by default. Omitted = `ffmpeg.default_workflow` |
| `dedup_threshold` | no | Per-job override of `frames.dedup`: `0`–`64` enables at that Hamming distance, `-1` disables, omitted inherits config |
| `metadata` | no | An opaque JSON **object** echoed back verbatim on the result envelope, for correlating a verdict with your own records. vismod never interprets it, and it can never affect a verdict. Max 4096 bytes once compacted. Not written to the audit log — **do not put secrets in it** |

| Response | When |
|---|---|
| `202` + `{"job_id":"job-…"}` | Accepted and queued |
| `400` | Bad JSON, missing `ref`, unknown `kind`, unknown workflow, out-of-range `dedup_threshold`, or a url that fails validation, or metadata that is not a JSON object or exceeds 4096 bytes compacted |
| `503` + `Retry-After: 30` | Intake paused by backpressure or by the operator, or the queue / dead-letter queue is full. **Retry — this is a signal, not a drop** |
| `500` | Queue failure |

Request bodies are capped at 1 MiB. Payloads carry refs only; **never
POST media bytes to this endpoint** — there is no field for them.

## Scanning from a URL

### 1. Nothing to enable

`kind:"url"` works with the config you already have. Start the worker and
POST a public `https` URL:

```sh
vismod serve -c config.yaml
```

In production, narrow the destinations a job can name:

```yaml
source:
  url:
    allow_hosts: ["media.example.com"]   # exact hostnames; no wildcards
    max_bytes: 268435456                 # 256 MiB, enforced on bytes read
    timeout: 60s
    max_attempts: 3
    allowed_media_types: [video/mp4, image/jpeg, image/png, image/webp]
```

Empty `allow_hosts` means any host. Either way, private, loopback,
link-local (cloud metadata) and CGNAT addresses are denied at connect
time, so a job cannot point vismod at your own infrastructure.

### 1b. Scanning media you serve yourself

To scan from a media server on the machine or LAN running vismod — say
`python3 -m http.server 8000` on macOS/Linux or `py -m http.server 8000`
on Windows, reached from a container as `host.docker.internal` — name
that host explicitly:

```yaml
source:
  url:
    allow_private_hosts: ["host.docker.internal"]
```

Only hostnames on that list may resolve into loopback / RFC 1918 / ULA
space, and only they may be reached over plain `http`. The
instance-metadata ranges stay denied for them too, and every other host
keeps the full deny-list. A host here does not also need to be in
`allow_hosts`.

`host.docker.internal` is provided by Docker Desktop on Windows and
macOS. On plain Linux, add
`extra_hosts: ["host.docker.internal:host-gateway"]` to the service
(Docker Engine 20.10+), or just name the host's LAN address instead. See
[deploy/compose/README.md](../deploy/compose/README.md) for the worked
version.

The full rule set and the reasoning are in
[SECURITY.md](https://github.com/matthupy/vismod/blob/main/SECURITY.md).

### 2. Submit an image

macOS / Linux:

```sh
curl -sS -X POST http://127.0.0.1:8080/jobs \
  -H 'content-type: application/json' \
  -d '{"kind":"url","ref":"https://media.example.com/photo.jpg"}'
# {"job_id":"job-1754160000000000000"}
```

Windows PowerShell:

```powershell
$body = @{ kind = 'url'; ref = 'https://media.example.com/photo.jpg' } |
    ConvertTo-Json
Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8080/jobs' `
    -ContentType 'application/json' -Body $body
# job_id
# ------
# job-1754160000000000000
```

Use `Invoke-RestMethod`, not `curl` — Windows PowerShell aliases `curl`
to `Invoke-WebRequest`, whose `-X`/`-d` flags mean something else.

### 3. Submit a video, with explicit workflows

Frames are the **union** of the named workflows, deduplicated before they
consume scan budget:

```sh
curl -sS -X POST http://127.0.0.1:8080/jobs \
  -H 'content-type: application/json' \
  -d '{"kind":"url","ref":"https://media.example.com/clip.mp4",
       "media_type":"video","workflows":["scene-detect","keyframe"],
       "dedup_threshold":8}'
```

```powershell
$body = @{
    kind            = 'url'
    ref             = 'https://media.example.com/clip.mp4'
    media_type      = 'video'
    workflows       = @('scene-detect', 'keyframe')
    dedup_threshold = 8
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8080/jobs' `
    -ContentType 'application/json' -Body $body
```

A presigned URL works the same way — pass it whole, query string
included. What gets **recorded** is only scheme+host+path, plus
`ref_digest` (SHA-256 of the full URL) so the verdict stays traceable.

### 4. Read the result back

The verdict is in the sinks, not in the HTTP response. With a file sink:

```yaml
output:
  sinks:
    - type: file
      path: results.jsonl
```

macOS / Linux:

```sh
# wait for the envelope for a specific job, then read its verdict
grep -m1 '"job_id":"job-1754160000000000000"' results.jsonl \
  | jq '{job: .job_id, source: .source, verdict: .result.overall.verdict}'
```

```json
{"job": "job-1754160000000000000",
 "source": {"kind":"url","ref":"https://media.example.com/clip.mp4",
            "ref_digest":"7b1f…","media_type":"video"},
 "verdict": "flag"}
```

Windows PowerShell:

```powershell
Get-Content results.jsonl |
    ForEach-Object { $_ | ConvertFrom-Json } |
    Where-Object { $_.job_id -eq 'job-1754160000000000000' } |
    ForEach-Object {
        [pscustomobject]@{
            job_id  = $_.job_id
            ref     = $_.source.ref
            verdict = $_.result.overall.verdict
        }
    }
```

(Convert one line at a time: Windows PowerShell 5.1's
`ConvertFrom-Json` joins piped input into a single string, which is not
valid JSON for a multi-line JSONL file.)

For a push model instead of polling a file, add a `webhook` sink and the
envelope is POSTed to you as each job finishes. Sink guarantees, and
where idempotency stops, are in [Result envelope](result-envelope.md).

`verdict:"error"` means could-not-evaluate — a provider outage, an
unreadable asset, or a failed fetch — and routes to human review. It is
never a silent `allow`.

## Scanning the same URL through several vendors

**One process runs exactly ONE vendor.** `adapter.name` is chosen at
startup and there is no fan-out flag; scanning the same asset through
Microsoft and Hive means running **two instances**, each with its own
config, its own `adapter.name`, its own `intake_addr` port, its own audit
log, and its own sinks — and submitting the job to each. Correlate the
two results yourself, on your own identifier or on `source.ref_digest`.
Scores are **not comparable across vendors**; see
[MODEL_LIMITATIONS.md](https://github.com/matthupy/vismod/blob/main/MODEL_LIMITATIONS.md)
before you treat two verdicts as one number.

## Why a url job was rejected

`400` responses name the rule (`bad request: ` + the message below,
fetcher rules additionally prefixed `fetch: `). Error text is built from
the **redacted** URL, so a query string is never echoed back to the
caller or into an access log.

| Message | Cause |
|---|---|
| `url scheme must be https, got "http"` | Any scheme but `https`, unless the host is in `source.url.allow_private_hosts` |
| `url must not contain userinfo — credentials are env-only` | `https://user:pass@host/…` |
| `host "…" is not in source.url.allow_hosts` | Exact-match miss against a populated `allow_hosts` (a subdomain of an allowed host is still a miss) |
| `url is not parseable` / `url has no host` | Malformed input |

Failures that can only be seen once the fetch runs — a denied address, a
redirect, an oversize body, a disallowed `Content-Type`, an HTTP error
from the origin — happen in the worker, not at intake. Those end as
`verdict:"error"` with a dead-letter, and show up in
`vismod_fetch_failures_total{reason=…}` (`rejected_url`,
`denied_address`, `redirect`, `oversize`, `media_type`, `http_status`,
`timeout`, `other`).

Validation deliberately runs twice — here, and again in the fetcher —
because with `queue.driver: redis` a job can be enqueued without ever
passing through this endpoint.
