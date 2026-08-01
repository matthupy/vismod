# Build Prompt — Open-Source Visual Content Moderation Pipeline (`vismod`)

> **How to use this document.** This is a self-contained build prompt for Fable 5 (a senior Go engineer).
> **§A–§C are research-anchored constraints** — treat as ground truth; do not re-derive them. Where a fact
> is marked **[verify]**, confirm it against the vendor's *current* live docs during the relevant milestone
> before relying on it. **§D–§L are build instructions.** Work **prototype-first**: milestone M0 (§H) must
> run end-to-end on day one with **no credentials and no network**, using interface fakes. Then iterate in
> small, tested, `main`-green steps. **When anything here conflicts with a prior assumption, this document
> wins.**
>
> Default identifiers (rename freely): module `github.com/<org>/vismod`, binary `vismod`.

---

## ROLE & MISSION

You are a senior Go engineer building an **open-source visual content moderation pipeline**. It scans
**images and video** for harmful content using a pluggable visual-moderation model, normalizes wildly
different provider outputs into **one common scoring schema**, and runs both as a **one-shot CLI** and a
**long-running containerized worker** that scales horizontally under load.

The project is a **public good with no commercial goals**, intended for trust & safety organizations and
smaller platforms that lack in-house moderation infrastructure. Optimize for: **a working prototype as fast
as possible**, then **fast, well-tested iteration**. Correctness, safety guardrails, and auditability are
first-class — this tool may encounter illegal content, and a careless design causes real-world harm.

---

## OPERATING PRINCIPLES

1. **Prototype-first, credential-free.** Get the full pipeline running end-to-end before integrating any
   real provider. The prototype and the entire test suite run with **no network and no credentials** via
   interface fakes (`fakeModerator`, `fakeFrameSource`). **There is no `stub` adapter in the shipped
   registry** — credential-free running is a test/harness concern, not a shipped model.
2. **Code to interfaces, swap implementations.** Every external concern (moderation model, queue, frame
   source, result sink, hash matcher, review diverter) sits behind a Go interface in its own package and
   swaps via config with **zero call-site changes**. The interface must be rich enough that the swap is
   genuinely behavior-preserving (see §D.6 Queue).
3. **Prefer stdlib and official packages; add a dependency only when it earns its place.** Use `log/slog`,
   `context`, `os/exec`, `encoding/json`, `net/http` + `httptest`, `embed`, `golang.org/x/sync/errgroup`.
   The **blessed dependency set** is pre-justified in §D — do not expand it casually. The goal is to
   **decrease external dependencies**; every new import is a decision, not a reflex.
4. **Tests land with features.** Table-driven tests, interface fakes, golden files for normalization,
   `httptest` for provider clients (incl. retry/backoff/error-mapping). CI runs without secrets.
5. **Fail safe, never fail silent.** Moderation is security-critical. On any provider/frame/extraction
   failure, **never emit `allow`** — emit an `error` verdict and route to human review / dead-letter (§F.5).
6. **Small, reviewable steps.** Land each milestone (§H) as a coherent, tested unit. Keep `main` green.

---

# §A — FUNCTIONAL REQUIREMENTS

1. **Inputs:** `image` and `video`. For **video**, extract the most relevant frames via **direct FFmpeg**
   (§B), then moderate each frame as an image (unless the active adapter is video-native — §D.1).
2. **Three moderation vendors, modular by design:** ship **`microsoft`** (Azure AI Content Safety),
   **`google`** (Cloud Vision SafeSearch), and **`hive`** (thehive.ai) from v1. Providers have **wildly
   different output formats** → a **normalization layer** (§E) maps each into one model-agnostic schema.
   The schema is what makes vendors modular: adding a fourth vendor is one adapter package + golden tests,
   with zero pipeline changes.
3. **Runtime model selection:** Exactly **one** moderation model active per process, chosen from config at
   startup. **Restart-to-change is acceptable** — no hot-swapping.
4. **Per-adapter capabilities:** Adapters declare capabilities (e.g. `SupportsVideo`, `MaxImageBytes`). All
   adapters **must** support image input; video support is optional. When the active adapter is not
   video-native, the pipeline extracts frames (FFmpeg) and moderates them as images.
5. **Language & packaging:** **Go**, runnable as a **CLI** and as a **Docker container** (one binary,
   subcommands).
6. **Job management:** a **FIFO** job queue with a **multi-worker** design that **autoscales horizontally
   by input-queue size** (§D.6).
7. **Configuration focus:** ship a **default FFmpeg workflow set**, and support **user-created custom
   FFmpeg workflows** (§B). Configuration is a first-class, documented surface.
8. **Code storage:** **GitHub.**

---

# §B — FRAME EXTRACTION: DIRECT FFMPEG + CONFIGURABLE WORKFLOWS

Frame extraction is a **hard dependency on external `ffmpeg` + `ffprobe` binaries**, invoked via
`os/exec`. There is no third-party Go framing library. The design goal is **configuration-driven
extraction**: sensible defaults out of the box, plus user-authored custom workflows.

## B.1 — `FrameSource` interface (`internal/frames`)
```go
// Pipeline-owned Frame. Absolute path to a PNG on local disk.
type Frame struct { Index int; TimestampSec float64; Path string }

type FrameSource interface {
    // Returns extracted frames, a cleanup closure (idempotent), and an error.
    Frames(ctx context.Context, videoPath string) (frames []Frame, cleanup func() error, err error)
}
```
Real impl = **`FFmpegSource`**. Also provide a `fakeFrameSource` for tests.

**🔴 Lifecycle contract:** `FFmpegSource` creates and owns an **absolute** `WorkDir`, runs the selected
workflow to materialize PNGs there, and returns a `cleanup` closure that deletes that dir. The pipeline
**MUST `defer cleanup()` immediately after `Frames()` returns** (before any fan-out) so `WorkDir` is
deleted on **every** exit path — error, ctx-cancel, panic. `cleanup` is idempotent; its error is logged
but never changes the verdict. **Order per job:** `Frames()` → `defer cleanup` → fan-out+normalize+rollup
→ `Sink.Write` → **only then** ack/dead-letter.

**🔴 Fail-safe mapping (mandatory):** any FFmpeg/FFprobe error, a missing binary at runtime, or **zero
frames extracted from a video** is a **could-not-evaluate** condition → emit `Verdict=error` and
dead-letter. **Never treat zero-frames as clean/allow** (a static/looping harmful video must not pass by
producing no frames). Only an explicit, audited operator override (the §F.5 gated flag) may downgrade an
empty result — producing an operational **skip** (job acked, no verdict emitted), **not** a `Verdict`
value.

**🔴 Boot prerequisite:** `ffmpeg` and `ffprobe` must be on PATH (or explicit config paths). **Validate
once at boot** (§F.2) so a missing binary is a clear operator error, not a per-job failure. Docker must
bundle both (§I).

## B.2 — Configurable FFmpeg workflows (the configuration focus)

A **workflow** is a named, parameterized **FFmpeg argument-list template** — never a shell string. Config:

```yaml
ffmpeg:
  ffmpeg_path: ffmpeg          # default "ffmpeg"; resolved on PATH
  ffprobe_path: ffprobe
  default_workflow: scene-detect
  max_frames: 64               # REQUIRED > 0; hard cap on frames materialized per video (cost + disk bound)
  timeout: 120s                # per-extraction wall-clock timeout
  workflows:
    scene-detect:              # DEFAULT — one PNG per detected scene change
      description: "Extract frames on scene changes (select='gt(scene,threshold)')."
      args: ["-hide_banner","-nostdin","-y","-i","{{.Input}}",
             "-vf","select='gt(scene,0.4)',scale={{.MaxWidth}}:-1","-vsync","vfr",
             "-frames:v","{{.MaxFrames}}","{{.WorkDir}}/frame-%06d.png"]
    keyframe:                  # DEFAULT — extract I-frames only
      description: "Extract keyframes (I-frames) only."
      args: ["-hide_banner","-nostdin","-y","-skip_frame","nokey","-i","{{.Input}}",
             "-vsync","vfr","-frames:v","{{.MaxFrames}}","{{.WorkDir}}/frame-%06d.png"]
    interval:                  # DEFAULT — one frame every N seconds
      description: "Extract one frame every 2 seconds."
      args: ["-hide_banner","-nostdin","-y","-i","{{.Input}}",
             "-vf","fps=1/2,scale={{.MaxWidth}}:-1","-frames:v","{{.MaxFrames}}",
             "{{.WorkDir}}/frame-%06d.png"]
```

**Template model (approach A — arg-list templates with typed substitution).**
- Each workflow's `args` is a `[]string` rendered through Go `text/template`. **Allowed placeholders only:**
  `{{.Input}}`, `{{.WorkDir}}`, `{{.MaxFrames}}`, `{{.MaxWidth}}` (and any explicitly-documented additions).
  Unknown placeholders fail validation.
- The pipeline runs `exec.CommandContext(ctx, ffmpegPath, renderedArgs...)` — **arg slice, never a shell**,
  always with `-nostdin`. `ffprobe` is used first to read duration/dimensions where a workflow needs them.

**🔴 SECURITY GUARDRAILS (mandatory — ffmpeg + user input is an injection & SSRF surface):**
1. **No shell.** Only `exec.Command` with a rendered arg slice. Never `sh -c`, never string interpolation
   into a command line.
2. **`{{.Input}}` is bound to the current job's local file path** and is the **only** input. Users cannot
   inject a second `-i` or redirect input — a workflow whose rendered args contain an operator/`-i` beyond
   the templated one fails validation.
3. **Protocol allow-list.** Reject any workflow (at validate time) or rendered path that references FFmpeg
   remote/indirect protocols — `http:`, `https:`, `rtmp:`, `concat:`, `pipe:`, `subfile:`, `data:`, `tcp:`,
   `udp:`, `file:` with traversal, etc. **Only plain local paths** the pipeline owns are permitted. This
   blocks exfiltration and arbitrary-file reads via a crafted "workflow."
4. **Output confinement.** All outputs must land under the pipeline-owned `{{.WorkDir}}`; reject templates
   that write elsewhere. Enforce `max_frames` and `timeout` as hard caps.
5. **Boot + CLI validation.** `vismod workflows validate` and boot validation must: confirm ffmpeg/ffprobe
   present, parse every workflow template, reject forbidden tokens/placeholders, and dry-render each
   workflow against a synthetic input to prove it produces only in-`WorkDir` outputs.

**CLI:** `vismod workflows list` (names + descriptions), `vismod workflows validate` (gate).
**Docs:** a `docs/` section "Writing custom FFmpeg workflows" covering the placeholder set, the guardrails,
and worked examples. Document that custom workflows are an **operator-trust boundary**.

---

# §C — VENDOR ADAPTER CONTRACTS (three shipped adapters)

All three normalize into the §E schema, tagging every score with a `ScoreOrigin`. Registry never imports
adapters; each self-registers in `init()`; exactly one instantiated per process (§D.2).

## C.1 — `microsoft` — Azure AI Content Safety (Image) — research-verified
> **GA `api-version=2024-09-01`.** **No Go SDK** for the Content Safety data plane — call REST directly
> (`net/http` + `encoding/json`). Keep `api-version` a configurable const (~90-day deprecation cadence).

- **Endpoint:** `POST {endpoint}/contentsafety/image:analyze?api-version=2024-09-01`,
  `{endpoint} = https://<resource>.cognitiveservices.azure.com`. **Synchronous.**
- **Auth:** header `Ocp-Apim-Subscription-Key: <key>` **OR** Microsoft Entra ID / Managed Identity (OAuth2
  scope `https://cognitiveservices.azure.com/.default`). Support both.
- **Request:** `{"image":{"content":"<base64>"}}` (inline `content` only in v1 — see SSRF note). Optional
  `categories`, `outputType:"FourSeverityLevels"`.
- **Response (exact):** top-level has **only** `categoriesAnalysis`:
  `[{"category":"Hate","severity":0},{"category":"SelfHarm","severity":0},{"category":"Sexual","severity":0},{"category":"Violence","severity":2}]`.
  **No score / no decision field — you apply thresholds.**
- **Categories (image:analyze returns exactly 4):** `Hate`, `SelfHarm`, `Sexual`, `Violence`. (Do **not**
  include "Task Adherence" — it is not returned by `image:analyze`.)
- **🔴 Severity — IMAGE is the TRIMMED scale ONLY:** discrete `0,2,4,6` (Safe/Low/Medium/High).
  **Normalize `Score = severity / 6.0`, `ScoreOrigin="severity"`.**
- **Video:** not native → frames, per-image.
- **Limits:** max file **4 MB** (surface as `Caps.MaxImageBytes`); formats JPEG/PNG/GIF/BMP/TIFF/WEBP;
  configurable max dimension (default conservative 2048×2048).
- **Rate limits:** F0 = 5 RPS; S0 = 1000 req / 10s. No batch API. Backoff on `429` (`x-ms-error-code`).
- **🔴 SSRF/egress:** `blobUrl` (and any future `url`/`s3` source) is a remote-fetch vector — **v1 defaults
  to inline `content` only**; if ever enabled, require a host/scheme allow-list forbidding RFC1918 /
  `169.254.0.0/16` / `::1`. Document in `SECURITY.md`.
- **Mapping:** `Hate→HATE, Sexual→SEXUAL, Violence→VIOLENCE, SelfHarm→SELF_HARM`.
- **CSAM policy:** Azure must **not** be used to detect child-exploitation imagery — CSAM is the hash-match
  seam (§D.7), never the classifier.

## C.2 — `google` — Cloud Vision SafeSearch — **[verify against live docs at M1]**
- **Transport:** official Go SDK `cloud.google.com/go/vision/apiv1` (blessed dep). Auth via ADC /
  service-account (env-only, §F.1).
- **Categories:** `adult`, `spoof`, `medical`, `violence`, `racy`, each a **likelihood enum**:
  `UNKNOWN, VERY_UNLIKELY, UNLIKELY, POSSIBLE, LIKELY, VERY_LIKELY`.
- **Normalize** via a **configurable likelihood lookup**:
  `VERY_UNLIKELY→0.0, UNLIKELY→0.25, POSSIBLE→0.5, LIKELY→0.75, VERY_LIKELY→1.0`; **`UNKNOWN→Score=nil`**.
  `ScoreOrigin="likelihood_enum"`.
- **Mapping:** `adult→SEXUAL, racy→SUGGESTIVE_RACY, violence→VIOLENCE, medical→MEDICAL, spoof→SPOOF`.
  Document that `MEDICAL`/`SPOOF` are **not harm signals** — provenance carriers only.
- **Video:** SafeSearch is image-only → frames, per-image.

## C.3 — `hive` — thehive.ai Visual Moderation — **[verify against live docs at M1]**
- **Transport:** REST (`net/http` + `encoding/json`); token auth via env-only secret.
- **Output:** multi-head classes each with a **probability 0..1** (e.g. sexual/nudity, violence/gore,
  drugs, hate, weapons heads). Confirm exact head names + response envelope against current Hive docs
  before finalizing the mapping.
- **Normalize:** per-head positive-class probability → `Score`, `ScoreOrigin="probability"`. Map heads to
  the canonical enum; **any unmapped head → `OTHER`, preserving the raw label**. Never drop a head; never
  emit `0` for an unknown (use `nil`).
- **Video:** if Hive exposes a native video endpoint, it MAY implement `VideoModerator` (§D.1); otherwise
  frames, per-image.

**🔴 Cross-vendor rule (applies to all three):** `severity/6`, a likelihood bucket, and a head probability
are **not the same quantity**. Scores are **within-provider comparable only**; thresholds are
**per-adapter and NOT portable**. State this in §E and `MODEL_AND_HASH_LIMITATIONS.md`.

---

# §D — ARCHITECTURE (locked decisions)

**Module layout**
```
github.com/<org>/vismod
  cmd/vismod/main.go            # thin: calls cli.Execute()
  pkg/moderation/               # PUBLIC contract types external consumers bind to (no internal deps)
    types.go                    # Moderator, VideoModerator, NormalizedResult, Caps, Verdict, Category,
                                #   ScoreOrigin, CategoryResult, Image, HashMatcher
  internal/
    cli/                        # cobra: root (blank-imports adapters), scan, serve, adapters, workflows,
                                #   audit, version, healthcheck
    config/                     # viper loader + typed Config; env overlay; secrets env-only; ConfigHash
    moderate/
      registry.go               # AdapterConfig + map[string]Factory + Register/New; init self-registration
      adapters/{microsoft,google,hive}/
    frames/                     # FrameSource + FFmpegSource + workflow engine + fakeFrameSource
    queue/                      # Queue interface + memq (dev) + redisq (durable, autoscale-ready)
    pipeline/                   # HashMatcher pre-stage -> frames -> fan-out -> Moderator -> normalize
                                #   -> rollup -> Sink
    result/                     # Sink interface (stdout JSONL / file; webhook/DB later)
    audit/                      # append-only hash-chained decision log + verify
    observe/                    # slog, Prometheus /metrics, /healthz /readyz
    review/                     # Diverter interface (potential-CSAM frame divert seam)
    hashmatch/                  # HashMatcher impls (no-op default in v1; PDQ/TMK in v1.1)
    ui/                         # STRETCH: embedded web dashboard (net/http + embed)
```

**Blessed dependencies (the whole set — justify any addition):** `spf13/cobra` + `spf13/viper` (CLI +
daemon in one binary); `golang.org/x/sync/errgroup`; `prometheus/client_golang` (metrics);
`cloud.google.com/go/vision/apiv1` (Google adapter); a durable-queue backend for `redisq` (`hibiken/asynq`
or a `redis` client — pick one, justify). `microsoft` and `hive` adapters use **stdlib `net/http`** — no
SDK. Logging via `log/slog`. UI static assets via `embed`. **No other third-party deps without cause.**

### D.1 — `Moderator` (+ optional `VideoModerator`) — `pkg/moderation`
```go
type Moderator interface {
    Name() string
    AnalyzeImage(ctx context.Context, img Image) (NormalizedResult, error)
    Capabilities() Caps
    Close() error
}
// OPTIONAL second interface for a video-native provider. Pipeline type-asserts for it.
// Do NOT add AnalyzeVideo to Moderator later (that breaks every impl).
type VideoModerator interface {
    AnalyzeVideo(ctx context.Context, video Source) (NormalizedResult, error)
}
type Image struct { Bytes []byte; MIME string; Width, Height int; Meta map[string]string }
type Caps struct {
    SupportsVideo bool       // true => pipeline prefers AnalyzeVideo (if impl) over frame-by-frame
    MaxImageBytes int64      // pipeline pre-flights oversize images before AnalyzeImage
    Categories    []Category // canonical categories this adapter can emit
}
```
Dispatch: `if vm, ok := m.(VideoModerator); ok && m.Capabilities().SupportsVideo { vm.AnalyzeVideo(...) } else { frame-by-frame }`.

### D.2 — Adapter registry + `AdapterConfig` (`internal/moderate`)
```go
// PROVIDER-OPAQUE carrier. Lives in internal/moderate (carries secret wiring), NOT pkg/.
type AdapterConfig struct {
    Name    string
    Options map[string]any          // each adapter decodes into its OWN typed config inside its Factory
    Secret  func(key string) string // env-backed secret accessor (keeps API keys out of Options/yaml)
}
type Factory func(cfg AdapterConfig) (moderation.Moderator, error)
func Register(name string, f Factory)
func New(name string, cfg AdapterConfig) (moderation.Moderator, error) // unknown => fatal, lists registered
```
**Rule:** registry **never imports adapter packages**; adapters self-register via `init()`, pulled in by
blank import at the composition root (`internal/cli/root.go`). `New` instantiates **only** the one
configured factory ⇒ "exactly one model active per process." `vismod adapters` prints registry keys +
`Capabilities`. **No `stub` adapter is registered** — tests use `fakeModerator` (an in-package fake), not a
shipped adapter.

### D.6 — `Queue` + multi-worker autoscaling (FIFO; behavior-preserving swap)
```go
type Disposition int
const ( Ack Disposition = iota; Retry; DeadLetter ) // explicit handler outcome; both drivers honor identically

type Queue interface {
    Enqueue(ctx context.Context, j Job) (JobID, error)
    Start(ctx context.Context, handler func(context.Context, Job) (Disposition, error)) error
    QueueDepth(ctx context.Context) (int, error) // uniform across drivers — drives autoscaling
    Close(ctx context.Context) error             // graceful drain
}
type QueueConfig struct {
    Workers       int           // worker goroutines per replica (fixed pool per process)
    Buffer        int
    MaxRetries    int
    RetryBackoff  time.Duration
    DrainTimeout  time.Duration // graceful-drain budget for in-flight jobs
    JobTimeout    time.Duration // per-job processing timeout
    DeadLetterMax int           // DLQ depth cap; at capacity reject enqueues + alert (§F.5)
    DeadLetter    Sink          // where dead-lettered jobs go (must exist in the prototype)
}
type JobID string
type Job struct { ID JobID; Source Source; SubmittedAt time.Time }
```

**Autoscaling model — horizontal, queue-depth driven.** Each `serve` replica runs a **fixed worker pool**
of `queue.workers` goroutines. **Scale is achieved by scaling replicas**, driven by the exported
`vismod_queue_depth` metric:
- Export `vismod_queue_depth` from `Queue.QueueDepth` (uniform across drivers). This is the scaling signal.
- **Multi-replica requires `driver=redis`** (memq is single-process and non-durable). Ship a documented
  **KEDA `ScaledObject` example** (and an HPA-with-custom-metrics alternative) targeting queue depth — e.g.
  scale up when depth-per-replica exceeds a target, scale to a floor of 1. The prompt specs the **metric +
  scaling contract**, not the scaler itself.
- Document the tradeoff: replicas × per-process rate limiter can overshoot a vendor quota — see §F.3
  (Redis-backed shared limiter or a documented `global_limit / replicas` budget).

**Drivers.**
- **`memq` (dev/CLI):** buffered `chan Job` (**FIFO by construction**) + fixed worker pool. Implements a
  **real DLQ** + bounded in-memory retry honoring `MaxRetries`/`RetryBackoff`; job states in a
  mutex-guarded map. **Non-durable, at-most-once, single-process** — warn at boot / `/readyz` when
  `driver=memory && serve`. A crash loses enqueued + in-flight jobs. **Not for production intake.**
- **`redisq` (production, durable):** Redis-backed, **at-least-once**, durable across restarts, and the
  substrate for multi-replica autoscaling. Maps `Disposition` → retry / skip-retry / archive. **Payload
  hygiene:** job payloads in Redis (and any ops UI) **carry opaque IDs/refs, never media bytes**; the store
  must be access-controlled.

**🔴 FIFO is a property of the QUEUE — dequeue order = enqueue order.** Never order the pending set by a
sortable key (UUID/job-ID string) — lexicographic ordering silently starves jobs past the pivot. The
buffered channel gives arrival order for free; any other backing store MUST preserve it (insertion-ordered
structure or a monotonic enqueue sequence).

**Start order ≠ completion order.** FIFO governs dequeue/start only. With `>1` worker, completion order is
not guaranteed. Strict end-to-end ordering needs `workers=1` or per-key serialization. Document this.

**Behavior-preserving swap.** The same handler `Disposition` produces the same retry/DLQ outcome on `memq`
and `redisq`: `Ack`→success, `Retry`→bounded backoff then DLQ, `DeadLetter`→DLQ immediately. **At-least-once
⇒ idempotency required** — `Sink.Write` and the audit append dedupe per `JobID` so redelivery never
double-writes (§D.5/§G.5).

**Graceful drain.** Distinguish the worker-lifecycle ctx (cancels pulling **new** work) from the per-job
ctx. On shutdown: stop enqueues; stop pulling; in-flight jobs get `drain_timeout` to finish + `Sink.Write`
+ ack; jobs not done in time are **left unacked / requeued**, never acked-done, never silently dropped.
Buffered-but-unstarted `memq` jobs are logged at WARN.

### D.5 — `Source`, `Sink`, `ResultEnvelope`
```go
type Source struct { Kind string; Ref string; MediaType string } // Kind "file" (v1); "url"/"s3" later (SSRF allow-list)
type Sink interface { Write(ctx context.Context, env ResultEnvelope) error } // MUST be idempotent per JobID
type ResultEnvelope struct {
    JobID      JobID                        `json:"job_id"`
    Source     Source                       `json:"source"`
    ModelID    ModelIdentity                `json:"model_id"`
    Result     *moderation.NormalizedResult `json:"result,omitempty"`
    Error      string                       `json:"error,omitempty"`
    StartedAt  time.Time                    `json:"started_at"`
    FinishedAt time.Time                    `json:"finished_at"`
}
type ModelIdentity struct { Adapter, ModelVersion, ConfigHash string } // stamped on every job for audit
```
`ConfigHash` = SHA-256 over the canonicalized verdict-affecting config (adapter name + `ModelVersion` +
resolved per-category threshold map; exclude secrets/log level/addrs). `AssetID` = job's `Source.Ref`
(fallback `JobID`), **stamped by the pipeline after normalization** (the adapter leaves it empty). Prototype
`Sink`: JSON-lines to stdout and/or file, **idempotent per `JobID`**.

### D.6b — Concurrency, backpressure & per-frame failure
Each video job expands to many frames. **Do NOT cancel-on-first-error the moderation fan-out** — each frame
is an independent evidence sample. Bound parallelism with `errgroup.SetLimit(frames.concurrency)` (distinct
from any ffmpeg-internal concurrency); each per-frame task **captures-and-returns its own `{FrameResult,
err}` and returns `nil`** so one frame's error never cancels siblings. **Lazy decode:** read→decode→
`AnalyzeImage`→release inside each task so at most `frames.concurrency` decoded images are resident; never
pre-decode the whole slice. Peak disk = `frames.max_frames` PNGs. **Pre-flight** oversize images against
`Caps.MaxImageBytes` **before** the shared rate limiter's `Wait`. Per-job timeout via ctx; **panic recovery
in every worker handler** → `Verdict=error` → dead-letter, pool survives.

### D.7 — `HashMatcher` pre-stage seam (CSAM)
```go
type HashMatcher interface { Match(ctx context.Context, img Image) (HashMatch, error) }
type HashMatch struct { Matched bool; ListName string; Algo string } // binary list-membership, NOT a score
```
Runs **before the `Moderator`**. A match short-circuits to a `CSAM_HASH_MATCH` `CategoryResult`
(`ScoreOrigin="list_membership"`, `Score=nil`, `Flagged=true`) and **does not call the classifier**. **v1
ships a no-op default** (always `Matched:false`); the PDQ/TMK matcher is v1.1. Shipping the seam + category
+ schema fields in v1 is a hard requirement (§K).

---

# §E — NORMALIZATION LAYER (the common scoring contract)

**Canonical category taxonomy (typed enum):**
`SEXUAL, SUGGESTIVE_RACY, VIOLENCE, GORE_GRAPHIC, WEAPONS, SELF_HARM, HATE, DRUGS, MEDICAL, SPOOF,
CSAM_HASH_MATCH, OTHER`. `CSAM_HASH_MATCH` is reserved for §D.7. `MEDICAL`/`SPOOF` carry Google's
`medical`/`spoof` — document their provenance so consumers don't misread them as harm signals. **Fallback
discipline:** any provider label with no canonical mapping → `OTHER`, preserving the raw label in
`ProviderLabel`, carrying its `Score`. **Never drop a result.**

**Public Go types (`pkg/moderation`):**
```go
type Verdict string     // "allow" | "flag" | "block" | "error"
type Category string     // canonical enum above
type ScoreOrigin string  // "probability" | "confidence_pct" | "likelihood_enum" | "severity" | "list_membership"
type FrameStatus string  // "ok" | "error"

type CategoryResult struct {
    Category      Category    `json:"category"`
    ProviderLabel string      `json:"provider_label"`
    Score         *float64    `json:"score"`      // normalized 0..1; nil = unknown/unsupported/list-membership
    ScoreOrigin   ScoreOrigin `json:"score_origin"`
    Threshold     *float64    `json:"threshold"`  // flag_at boundary; nil for list_membership; block_at at rollup
    Flagged       bool        `json:"flagged"`    // (Score!=nil && Threshold!=nil && *Score>=*Threshold) OR list-match
    MatchType     string      `json:"match_type,omitempty"` // list_membership only, e.g. "pdq"
    MatchList     string      `json:"match_list,omitempty"` // list_membership only, e.g. "ncmec"
}
type FrameResult struct {
    TimestampSec *float64         `json:"timestamp_sec"` // nil for still images
    Status       FrameStatus      `json:"status"`
    Error        string           `json:"error,omitempty"`
    Categories   []CategoryResult `json:"categories"`
}
type OverallVerdict struct {
    Verdict     Verdict   `json:"verdict"`
    Flagged     bool      `json:"flagged"`
    TopCategory *Category `json:"top_category"`
    MaxScore    *float64  `json:"max_score"`  // nil when NO non-nil score exists (never collapse to 0.0)
    Confidence  *float64  `json:"confidence"` // nil when NO non-nil score exists
}
type NormalizedResult struct {
    SchemaVersion string          `json:"schema_version"` // set by NORMALIZER, not adapter
    Provider      string          `json:"provider"`
    ModelVersion  string          `json:"model_version"`
    MediaType     string          `json:"media_type"`     // "image" | "video"
    AssetID       string          `json:"asset_id"`
    Frames        []FrameResult   `json:"frames"`         // image => single frame, TimestampSec nil
    Overall       OverallVerdict  `json:"overall"`
    Raw           json.RawMessage `json:"raw,omitempty"`  // OPTIONAL + SANITIZED — never leak free-text/OCR/captions
}
```

**🔴 Nullable scalars serialize as JSON `null`, never omitted** (no `omitempty` on `Score`, `Threshold`,
`MaxScore`, `Confidence`, `TimestampSec`, `TopCategory`). `nil` score = could-not-evaluate; **never emit
`0` for unknown.**

**Score normalization → `[0,1]` (tag each with `ScoreOrigin`):** Microsoft `severity/6` (`severity`);
Google likelihood lookup, `UNKNOWN→nil` (`likelihood_enum`); Hive per-head probability (`probability`);
hash match `Score=nil` (`list_membership`).

**Decision & aggregation:**
- Per-frame independent: a provider/pre-flight error sets `Status="error"`, `Error`, empty `Categories`.
- Per-category configurable thresholds `flag_at`/`block_at` (or `thresholds.default`); `SEXUAL`/`CSAM`
  strictest.
- **Asset rollup** over `ok` frames: `Overall.Flagged` = any `CategoryResult.Flagged`; `MaxScore`/
  `Confidence` = max over **non-nil** scores (**nil if none**); `TopCategory` = category of that max.
  **Verdict precedence is STRICT: `block > error > flag > allow`.** Evaluate in order: **`block`** if any
  `ok` category has `*Score ≥ block_at` OR a list-membership match; else **`error`** if any frame
  `Status="error"`, zero `ok` frames exist, OR every score across all frames is `nil`; else **`flag`** if
  any `Flagged`; else **`allow`**. **Never `allow` while any frame errored.** Video default = any-frame;
  configurable "min flagged frames" / "N consecutive".
- Consumers must tolerate **unknown future `Category` as `OTHER`**. Additive fields/values = minor bump;
  remove/rename/meaning-change = major bump.

**Testing the normalizer:** capture each provider's raw JSON fixture → normalize → compare `*.golden`
(`-update` to regenerate). Ship worked input→`NormalizedResult` examples proving (1) non-emitted categories
are `nil`, not 0, and (2) the `OTHER`-fallback + nil discipline.

---

# §F — CONFIG, ERRORS, OBSERVABILITY

### F.1 — Config (`viper`)
Keys: `adapter.name` + `adapter.options`; `thresholds.{category}.{flag_at,block_at}` +
`thresholds.default.{flag_at,block_at}` (**per-adapter, not portable**; `thresholds.SEXUAL.potential_csam`
— §G.8); the Google likelihood lookup table; `ffmpeg.*` (§B.2: `default_workflow`, `workflows`,
`max_frames` REQUIRED > 0, `timeout`, binary paths); `frames.concurrency` (default 4); `queue.driver`
(`memory`|`redis`), `queue.workers`, `queue.buffer`, `queue.max_retries`, `queue.retry_backoff`,
`queue.drain_timeout`, `queue.job_timeout`, `queue.deadletter_max`, `queue.redis.addr`; `log.level`;
`metrics.addr`; `audit.*`; `ui.enabled` + `ui.addr` + `ui.auth`. **Secrets are env-only** (`VISMOD_`
prefix, `.`→`_`); **never in yaml.** Ship a fully annotated `config.example.yaml`.

### F.2 — Boot-time validation (fail fast)
Validate `ffmpeg`+`ffprobe` on PATH; **validate every FFmpeg workflow** (§B.2 guardrails); validate the
selected adapter's credentials. When `queue.driver=redis`, validate **Redis reachability (PING)** at boot
**and in `/readyz`** (Redis is the SPOF — an outage flips readiness to not-ready, never black-holes jobs).

### F.3 — Rate limiting & cost control
A token-bucket limiter is **owned by the single active `Moderator` and SHARED across all workers and all
per-job fan-out** — aggregate request rate = limiter rate regardless of `workers × frames.concurrency`.
Default to the adapter's known quota (Azure F0 = 5 RPS). `frames.max_frames` bounds per-video cost.
**Multi-replica:** a per-process limiter × N replicas overshoots quota × N — provide a Redis-backed shared
limiter **or** a documented `global_limit / replicas` budget.

### F.4 — Retry / error classification
**Retryable** (`429`, `5xx`, timeouts, transient net) → bounded backoff → dead-letter. **Terminal** (`4xx`
validation, unsupported/oversize media) → fail, no retry. **Panic/poison:** every handler under `recover` →
`Verdict=error` → dead-letter; never crash the pool. Cap retries so a deterministically-failing job lands
in the DLQ after K attempts. Surface provider error codes.

### F.5 — 🔴 Fail-safe policy (security-critical)
- After retries, **never emit `allow`** — emit `error`, route to dead-letter / human review. A
  partially-errored video never yields `allow`.
- **Surge/outage backpressure:** on sustained provider failure (≥`N` consecutive errors OR error-rate ≥`X`%
  over window `W`; configurable, defaults `N=20, X=50, W=60s`) **stop accepting new jobs** (readiness flips
  not-ready; ingress rejects with a retryable signal). Restore ready only after `M` consecutive successes
  (hysteresis, default `M=5`).
- **Bounded dead-letter:** cap DLQ depth; at capacity reject new enqueues + alert (never drop, never
  auto-allow). Metrics/alerts on DLQ depth + review-backlog age.
- The fail-safe override is gated behind a **non-default** flag emitting a prominent **audit event**.

### F.6 — Observability
`log/slog` structured logging (job id, adapter, latency, verdict — **never log media bytes, PII, `Raw`
free-text, OCR, captions**). **Prometheus `/metrics`** on `metrics.addr`:
`vismod_jobs_total{verdict}`, `vismod_adapter_request_seconds{adapter}`,
`vismod_adapter_errors_total{adapter,code}`, **`vismod_queue_depth`** (the autoscaling signal),
`vismod_deadletter_depth`, `vismod_workers_active`. `serve` exposes `/healthz` (liveness) and `/readyz`
(readiness incl. boot validation + Redis when applicable).

---

# §G — 🔴 RESPONSIBLE-USE, SAFETY & LICENSING (acceptance criteria, not optional)

Frame all legal references as design drivers with a prominent "this is not legal advice — consult counsel"
disclaimer.

1. **License: Apache-2.0** (patent grant + retaliation + `NOTICE`). Ship `LICENSE` + `NOTICE`.
2. **Never persist or transmit the illegal media itself.** Operate on hashes/derivatives + per-job
   transient working copies deleted promptly (§B.1 cleanup contract). Encrypt-at-rest + strict access
   control for any transiently held flagged material. Durable queue payloads and any operator/UI surface
   **carry opaque IDs/refs, never media bytes**, and are access-controlled.
3. **CSAM is handled by the §D.7 hash-match seam, not the classifier.** v1 ships the **seam + the
   `CSAM_HASH_MATCH` category + the `match_type`/`match_list` fields + docs**; the matcher (Meta PDQ image /
   TMK+PDQF video, BSD via `facebook/ThreatExchange`/HMA) is **v1.1**. **PhotoDNA is licensed-access only —
   never bundle it.** A hash hit is binary list-membership (`Score=nil`), never `1.0`.
4. **Human-in-the-loop:** no fully-automated consequential action on a positive match; borderline →
   human review.
5. **Tamper-evident audit log** (`internal/audit`): append-only, hash-chained. Each record
   `= {seq, timestamp, prev_hash, payload, entry_hash}`,
   `entry_hash = SHA-256(seq‖timestamp‖prev_hash‖canonical(payload))`; genesis `prev_hash` = zeros;
   `O_APPEND`; reject out-of-order `seq`; **idempotent per `JobID`** (look up JobID under the append lock
   first; if present, skip the append — no new `seq`). **Determinism:** `canonical(payload)` = **RFC 8785
   JCS** (sorted-key, UTF-8, compact JSON); `‖` fields **length-prefixed** with fixed encodings (`seq`
   8-byte BE, `timestamp` RFC 3339 UTC ns, `prev_hash` raw 32 bytes). Payload binds the decision to inputs
   **by hash** — store `SHA-256(Raw)` + `ModelIdentity` + verdict, **never `Raw` itself**. Ship **`vismod
   audit verify`** (recomputes the chain, reports the first broken link). **Honestly scoped:** a bare chain
   detects truncation/in-place edits, not a full-chain rewrite by a write-capable insider — document in
   `SECURITY.md`; define a seam for HMAC/Ed25519 signing or head-hash anchoring as the tamper-*resistant*
   upgrade.
6. **Configurable thresholds + transparency on limits.** Document perceptual-hash evadability and
   classifier false-positives/bias; make the precision/recall tradeoff tunable.
7. **Score output is not treated as an evasion-oracle risk in v1.** v1 emits full scores/thresholds (the
   underlying models are already publicly testable); note the residual risk in `SECURITY.md`. No
   `antiabuse.*` config or output mode ships in v1.
8. **v1 has NO CSAM detection — define the safe failure mode.** A high-severity `SEXUAL` hit is **not** a
   CSAM determination and must be handled as **potential-CSAM**. **Trigger:** a `SEXUAL` `CategoryResult`
   with normalized `Score ≥ thresholds.SEXUAL.potential_csam` (default `0.667`). **Divert path** (via the
   `internal/review` `Diverter` seam): on trigger the pipeline diverts the frame to the potential-CSAM
   channel **before `Sink.Write`** — the ordinary envelope/audit record stores only `SHA-256(frame)` +
   verdict (never frame bytes or `Raw`), and the frame goes to human review under §G.2 rules; **do not
   persist the frame in ordinary result/audit storage**; surface jurisdictional reporting guidance.
   `RESPONSIBLE_USE.md` must state operators needing CSAM coverage cannot rely on this tool until the v1.1
   matcher ships.
9. **Docs that MUST ship:** `README.md`, `LICENSE` (Apache-2.0), `NOTICE`, `SECURITY.md` (SSRF, audit-log
   threat scope, anti-abuse residual risk, ffmpeg-workflow trust boundary), `RESPONSIBLE_USE.md`
   (not-legal-advice + reporting guidance + "do not test against real CSAM" + potential-CSAM policy),
   `MODEL_AND_HASH_LIMITATIONS.md` (cross-provider non-portability), `CONTRIBUTING.md`,
   `CODE_OF_CONDUCT.md`, `config.example.yaml`, and a "Writing custom FFmpeg workflows" guide.

---

# §H — BUILD SEQUENCE (milestones; keep `main` green)

- **M0 — Runnable skeleton (day one, no credentials/network):** `go mod init`; blessed deps. Define
  `pkg/moderation` types (§D.1, §E incl. nullable scores + schema_version + hash-match fields). Registry +
  `AdapterConfig` (§D.2). `memq` **with a real DLQ + bounded retry + panic recovery**, `internal/pipeline`
  (per-frame non-fatal fan-out), no-op `HashMatcher` pre-stage, `result` JSONL sink (idempotent per
  JobID), cobra `scan`/`serve`/`adapters`/`workflows`/`audit`/`version`/`healthcheck`. **Uses
  `fakeModerator` + `fakeFrameSource`** so `vismod scan x.jpg` and `vismod serve` run end-to-end with no
  creds. Tests: registry; threshold→verdict; per-frame fail-safe rollup; memq FIFO + DLQ + drain; panic
  dead-letters.
- **M1 — Three vendor adapters (§C):** `microsoft` (REST, both auth modes, `MaxImageBytes` pre-flight,
  shared rate limiter, retry classification), `google` (official SDK, likelihood lookup), `hive` (REST,
  per-head probability; **verify live response schema**). Golden-file normalization + `httptest` for the
  REST clients. Switching `adapter.name` selects the model at startup with no code change.
- **M2 — Direct FFmpeg framing + configurable workflows (§B):** `FFmpegSource` (absolute caller-owned
  `WorkDir` + `defer cleanup`, boot probe), the workflow engine + guardrails + `vismod workflows
  list/validate`, ship 3 default workflows, lazy-decode fan-out, video aggregation, fail-safe error mapping.
- **M3 — Durable queue + autoscaling (§D.6):** `redisq` (durable, at-least-once, idempotent Sink+audit),
  `vismod_queue_depth` metric, KEDA `ScaledObject` + HPA examples, Redis boot/readiness validation,
  drain-on-deploy, per-process→shared limiter note.
- **M4 — Docker (§I) + observability (§F.6):** multi-stage build bundling ffmpeg+ffprobe, boot validation,
  `/metrics` `/healthz` `/readyz`.
- **M5 — Responsible-use & docs (§G):** all docs, concretized audit log + `audit verify`,
  potential-CSAM divert (`internal/review`), `config.example.yaml`.
- **M6 — Web UI (STRETCH, §J):** embedded read-mostly dashboard.

---

# §I — DOCKER

Multi-stage. **Stage 1:** builder pinned to the module's Go toolchain; `go build -trimpath
-ldflags='-s -w'`, `CGO_ENABLED=0 GOOS=linux`. **🔴 Stage 2 runtime MUST include `ffmpeg`+`ffprobe`**
(`distroless/static` is insufficient): `debian:bookworm-slim` or `alpine` + `apk add --no-cache ffmpeg`,
**non-root**. Ensure `frames`/`ffmpeg` workdir is a **writable, non-root-owned, ephemeral path**
(tmpfs/`emptyDir`/`VOLUME`) so a read-only-rootfs container can still create the extraction `WorkDir`. Add
`HEALTHCHECK` hitting `/healthz` and `STOPSIGNAL SIGTERM` (graceful drain). One image, both modes:
`ENTRYPOINT ["/vismod"]`, default `CMD ["serve"]`; one-shot via `docker run <img> scan /data/clip.mp4`.

---

# §J — WEB UI (STRETCH GOAL)

A **read-mostly** operator dashboard served by the **same binary**, behind `ui.enabled` (default off) and
auth (`ui.auth`). Built with **stdlib `net/http` + `embed`** for static assets (no external frontend
toolchain unless justified). Surfaces:
- **Ongoing jobs:** live list + status (queued/running/done/dead-lettered), counts, throughput.
- **Workers / pool:** active worker count, queue depth, per-replica view; the metric that drives
  autoscaling. "Manage workers" in v1 = **operational controls only** (pause intake / drain / resume via
  the backpressure seam), **not** arbitrary code — document the boundary.
- **Configuration:** active adapter, thresholds, FFmpeg workflows — **read-only** (config changes are
  restart-to-apply; never expose secrets).
- **Metrics:** embed/scrape the Prometheus counters (`/metrics`) into simple charts.

**🔴 Hard rules:** the UI **never renders media bytes, `Raw` free-text, OCR, captions, or PII** — hashes,
verdicts, and metadata only. It is an additive surface: the pipeline is fully functional headless without
it.

---

# §K — ACCEPTANCE CRITERIA (Definition of Done)

1. `vismod scan <image>`/`<video>` and `vismod serve` run end-to-end and emit a valid envelope; `serve`
   drains gracefully on SIGINT/SIGTERM (no job left both unacked and uncleaned). M0 runs with **no creds**.
2. Switching `adapter.name` among `microsoft`/`google`/`hive` selects the model at startup with **no code
   change**; unknown adapter fails fast listing registered names. **No `stub` in the registry**; tests run
   credential-free via fakes.
3. Each adapter normalizes into the §E schema (Microsoft `severity/6`; Google likelihood lookup with
   `UNKNOWN→nil`; Hive per-head probability); golden-file tests prove stability; unmapped labels → `OTHER`,
   never `0` for unknown.
4. Video path uses **direct FFmpeg** with an **absolute caller-owned `WorkDir`** and `defer cleanup` on
   every exit path; missing ffmpeg/ffprobe surfaces at boot; extraction errors / zero-frames yield
   `Verdict=error` (never allow); cleanup completes **before** ack.
5. **Configurable workflows:** 3 defaults ship; `vismod workflows validate` gates; custom workflows run via
   arg-slice exec with **no shell**, protocol allow-list, `{{.Input}}`-only input binding, and
   output confined to `WorkDir`; an injection/SSRF-crafted workflow is rejected at validation.
6. **Autoscaling:** `vismod_queue_depth` is exported; a documented KEDA/HPA contract scales replicas;
   multi-replica requires `driver=redis`; the shared-rate-limiter (or per-replica budget) note ships.
7. **Fail-safe:** provider failure → `Verdict=error` (never `allow`) + dead-letter; a partial video with
   any errored frame never yields `allow`; a worker **panic dead-letters and the pool keeps running**;
   sustained outage flips readiness (backpressure).
8. **Schema:** every envelope carries `schema_version`; `MaxScore`/`Confidence` are `nil` (not `0.0`) when
   no non-nil score exists; all-`nil`/unsupported never yields `allow`; unknown future `Category` tolerated
   as `OTHER`.
9. **Queue swap is behavior-preserving:** the same `Disposition` produces the same retry/DLQ outcome on
   memq and redisq; memq exercises a real DLQ; re-enqueuing a completed `JobID` does **not** double-write
   Sink or audit.
10. **Safety:** Apache-2.0; all §G docs ship; `CSAM_HASH_MATCH` seam + category + `match_*` fields exist
    (no-op matcher); no adapter leaks `Raw` free-text/OCR/captions into Sink or audit (audit stores
    `SHA-256(Raw)` + `ModelIdentity`); `vismod audit verify` detects a tampered link; a high-severity
    `SEXUAL` result does not persist the frame into ordinary Sink/audit storage; secrets are env-only.
11. Docker image runs both modes, contains ffmpeg+ffprobe, non-root, writable workdir, `/healthz`
    healthcheck. README documents the FIFO completion-order caveat, memq non-durability, cross-provider
    score non-portability, and the ffmpeg-workflow trust boundary.
12. **(Stretch)** Web UI: read-mostly, auth-gated, embedded, never renders media/`Raw`/PII; pipeline fully
    functional headless without it.

---

# §L — SCOPE DECISIONS / NON-GOALS

- **v1 adapters = `microsoft` + `google` + `hive`.** A fourth vendor is one adapter package + golden tests
  via the §E seam.
- **Autoscaling = horizontal replica scaling driven by `vismod_queue_depth`** (KEDA/HPA). In-process
  elastic goroutine pools are **not** in scope; each replica runs a fixed pool. Multi-replica requires
  `driver=redis`.
- **v1 implements classifier-based moderation only.** CSAM perceptual-hash matcher (PDQ/TMK) is
  architected-for + documented in v1, implemented **v1.1**.
- **No hot-swapping** the active model (restart-to-change accepted). **No batch path** in v1.
- **Web UI is a stretch goal**, additive, read-mostly. Non-US legal regimes (EU CSA Reg, UK OSA,
  IWF/INHOPE) noted in docs, not encoded.
- **Custom FFmpeg workflows are an operator-trust boundary** — the guardrails (§B.2) are the security
  contract; arbitrary raw ffmpeg command strings and remote protocols are out of scope by design.
