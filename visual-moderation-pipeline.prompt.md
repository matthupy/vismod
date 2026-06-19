# Build Prompt — Open-Source Visual Content Moderation Pipeline

> **How to use this document.** This is a self-contained build prompt for a coding agent (or engineer).
> Everything in **§A–§C is research-verified fact** — treat it as ground truth and do not re-research it.
> **§D–§K are the build instructions.** Work prototype-first (§H milestone M0 must run end-to-end on day one),
> then iterate. When a fact here conflicts with a model's prior assumption, **this document wins.**
>
> **Revision v2 (post architecture review).** This version folds in a 5-lens senior-architect review.
> v2 changes are marked **[v2]**. Scope decision: **v1 targets a single `serve` replica**; horizontal
> scaling is the M5 hardening path (§L). Default identifiers (rename freely): module
> `github.com/<org>/vismod`, binary `vismod`.

---

## ROLE & MISSION

You are a senior Go engineer building an **open-source visual content moderation pipeline**. It scans
**images and video** for harmful content using a pluggable visual-moderation model, normalizes wildly
different provider outputs into one schema, and runs both as a **one-shot CLI** and a **long-running
containerized worker**.

The project is a **public good with no commercial goals**, intended for adoption by trust & safety
organizations such as **ROOST (Robust Open Online Safety Tools)** and smaller platforms that lack
in-house moderation infrastructure. Optimize for: **a working prototype as fast as possible**, then
**fast, well-tested iteration**. Correctness, safety guardrails, and auditability are first-class — this
tool may encounter illegal content, and a careless design causes real-world harm.

---

## OPERATING PRINCIPLES

1. **Prototype-first.** Get the full pipeline running end-to-end with a credential-free **`stub` adapter**
   before integrating any real provider (§H M0). Never let "no API key" block a runnable system.
2. **Code to interfaces, swap implementations.** Every external concern (moderation model, queue, frame
   source, result sink, hash matcher) sits behind a Go interface. The prototype uses the lightest impl;
   the hardened impl swaps in via config with **zero call-site changes** — and the interface must be rich
   enough that the swap is genuinely behavior-preserving (see §D.3 **[v2]**).
3. **Prefer platform/stdlib features over hand-rolled scaffolding.** Use `log/slog`, `context`,
   `errgroup`, `encoding/json`, `net/http` + `httptest`. Add a dependency only when it earns its place
   (the locked deps in §D are pre-justified).
4. **Tests land with features.** Table-driven tests, interface fakes/mocks, golden files for
   normalization, `httptest` for provider clients. The pipeline must be runnable and testable without
   network or credentials.
5. **Fail safe, never fail silent.** Moderation is security-critical. On provider/frame failure, **never
   emit `allow`** — emit an `error` verdict and route to human review / dead-letter (§F.5).
6. **Small, reviewable steps.** Land each milestone (§H) as a coherent, tested unit. Keep `main` green.
7. **Track latest; pin only deliberately.** Default to the newest version of every dependency. Do **not**
   freeze a dependency to a commit SHA — *especially* an internal repo under active co-development (see the
   §B videosift dependency policy). A pin freezes you against a stale snapshot and silently blocks upstream
   changes from propagating. Introduce a version pin only at a deliberate release boundary, and only after
   explicitly confirming it — never assume a pin.

---

# §A — FUNCTIONAL REQUIREMENTS (verified against the task spec)

1. **Inputs:** `image` and `video`.
   - For **video**, extract the most relevant frames via the internal dependency **videosift** (§B), then
     moderate each frame as an image (unless the active adapter is video-native — §D.1 **[v2]**).
2. **Visual moderation models:** **Azure AI Content Safety first** (§C), with a pluggable adapter design
   so others (Hive/thehive.ai, Google, AWS, …) can be added later. Providers have **wildly different
   output formats** → a **normalization layer** (§E) maps each into one model-agnostic schema.
3. **Runtime model selection:** Exactly **one** moderation model active per process, chosen from config at
   startup. **Restart-to-change is acceptable** — no hot-swapping. **[v2]** v1 is **single-replica**; the
   cluster-wide "exactly one model" invariant under multiple replicas is an M5 concern (§L).
4. **Per-adapter capabilities:** Adapters declare capabilities (e.g. `SupportsVideo`). All adapters
   **must** support image input; video support is optional. When the active adapter is not video-native,
   the pipeline extracts frames (videosift) and moderates them as images.
5. **Language & packaging:** **Go**, runnable as a **CLI** and as a **Docker container** (one binary,
   subcommands).
6. **Job management:** a **FIFO** job queue.
7. **Code storage:** **GitHub.**

---

# §B — INTERNAL DEPENDENCY CONTRACT: `github.com/matthupy/videosift`

> Reviewed at commit `66e5f27` **only to verify the API surface documented below — this is NOT a version
> to pin to.** License: MIT. `go.mod` declares `go 1.26.3` (README says "1.22+ to build" — your build
> toolchain **must satisfy `1.26.3`**, see §I). *How* frames are extracted (ffmpeg internals) is out of
> scope and encapsulated in `internal/ffmpeg`; consume the public API only.
>
> **🔴 DEPENDENCY POLICY — videosift is actively co-developed by the project owner: TRACK LATEST, DO NOT
> PIN.** Add it with `go get github.com/matthupy/videosift@latest` (with no tags, `@latest` resolves to the
> newest commit on the default branch). While developing BOTH repos in tandem, add a local
> `replace github.com/matthupy/videosift => ../videosift` to `go.mod` so in-progress videosift changes
> propagate **immediately**, with no push/pull cycle. **Never freeze videosift to a specific commit SHA** —
> that silently strands the pipeline on a stale snapshot while the owner's ongoing changes fail to flow
> through (a multi-hour debugging trap). Introduce a pin only at a deliberate release boundary, and only
> after explicitly confirming it first.

**Import:** `import "github.com/matthupy/videosift"` (package `videosift`).

**Sole entry point (verbatim):**
```go
func Extract(ctx context.Context, videoPath string, cfg Config) ([]Frame, error)
```
Stateless package function. Honors `ctx` cancellation. **[v2] There is NO streaming/iterator API** — it
returns the *entire* `[]Frame` slice only after writing every PNG to `WorkDir`. Strategies fan out via
`errgroup`; the **first** strategy error cancels all in-flight work and is returned wrapped as
`strategy <name>: ...`.

**Config** (value type; obtain via `DefaultConfig() Config` then override; a zero `Config` also works):
- Strategy toggles (all default `true`; `ErrNoStrategies` if all `false`): `Scene`, `Keyframe`,
  `Temporal`, `MPDecimate` — fixed, **not** a pluggable interface.
- `SceneThreshold float64` (0.4), `TemporalInterval float64` sec (2.0), `MPDecimateHi/Lo int` +
  `MPDecimateFrac float64` (768/320/0.33).
- `HashAlgo HashAlgo` (`"phash"`|`"dhash"`, default phash), `HammingThreshold int` (8; `0` disables hash
  dedup), `HashResizeWidth int` (256; `0` disables).
- **`MaxFrames int`** (`0` = **unlimited**; uniform-stride cap keeping first+last). **[v2] Always set a
  non-zero `MaxFrames`** — it is the primary bound on both per-video classifier **cost** and peak **disk**
  (it materializes `MaxFrames` PNGs in `WorkDir`). Default `frames.max_frames: 64` in config (§F.3).
- `Concurrency int` (default = number of enabled strategies).
- `FFmpegPath`/`FFprobePath string` (default `"ffmpeg"`/`"ffprobe"`).
- **`WorkDir string` — see the critical rule below.**

**`Frame` (verbatim):** `{ Index int; TimestampSec float64; Strategy Strategy; Path string; Hash uint64; Video *VideoInfo }`.
`Path` = absolute path to a **PNG** (output is always PNG). The slice is **already deduplicated and
ordered** (`Index` asc, `TimestampSec` asc). `VideoInfo`:
`{ Duration float64; Width int; Height int; Codec string; FrameRate float64; BitRate int64 }`.

**🔴 CRITICAL TEMP-FILE RULE (verified in `extract.go`):** if `cfg.WorkDir == ""`, `Extract` creates an
`os.MkdirTemp("", "videosift-*")` dir and `defer os.RemoveAll`s it **before returning** — so every
returned `Frame.Path` points at an **already-deleted file**. The pipeline **MUST** set `cfg.WorkDir` to an
**absolute** dir it creates and owns, decode each PNG, then delete that dir itself. See the **lifecycle
contract in §D.4 [v2]** for exactly when cleanup runs relative to ack.

**Errors (`errors.go`):** sentinels `ErrNoBinaries` (ffmpeg/ffprobe not on PATH), `ErrNoStrategies`,
`ErrNoFrames` (zero frames, or dedup emptied the set). `FFmpegError{ Args []string; Stderr string; Cause error }`
with `Unwrap()`. Use `errors.Is`/`errors.As`.
**[v2] Fail-safe mapping (mandatory):** any videosift extraction failure (`*FFmpegError`, `ErrNoBinaries`
at runtime) **and `ErrNoFrames` for video** are **could-not-evaluate** conditions → emit `Verdict=error`
and dead-letter; **never treat zero-frames as clean/allow** (a static/looping harmful video must not pass
by producing no frames). Only an explicit, audited operator override (the §F.5 gated flag) may downgrade
an empty-after-dedup result — this produces an operational **skip** (job acked, no verdict emitted), **not**
a `Verdict` value (the enum stays `allow|flag|block|error`).

**🔴 OPERATIONAL PREREQ (locked — affects Docker §I):** videosift **execs external `ffmpeg` AND `ffprobe`
binaries.** A `CGO_ENABLED=0` static Go binary is therefore **not self-sufficient at runtime**; the
container runtime stage **must** bundle `ffmpeg`+`ffprobe`. **Validate once at boot** so `ErrNoBinaries`
surfaces as a clear operator error, not a per-job failure.

**Out-of-process option:** videosift ships `cmd/extract` (`-i/-o/-threshold/-interval/-max-frames/-hamming/-algo/-no-{scene,keyframe,temporal,mpdecimate}/-ffmpeg/-ffprobe/-json`).
Wrap your in-process call behind `FrameSource` (§D.4) so either approach stays swappable and mockable.

---

# §C — FIRST ADAPTER CONTRACT: Azure AI Content Safety (Image)

> Verified against Microsoft Learn; **GA `api-version=2024-09-01`**. **No Go SDK** for the Content Safety
> data plane — call REST directly (`net/http` + `encoding/json`). Pin `api-version` as a **configurable
> const** (Microsoft uses a ~90-day deprecation cadence).

- **Endpoint:** `POST {endpoint}/contentsafety/image:analyze?api-version=2024-09-01`
  where `{endpoint}` = `https://<resource>.cognitiveservices.azure.com`. **Synchronous.**
- **Auth:** API key header `Ocp-Apim-Subscription-Key: <key>` + endpoint URL **OR** Microsoft Entra ID /
  Managed Identity (OAuth2 scope `https://cognitiveservices.azure.com/.default`). Support both.
- **Request body:** `{"image":{"content": "<base64>"} | {"blobUrl":"<uri>"}, "categories":[...]?, "outputType":"FourSeverityLevels"?}`.
  Provide **exactly one** of `content`/`blobUrl` (both ⇒ refused). Minimal: `{"image":{"content":"<b64>"}}`.
  **[v2] SSRF/egress:** `blobUrl` (and any future `Source.Kind=url/s3`) is a remote-fetch vector.
  **v1 defaults to local-file / inline `content` only.** If `blobUrl` is enabled, require a host/scheme
  **allow-list** and forbid private/link-local/metadata ranges (RFC1918, `169.254.0.0/16`, `::1`).
  Document in `SECURITY.md`.
- **Response (exact):**
  ```json
  {"categoriesAnalysis":[{"category":"Hate","severity":0},{"category":"SelfHarm","severity":0},
                         {"category":"Sexual","severity":0},{"category":"Violence","severity":2}]}
  ```
  Top-level has **only** `categoriesAnalysis`. **No score, no flag/decision field** — *you* apply
  thresholds. Multi-label.
- **Categories (image:analyze returns exactly 4):** `Hate`, `SelfHarm`, `Sexual`, `Violence`.
  ⚠️ The harm-categories doc also lists **"Task Adherence"** — an agent-behavior category **NOT** returned
  by `image:analyze`. **Do not include it.**
- **🔴 Severity model — IMAGE is the TRIMMED scale ONLY:** discrete `0, 2, 4, 6`
  (`0`=Safe, `2`=Low, `4`=Medium, `6`=High). **Normalize as `severity / 6.0`.**
- **Video:** **not natively supported** (animated GIFs ⇒ first frame only) ⇒ extract frames, moderate
  per-image. The adapter operates on a **single image**; video aggregation lives one layer up (§E).
- **Input limits:** max file size **4 MB**; formats **JPEG/PNG/GIF/BMP/TIFF/WEBP**. Dimensions: sources
  conflict (REST ref ≤2048×2048; overview ≤7200×7200) → **configurable max-dimension, default conservative
  2048×2048.** Hard-code the 4 MB cap and the format allow-list. **[v2]** Surface the 4 MB cap as
  `Caps.MaxImageBytes` so the pipeline pre-flights oversize images (§D.6).
- **Rate limits:** F0 (free) = **5 RPS**; S0 = **1000 requests / 10s**. No batch API. *(Do not state F0
  monthly quotas/$ — point implementers to `aka.ms/content-safety-pricing`.)*
- **Errors:** `{"error":{"code","message","target","details","innererror"}}`; code in header
  `x-ms-error-code`. Backoff on `429`.
- **CSAM policy:** Azure Content Safety **must not be used to detect child exploitation imagery** (vendor
  policy) — CSAM is handled by the hash-match seam (§D.7/§G.3), never the classifier.

**Normalization mapping (Azure → §E):** per element → `Hate→HATE, Sexual→SEXUAL, Violence→VIOLENCE,
SelfHarm→SELF_HARM`; `Score = severity/6.0`, `ScoreOrigin="severity"`; `Flagged` via configurable
per-category threshold (Microsoft suggests level 4 ⇒ `Score ≥ 0.667`).

---

# §D — ARCHITECTURE (locked decisions)

**Module layout**
```
github.com/<org>/vismod
  cmd/vismod/main.go            # thin: calls cli.Execute()
  pkg/moderation/              # PUBLIC: the result/contract types external CONSUMERS of NormalizedResult bind to
    types.go                   # Moderator, VideoModerator, NormalizedResult, Caps, Verdict, Category, ScoreOrigin, Image, HashMatcher
  internal/
    cli/                       # cobra commands: root, scan, serve, adapters, audit, version
    config/                    # viper loader + typed Config; env overlay; secrets via env only
    moderate/
      registry.go              # AdapterConfig + map[string]Factory + Register/New; init-based self-registration
      adapters/{stub,azure}/   # v1 adapters (in-tree); future: hive/, google_vision/, aws_rekognition/, ollama/
    frames/                    # FrameSource interface + pipeline-owned Frame; videosift impl + fake
    queue/                     # Queue interface + memq (prototype, with real DLQ) + asynqq (M5)
    pipeline/                  # HashMatcher pre-stage -> frames -> fan-out -> Moderator -> normalize -> aggregate -> Sink
    result/                    # Sink interface (stdout JSONL / file; webhook/DB later)
    audit/                     # append-only hash-chained decision log + `verify`
    observe/                   # slog, Prometheus /metrics, /healthz /readyz
    hashmatch/                 # HashMatcher interface impls (no-op default in v1; PDQ in v1.1)
```
**[v2] Adapter authorship for v1 is IN-TREE.** `pkg/moderation` exposes the contract/result types (so
external *consumers* of the JSON output get typed bindings, and adapter authors implement `Moderator`
against them); the **registry lives in `internal/moderate`**. Do **not** claim out-of-tree third-party
adapters work in v1 (Go's `internal/` rule makes `Register` unreachable externally). If out-of-tree
adapters become a goal, promote `Register`/`Factory`/`AdapterConfig` to a public `pkg/moderation/registry`
package — note this as a documented future step, don't build it now.

**Locked dependencies:** `spf13/cobra` + `spf13/viper` (one binary, CLI + daemon via subcommands);
`golang.org/x/sync/errgroup`; `hibiken/asynq` (Redis queue — **M5 only**); `prometheus/client_golang`
(metrics). `log/slog` for logging.

### D.1 — `Moderator` (+ optional `VideoModerator`) — `pkg/moderation` **[v2]**
```go
type Moderator interface {
    Name() string
    AnalyzeImage(ctx context.Context, img Image) (NormalizedResult, error)
    Capabilities() Caps
    Close() error
}

// OPTIONAL second interface — keeps Moderator stable when a video-native provider lands.
// The pipeline type-asserts for it; do NOT add AnalyzeVideo to Moderator later (that breaks every impl).
type VideoModerator interface {
    AnalyzeVideo(ctx context.Context, video Source) (NormalizedResult, error)
}

type Image struct { Bytes []byte; MIME string; Width, Height int; Meta map[string]string }

type Caps struct {
    SupportsVideo bool       // true => pipeline prefers AnalyzeVideo (if impl) over frame-by-frame
    MaxImageBytes int64      // pipeline pre-flights oversize images before calling AnalyzeImage
    Categories    []Category // canonical categories this adapter can emit
}
```
Pipeline dispatch: `if vm, ok := m.(VideoModerator); ok && m.Capabilities().SupportsVideo { vm.AnalyzeVideo(...) } else { frame-by-frame via FrameSource }`.
**[v2]** `SupportsBatch` is dropped for v1 (no batch path exists; Azure has no batch API). Re-add when an
adapter with a batch API lands.

### D.2 — Adapter registry + `AdapterConfig` (`internal/moderate`) **[v2]**
```go
// AdapterConfig is a PROVIDER-OPAQUE carrier. Lives in internal/moderate (carries secret wiring), NOT pkg/.
type AdapterConfig struct {
    Name    string
    Options map[string]any          // each adapter decodes this into its OWN typed config inside its Factory
    Secret  func(key string) string // env-backed secret accessor (keeps API keys out of Options/yaml)
}
type Factory func(cfg AdapterConfig) (moderation.Moderator, error)
func Register(name string, f Factory)
func New(name string, cfg AdapterConfig) (moderation.Moderator, error) // unknown name => fatal, lists registered
```
**Rule:** the registry **never imports adapter packages**; adapters self-register via `init()` and are
pulled in by blank import at the composition root (`internal/cli`). `New` instantiates **only** the one
configured factory ⇒ "exactly one model active per process." `vismod adapters` prints registry keys +
`Capabilities`.

### D.3 — `Queue` interface (FIFO; prototype-first, **behavior-preserving** swap) **[v2]**
```go
type Disposition int
const ( Ack Disposition = iota; Retry; DeadLetter ) // explicit handler outcome — both drivers honor identically

type Queue interface {
    Enqueue(ctx context.Context, j Job) (JobID, error)
    Start(ctx context.Context, handler func(context.Context, Job) (Disposition, error)) error
    QueueDepth(ctx context.Context) (int, error) // uniform across drivers (memq: len; asynq: Inspector)
    Close(ctx context.Context) error             // graceful drain
}

type QueueConfig struct {
    Workers       int
    Buffer        int
    MaxRetries    int           // bounded retry before dead-letter
    RetryBackoff  time.Duration
    DrainTimeout  time.Duration // [v2] graceful-drain budget for in-flight jobs (drain rule below)
    JobTimeout    time.Duration // [v2] per-job processing timeout (§D.6/§F.4)
    DeadLetterMax int           // [v2] DLQ depth cap; at capacity reject enqueues + alert (§F.5)
    DeadLetter    Sink          // where dead-lettered jobs go (must exist in the prototype, not invented at M5)
}

type JobID string
type Job struct { ID JobID; Source Source; SubmittedAt time.Time }
```
- **Why `Disposition` (not a bare `error`):** §F.4 needs retryable-vs-terminal, and a bare `error` means
  opposite things on memq vs asynq. The explicit outcome makes the **memq→asynq swap behavior-preserving**
  (the "pipeline code unchanged" promise in §K.2 must be *true*): `Ack`→success, `Retry`→bounded backoff
  then DLQ, `DeadLetter`→DLQ immediately.
- **Prototype `memq`:** buffered `chan Job` (**FIFO by construction**) + worker pool. **Implements a real
  DLQ** (bounded list/sink), bounded in-memory retry honoring `MaxRetries`/`RetryBackoff`. Job states in a
  mutex-guarded map. **[v2] Durability boundary (document in code + README + warn at boot/`/readyz` when
  `driver=memory && serve`):** memq is **at-most-once, non-durable, single-process — dev/CLI only.** A
  crash loses enqueued + in-flight jobs. Production intake **must** use `driver=redis` (M5).
- **🔴 FIFO is a property of the QUEUE, not the pipeline — *dequeue order = enqueue order*.** The first job
  that *enters* the queue is the first job a worker *pulls* from it. Workers MUST dequeue in arrival order:
  never pick an arbitrary/random pending item, and **never order the pending set by a sortable key such as
  a UUID or job-ID string** — lexicographic ordering of UUIDs would perpetually prefer low-sorting IDs and
  silently starve every job past the pivot (a queue that never fully drains). The buffered channel gives
  arrival order for free; any other backing store MUST preserve it explicitly (an insertion-ordered
  structure or a monotonic enqueue sequence/score).
- **Start order ≠ completion order (document; assert in §K).** FIFO governs *dequeue/start* order only.
  With `>1` worker, **completion order is not guaranteed** (jobs finish at different speeds). Strict
  end-to-end ordering needs `workers=1` or per-key serialization. Same for asynq (per-queue FIFO dequeue,
  at-least-once, no completion ordering).
- **Graceful drain [v2]:** distinguish the worker-lifecycle ctx (cancels pulling **new** work) from a
  per-job processing ctx. On shutdown: stop enqueues; stop pulling new jobs; in-flight jobs get
  `drain_timeout` to **finish + Sink.Write + ack**; jobs not done in time are **left unacked / requeued**,
  **never acked-done and never silently dropped** (memq: log incomplete IDs at WARN; asynq: leave unacked
  for redelivery). Buffered-but-unstarted memq jobs are logged at WARN (or persisted) — not lost.
- **Hardening `asynqq` (M5):** Redis-backed. Maps `Disposition`→asynq retry/`SkipRetry`/archive. **[v2]
  At-least-once ⇒ idempotency required** (§D.6/§D.5/§G.5). **Payload hygiene:** job payloads in Redis and
  any `asynqmon` UI **must not contain media bytes** — only opaque IDs/refs; `asynqmon` must be
  access-controlled; durably-referenced flagged material follows §G.2.

### D.4 — `FrameSource` + pipeline-owned `Frame` (`internal/frames`) **[v2]**
```go
// Pipeline-owned. MUST NOT embed or alias videosift types (that re-leaks the quarantined dep).
type Frame struct { Index int; TimestampSec float64; Path string } // absolute path to a PNG

type FrameSource interface {
    Frames(ctx context.Context, videoPath string) (frames []Frame, cleanup func() error, err error)
}
```
The videosift-backed impl sets an **absolute caller-owned `cfg.WorkDir`**, maps `videosift.Frame`→
`frames.Frame` (so the `cmd/extract` JSON path produces identical values), returns a `cleanup` closure
that deletes the dir, and maps videosift errors per §B fail-safe rule. Provide a `fakeFrameSource`.
**🔴 Lifecycle contract:** the pipeline **MUST `defer cleanup()` immediately after `Frames()` returns**
(before any fan-out) so `WorkDir` is deleted on **every** exit path — error, ctx-cancel, panic. `cleanup`
must be **idempotent**; its error is logged via slog but does not change the verdict. **Order per job:**
`Frames()` → `defer cleanup` → fan-out+normalize+aggregate → `Sink.Write` → **(only then)** ack/dead-letter.
`Sink.Write` must not retain references to frame files.

### D.5 — `Source` and `Sink`
```go
type Source struct {
    Kind      string // "file" (v1 default); "url"/"s3" later (SSRF allow-list required — §C)
    Ref       string
    MediaType string // "image" | "video" | "" (auto-detect)
}

type Sink interface { Write(ctx context.Context, env ResultEnvelope) error } // MUST be idempotent per JobID [v2]

type ResultEnvelope struct {
    JobID      JobID                        `json:"job_id"`
    Source     Source                       `json:"source"`
    ModelID    ModelIdentity                `json:"model_id"`   // [v2] adapter+version+threshold-config hash
    Result     *moderation.NormalizedResult `json:"result,omitempty"`
    Error      string                       `json:"error,omitempty"`
    StartedAt  time.Time                    `json:"started_at"`
    FinishedAt time.Time                    `json:"finished_at"`
}
type ModelIdentity struct { Adapter, ModelVersion, ConfigHash string } // [v2] stamped on every job for audit
```
**[v2] Provenance (so audit binding is reproducible):** `ConfigHash` = `SHA-256` over the canonicalized
verdict-affecting config — the active adapter name + `ModelVersion` + the resolved per-category threshold
map (exclude secrets, log level, addrs). `AssetID` (§E `NormalizedResult`) = the job's `Source.Ref`
(fallback: the `JobID`), **stamped by the pipeline after normalization** — the adapter/normalizer does not
know the job's `Source`, so it leaves `AssetID` empty and the pipeline fills it.
Prototype `Sink`: JSON-lines to stdout and/or file. **[v2] Idempotency:** `Sink.Write` upserts/dedupes by
`JobID` so asynq redelivery never double-writes; the audit append is likewise idempotent per `JobID`
(§G.5).

### D.6 — Concurrency, backpressure & per-frame failure **[v2 — substantially revised]**
Each video `Job` expands to many frames. **Do NOT use `errgroup` cancel-on-first-error for the moderation
fan-out** (that is right for videosift's *internal* strategy fan-out, but wrong here — each frame is an
independent evidence sample). Instead:
- Bound parallelism with `errgroup.SetLimit(frames.concurrency)` — the pipeline's per-job moderation
  fan-out limit, **distinct** from videosift's internal `cfg.Concurrency` (which the `FrameSource` impl
  leaves at its default of #enabled-strategies) — or a semaphore + `WaitGroup`, but each
  per-frame task **captures-and-returns its own outcome** `{FrameResult, err}` and **returns `nil`** so one
  frame's error **never cancels siblings**. Collect a `FrameResult` for **every** frame.
- **Lazy decode (memory bound):** videosift returns a materialized slice (no streaming — §B). Bound
  *memory* by decoding each `Frame.Path` **inside** its task (read→decode→`AnalyzeImage`→release) so at most
  `frames.concurrency` decoded images are resident. **Never pre-decode the whole slice.** Peak disk =
  `frames.max_frames` PNGs (bound it).
- **Pre-flight:** reject images exceeding `Caps.MaxImageBytes` *before* calling `AnalyzeImage` (terminal,
  per-frame error — §F.4). Pre-flight rejection happens **before** the shared rate limiter's `Wait`, so
  only real `AnalyzeImage` calls consume limiter tokens.
- **Per-job timeout** via ctx; **panic recovery** in every worker handler (per §F.4).
- **Asset aggregation rule (reconciles with §F.5):** see §E. In short — *block if any frame flagged; error
  if no frame could be evaluated; **never `allow` while any frame is in `error` state**.*
- Single active `Moderator` instance + its **shared** rate limiter (§F.3) gate all fan-out across all
  workers. `ctx` threads queue-worker → pipeline → tasks → `AnalyzeImage`.

### D.7 — `HashMatcher` pre-stage seam (CSAM) **[v2 — new]**
```go
type HashMatcher interface { Match(ctx context.Context, img Image) (HashMatch, error) }
type HashMatch struct { Matched bool; ListName string; Algo string } // binary list-membership, NOT a score
```
Runs as a **pipeline pre-stage before the `Moderator`**. A match short-circuits to a `CSAM_HASH_MATCH`
`CategoryResult` (`ScoreOrigin="list_membership"`, `Score=nil`, `Flagged=true`) and **does not call the
classifier**. **v1 ships a no-op default impl** (always `Matched:false`); the PDQ/TMK matcher is v1.1
(§G.3/§L). Shipping the seam + category + schema fields in v1 is a hard requirement (§K).

---

# §E — NORMALIZATION LAYER (the core public contract) **[v2 — schema revised]**

**Canonical category taxonomy (typed enum):**
`SEXUAL, SUGGESTIVE_RACY, VIOLENCE, GORE_GRAPHIC, WEAPONS, SELF_HARM, HATE, DRUGS, MEDICAL, SPOOF, CSAM_HASH_MATCH, OTHER`.
`CSAM_HASH_MATCH` is reserved for §D.7 (no classifier emits it). `MEDICAL`/`SPOOF` exist only to carry
Google Vision SafeSearch's `medical`/`spoof` — **document their provenance** so consumers don't misread
them as harm signals. **Fallback discipline:** any provider label with **no** canonical mapping →
`OTHER`, preserving the raw label in `ProviderLabel` and carrying its `Score` — **never drop a result.**

**Public Go types (`pkg/moderation`):**
```go
type Verdict string      // "allow" | "flag" | "block" | "error"
type Category string      // canonical enum above
type ScoreOrigin string   // "probability" | "confidence_pct" | "likelihood_enum" | "severity" | "list_membership"
type FrameStatus string   // "ok" | "error"

type CategoryResult struct {
    Category      Category    `json:"category"`
    ProviderLabel string      `json:"provider_label"` // raw native class/Name/enum
    Score         *float64    `json:"score"`          // normalized 0..1; nil = unknown/unsupported/list-membership
    ScoreOrigin   ScoreOrigin `json:"score_origin"`
    Threshold     *float64    `json:"threshold"`      // [v2] flag_at boundary; nil for list_membership rows; block_at applied at rollup (§E), captured in ConfigHash
    Flagged       bool        `json:"flagged"`        // (Score!=nil && Threshold!=nil && *Score>=*Threshold) OR list-membership match
    MatchType     string      `json:"match_type,omitempty"` // [v2] e.g. "pdq"/"md5"/"sha1" (list_membership only)
    MatchList     string      `json:"match_list,omitempty"` // [v2] e.g. "ncmec"/"iwf"/"gifct"
}

type FrameResult struct {
    TimestampSec *float64         `json:"timestamp_sec"` // nil for still images
    Status       FrameStatus      `json:"status"`        // [v2] "ok" | "error"
    Error        string           `json:"error,omitempty"` // [v2] per-frame failure detail
    Categories   []CategoryResult `json:"categories"`
}

type OverallVerdict struct {
    Verdict     Verdict   `json:"verdict"`
    Flagged     bool      `json:"flagged"`
    TopCategory *Category `json:"top_category"`
    MaxScore    *float64  `json:"max_score"`  // [v2] nil when NO non-nil score exists (never collapse to 0.0)
    Confidence  *float64  `json:"confidence"` // [v2] nil when NO non-nil score exists
}

type NormalizedResult struct {
    SchemaVersion string          `json:"schema_version"` // [v2] e.g. "1.0"; set by NORMALIZER, not adapter
    Provider      string          `json:"provider"`
    ModelVersion  string          `json:"model_version"`  // ModerationModelVersion / api-version; "" if none
    MediaType     string          `json:"media_type"`     // "image" | "video"
    AssetID       string          `json:"asset_id"`
    Frames        []FrameResult   `json:"frames"`         // image => single frame, TimestampSec nil
    Overall       OverallVerdict  `json:"overall"`
    Raw           json.RawMessage `json:"raw,omitempty"`  // [v2] OPTIONAL + SANITIZED — see §G.2/§F.6
}
```

**[v2] Serialization & provenance rules:**
- **Nullable scalars serialize as JSON `null`, never omitted** (no `omitempty` on `Score`, `Threshold`,
  `MaxScore`, `Confidence`, `TimestampSec`, `TopCategory`) — consumers read an explicit `null`, not an
  absent field. A flagged hash-match row emits `"score":null`, `"threshold":null`, `"flagged":true`; an
  asset rollup may emit `Overall.Flagged:true` with `"max_score":null` (the expected hash-match shape).
- **Hash-match field mapping (§D.7):** `HashMatch.ListName → CategoryResult.MatchList`,
  `HashMatch.Algo → CategoryResult.MatchType`.
- **Single source of truth:** `ResultEnvelope.ModelID.ModelVersion` (§D.5) is **copied from**
  `NormalizedResult.ModelVersion` (§E); the normalizer sets `ModelVersion` and `SchemaVersion`, never the
  adapter.

**Score normalization → `[0.0, 1.0]` (tag every score with `ScoreOrigin`):**
- **Azure:** `severity / 6.0` (`ScoreOrigin="severity"`).
- **Hive (future):** per-head positive-class mass (`ScoreOrigin="probability"`).
- **AWS (future):** `Confidence / 100.0` (`ScoreOrigin="confidence_pct"`); map via `ParentName`
  (`TaxonomyLevel` is **≥0** — parse defensively), keep leaf `Name` in `ProviderLabel`.
- **Google Vision (future):** configurable likelihood lookup `VERY_UNLIKELY→0.0, UNLIKELY→0.25,
  POSSIBLE→0.5, LIKELY→0.75, VERY_LIKELY→1.0`; **`UNKNOWN`→`Score=nil`** (`ScoreOrigin="likelihood_enum"`).
- **Hash match:** `Score=nil`, `ScoreOrigin="list_membership"`, `Flagged` by match presence.

**🔴 Scores are within-provider comparable ONLY [v2].** `severity/6`, `Confidence/100`, a likelihood
bucket, and a softmax mass are **not** the same quantity; a `0.667` threshold means different things per
provider. Thresholds **must be re-tuned per adapter and are not portable.** State this in §E and in
`MODEL_AND_HASH_LIMITATIONS.md`; cross-provider verdict equivalence is **not** claimed.

**Decision & aggregation rules:**
- Per-frame, independent: a provider/pre-flight error sets `Status="error"`, `Error`, empty `Categories`.
- Per-category configurable threshold map (defaults differ; `SEXUAL`/`CSAM` strictest); two thresholds
  (`flag_at`, `block_at`) or `thresholds.default`.
- **Asset rollup [v2]:** the aggregator receives the **resolved per-category threshold map**
  (`flag_at`/`block_at`) — `block_at` is **not** on `CategoryResult`, so the rollup reads it from that map.
  Over `ok` frames: `Overall.Flagged` = any `CategoryResult.Flagged`; `MaxScore`/`Confidence` = max over
  **non-nil** scores (**`nil` if none**); `TopCategory` = category of that max (nil if none).
  **Verdict precedence is STRICT: `block` > `error` > `flag` > `allow`** — a genuine `block` wins even if
  other frames errored. Evaluate in that order: **`block`** if any `ok` category has
  `*Score ≥ block_at[category]` OR a list-membership match; else **`error`** if **any frame
  `Status="error"`**, **zero `ok` frames exist**, OR **every score across all frames is `nil`** (never
  `allow`; or a configurable explicit-review verdict); else **`flag`** if any `CategoryResult.Flagged`;
  else **`allow`**. **Zero-`ok`-frames case:** `Verdict=error`, `Flagged=false`,
  `MaxScore`/`Confidence`/`TopCategory`=`nil`. (Video default = any-frame; configurable "min flagged
  frames"/"N consecutive".)
- **Unknown/unsupported categories:** emit with `Score=nil` (**never `0`**).
- **Always populate `SchemaVersion`, `ModelVersion`**; consumers must tolerate **unknown future `Category`
  values as `OTHER`** (documented contract, not implementation detail). Compatibility: additive fields /
  additive Category values = minor bump; remove/rename/meaning-change = major bump.

**Testing the normalizer (§J):** capture each provider's raw JSON as a fixture → normalize → compare
`*.golden` (`-update` to regenerate). **[v2] Ship at least two worked input→`NormalizedResult` examples**
in `testdata/` even for v1.1 providers: (1) a **single-category video-native** case proving non-emitted
categories are **absent/`nil`, not 0**; (2) a **hierarchical/sparse** case (AWS-style) proving the
`OTHER`-fallback + nil discipline. These are the executable contract for future adapter authors.

---

# §F — CONFIG, ERRORS, OBSERVABILITY

### F.1 — Config (`viper`)
Keys: `adapter.name` + `adapter.options`; two-level per-category thresholds
`thresholds.{category}.{flag_at,block_at}` + `thresholds.default.{flag_at,block_at}` (**per-adapter**, not
portable; `SEXUAL` also carries `thresholds.SEXUAL.potential_csam` — §G.8); the Google likelihood lookup
table; `queue.driver` (`memory`|`redis`), `queue.workers`, `queue.buffer`, `queue.max_retries`,
`queue.retry_backoff`, `queue.drain_timeout`, `queue.job_timeout`, `queue.deadletter_max`,
`queue.redis.addr`; `frames.workdir`, **`frames.max_frames` (default `64`)**, `frames.concurrency`
(default `4`), strategy toggles; `log.level`; `metrics.addr`. **Secrets are env-only** (`VISMOD_` prefix, `.`→`_`);
**never in yaml.** **Fail fast at boot** if a selected adapter's required secret is missing. Ship a fully
annotated `config.example.yaml`.

### F.2 — Boot-time validation (fail fast)
Validate `ffmpeg`+`ffprobe` on PATH (videosift probe ⇒ `ErrNoBinaries`); validate the selected adapter's
credentials. **[v2]** When `queue.driver=redis`, validate **Redis reachability (PING)** at boot **and in
`/readyz`** (Redis is the SPOF — a Redis outage must flip readiness to not-ready, not silently black-hole
jobs).

### F.3 — Rate limiting & cost control
**[v2] The token-bucket limiter is owned by the single active `Moderator` (constructed in its Factory) and
is SHARED across all workers and all per-job fan-out** — aggregate request rate = limiter rate regardless
of `queue.workers × frames.concurrency`. `frames.concurrency`/`queue.workers` bound *parallelism*; the
limiter bounds *throughput* (default to the adapter's known quota, Azure F0 = 5 RPS). `frames.max_frames`
bounds per-video cost (cost = frames × per-call). **Multi-replica note (M5):** a per-process limiter ×N
replicas overshoots the quota ×N — the hardened path needs a Redis-backed shared limiter or a documented
`global_limit / replicas` budget (§L).

### F.4 — Retry / error classification & resilience **[v2]**
- **Retryable** (`429`, `5xx`, timeouts, transient net) → bounded backoff → dead-letter (`Disposition`).
- **Terminal** (`4xx` validation, unsupported/oversize media) → fail, no retry.
- **Panic / poison message:** every worker handler runs under `recover` → `Verdict=error` → dead-letter;
  **never crash the pool.** Cap retries (`queue.max_retries`) so a deterministically-failing job lands in
  the DLQ after K attempts instead of looping. Enforce per-job timeout + the `frames.max_frames` hard cap.
- Surface the provider error code (e.g. Azure `x-ms-error-code`).

### F.5 — 🔴 Fail-safe policy (security-critical) **[v2]**
- On provider/frame failure after retries, **never emit `Verdict=allow`** — emit `error`, route to
  dead-letter / human review. A partially-errored video follows the §E rollup (never `allow` while any
  frame errored).
- **Surge / outage backpressure:** on **sustained** provider failure — defined as **≥`N` consecutive
  provider errors OR provider error-rate ≥`X`% over a rolling window `W`** (all configurable; defaults e.g.
  `N=20`, `X=50`, `W=60s`) — **stop accepting new jobs** (readiness flips to not-ready; ingress rejects with
  a retryable signal) rather than failing every job into the review queue. **Restore** ready only after
  **`M` consecutive successes** (hysteresis, default `M=5`) — otherwise an outage builds an undrainable
  human-review backlog.
- **Bounded dead-letter:** cap DLQ depth; at capacity reject new enqueues + alert (never drop, never
  auto-allow). Emit metrics + alerts on **DLQ depth** and **review-backlog age**.
- The fail-safe override is gated behind an explicit **non-default** flag that emits a prominent **audit
  event** so it can't be flipped casually.

### F.6 — Observability **[v2]**
`log/slog` structured logging (job id, adapter, latency, verdict — **never log media bytes, PII, `Raw`
free-text, OCR, or captions**). **Prometheus text exposition on `metrics.addr` `/metrics`**, names e.g.
`vismod_jobs_total{verdict}`, `vismod_adapter_request_seconds{adapter}`,
`vismod_adapter_errors_total{adapter,code}`, `vismod_queue_depth`, `vismod_deadletter_depth`. Queue depth
comes from `Queue.QueueDepth` (uniform across drivers). `serve` exposes `/healthz` (liveness) and
`/readyz` (readiness incl. boot validation + Redis when applicable).

---

# §G — 🔴 RESPONSIBLE-USE, SAFETY & LICENSING (acceptance criteria, not optional)

This tool may encounter illegal content (especially **CSAM**). **Frame all legal references as design
drivers, with a prominent "this is not legal advice — consult counsel" disclaimer.**

1. **License: Apache-2.0** (explicit patent grant + retaliation + `NOTICE`; matches ROOST/Osprey). Ship
   `LICENSE` + `NOTICE`.
2. **Never persist or transmit the illegal media itself.** Operate on hashes/derivatives + per-job
   transient working copies deleted promptly (the §D.4 cleanup contract). Originals leave the system
   **only** via a lawful channel (e.g. **NCMEC CyberTipline**, US). Encrypt-at-rest + strict access
   control for any transiently held flagged material. **[v2]** Durable queue payloads (asynq/Redis) and any
   operator UI (`asynqmon`) **must carry opaque IDs/refs, never media bytes**, and be access-controlled.
3. **CSAM is handled by the §D.7 hash-match seam, not the classifier.** v1 ships the **seam + the
   `CSAM_HASH_MATCH` category + the `match_type`/`match_list` schema fields + docs**; the matcher (Meta
   **PDQ** image / **TMK+PDQF**/**vPDQ** video, BSD via `facebook/ThreatExchange`/HMA; lists in
   PhotoDNA/PDQ/MD5/SHA-1) is **v1.1**. **PhotoDNA is licensed-access only — never bundle it.** A hash hit
   is binary list-membership (`Score=nil`), never `1.0`.
4. **Human-in-the-loop:** no fully-automated consequential action on a positive match; borderline/
   low-confidence → human review.
5. **Tamper-evident audit log [v2 — concretized]** (`internal/audit`): append-only, hash-chained. Each
   record `= {seq, timestamp, prev_hash, payload, entry_hash}` where
   `entry_hash = SHA-256(seq‖timestamp‖prev_hash‖canonical(payload))`; genesis `prev_hash` = all-zeros;
   `O_APPEND`, reject out-of-order `seq`; **idempotent per `JobID`**. **[v2] Determinism (so `audit verify`
   recomputes byte-identical hashes cross-process):** `canonical(payload)` = **RFC 8785 JCS** (sorted-key,
   UTF-8, compact JSON); the `‖` fields are **length-prefixed** (no ambiguous bare concat) with fixed
   encodings — `seq` as 8-byte big-endian, `timestamp` as RFC 3339 UTC nanoseconds, `prev_hash` as the raw
   32 hash bytes. **Idempotency rule:** under the append lock, look up `JobID` in an index **first**; if
   present, **skip the append** (no new `seq`) so the chain carries no duplicate/gap; `seq` increments
   **only** on a real append. The `payload` binds the decision to
   its inputs **by hash** — store `SHA-256(Raw)` + `ModelIdentity` (adapter+version+threshold `ConfigHash`)
   + verdict, **never `Raw` itself** (§G.2). Ship **`vismod audit verify`** (recomputes the chain, reports
   the first broken link). **Honestly scoped:** a bare chain detects truncation/in-place edits, **not** a
   full-chain rewrite by a write-capable insider — document this in `SECURITY.md` and define a seam for
   HMAC/Ed25519 signing with an externally-held key OR periodic head-hash anchoring to WORM/external
   storage as the tamper-*resistant* upgrade.
6. **Configurable thresholds + transparency on limits.** Document perceptual-hash evadability
   (>99.9% black-box) and classifier false-positives/bias. Make the precision/recall tradeoff tunable.
7. **Score output is NOT treated as an evasion-oracle risk in v1 [v2].** The per-category `Score` + exact
   `Threshold` output is theoretically an evasion oracle (an adversary reads distance-to-threshold), but
   the underlying scanning models are **already publicly available for testing**, so v1 deliberately does
   **not** suppress scores or ship any architectural control. v1 emits full scores/thresholds; note this
   residual risk plainly in `SECURITY.md`. **No `antiabuse.*` config key or output mode ships in v1.**
8. **v1 has NO CSAM detection — define the safe failure mode [v2].** The `Sexual` classifier **will**
   sometimes fire on content that is actually CSAM. A high-severity `Sexual` hit is **not** a CSAM
   determination and **must be handled as potential-CSAM**. **Trigger (concrete):** a `SEXUAL`
   `CategoryResult` with normalized `Score ≥ thresholds.SEXUAL.potential_csam` (config key; default `0.667`
   = Azure severity 4). **Divert path:** on trigger the pipeline diverts the frame to the potential-CSAM
   channel **before `Sink.Write`** — the ordinary envelope/audit record stores only `SHA-256(frame)` +
   verdict (never the frame bytes or `Raw`), and the frame goes to human review under the §G.2
   transient-handling + encrypt-at-rest rules; **do not persist the frame in ordinary result/audit
   storage**; surface jurisdictional reporting guidance. `RESPONSIBLE_USE.md` must state operators needing
   CSAM coverage cannot rely on this tool until the v1.1 matcher ships.
9. **Docs that MUST ship:** `README.md`, `LICENSE` (Apache-2.0), `NOTICE`, `SECURITY.md` (incl. SSRF,
   audit-log threat scope, anti-abuse residual risk), `RESPONSIBLE_USE.md` (not-legal-advice disclaimer +
   reporting guidance + "do not test against real CSAM" + the §G.8 potential-CSAM policy),
   `MODEL_AND_HASH_LIMITATIONS.md` (incl. cross-provider non-portability), `CONTRIBUTING.md`,
   `CODE_OF_CONDUCT.md`, `config.example.yaml`.

*ROOST positioning (README, institutional facts only — no leadership names):* ROOST is a 501(c)(3)
launched Feb 2025 (Paris AI Action Summit, ~$27M, backers incl. Google/OpenAI/Discord/Roblox) making T&S
infrastructure "open, shared, and auditable." Position this pipeline as the **Classification** stage that
feeds a Coop-style review console / Osprey-style rules engine; the `awesome-safety-tools` taxonomy is
Hash Matching → Classification → Review → Investigation.

---

# §H — BUILD SEQUENCE (milestones; keep `main` green)

- **M0 — Runnable skeleton (day one, no credentials):** `go mod init`; deps; add videosift via `@latest`
  + a local `replace => ../videosift` for tandem dev (§B policy — track latest, never pin). Define
  `pkg/moderation` types (§D.1, §E incl. nullable scores + schema_version + hash-match fields). Registry +
  `AdapterConfig` (§D.2) + a **`stub` adapter** (deterministic; emits one unmappable provider label to
  exercise the `OTHER` fallback, and stamps a valid `ModelIdentity`/`ConfigHash`). `memq` **with a real DLQ + bounded retry +
  panic recovery** (§D.3/§F.4), `internal/pipeline` (per-frame non-fatal fan-out, §D.6), no-op
  `HashMatcher` pre-stage (§D.7), `result` JSONL sink (idempotent per JobID), cobra
  `scan`/`serve`/`adapters`/`audit`/`version`. **Result:** `vismod scan x.jpg` and `vismod serve` run
  end-to-end. Tests: registry; threshold→verdict; **per-frame fail-safe rollup**; memq FIFO + DLQ + drain;
  panic dead-letters.
- **M1 — Azure adapter (§C):** direct-REST client, request/response structs, normalize to §E, both auth
  modes, input pre-validation + `MaxImageBytes`, shared rate limiter, retry/error classification.
  Tests: `httptest`, golden-file normalization.
- **M2 — Video via videosift (§B):** `FrameSource` (absolute caller-owned `WorkDir` + `defer cleanup`,
  boot probe), lazy-decode fan-out, video aggregation, `ErrNoFrames`/`*FFmpegError`→`error` (fail-safe).
- **M3 — Docker (§I)** + boot validation (§F.2) + observability (§F.6 `/metrics`, `/healthz`, `/readyz`).
- **M4 — Responsible-use & docs (§G):** all docs, the **concretized audit log + `audit verify`**,
  `config.example.yaml`, potential-CSAM handling (§G.8).
- **M5 — Hardening / scale:** `asynqq` (Redis) behind `driver=redis` (idempotency, payload hygiene, Redis
  readiness, drain-on-deploy); the multi-replica model-identity + shared-limiter story (§L); CI; golden
  coverage for every adapter; a 2nd adapter to validate the normalization seam.

---

# §I — DOCKER (§A.5) **[v2]**

Multi-stage. **Stage 1:** pin the builder to satisfy videosift's `go 1.26.3` — e.g.
**`golang:1.26-bookworm`** (not loose `golang:1.x`); `go build -trimpath -ldflags='-s -w' CGO_ENABLED=0
GOOS=linux`. **🔴 Stage 2 runtime MUST include `ffmpeg`+`ffprobe`** (videosift execs them — `distroless/static`
is insufficient): `debian:bookworm-slim` or `alpine` + `apk add --no-cache ffmpeg` (provides `ffprobe`),
**non-root**. `CGO_ENABLED=0` makes the musl/glibc base choice irrelevant. Ensure `frames.workdir` is a
**writable, non-root-owned, ephemeral path** (tmpfs/`emptyDir`/`VOLUME`) so a read-only-rootfs container
can still create the videosift `WorkDir`. Add `HEALTHCHECK` hitting `/healthz` and `STOPSIGNAL SIGTERM`
(graceful drain — §D.3). One image, both modes: `ENTRYPOINT ["/vismod"]`, default `CMD ["serve"]`;
one-shot via `docker run <img> scan /data/clip.mp4`.

---

# §J — TESTING & CI

Stub-first runnability (no net/creds); table-driven (threshold→verdict, registry, config precedence,
**§E rollup incl. all-nil and partial-error cases**); interface fakes (`fakeModerator`, `fakeFrameSource`,
in-memory `Sink`); golden files (`testdata/<provider>/*.json`→normalize→`*.golden`, `-update`) **including
the two worked §E examples**; `httptest` for every HTTP client incl. retry/backoff/error-mapping; queue
FIFO + DLQ + drain + **panic-dead-letters** + **idempotent redelivery**; `audit verify` round-trip. CI
(M5): `go vet`, `golangci-lint`, `go test ./... -race`, `go build`, Docker build, on push/PR.

---

# §K — ACCEPTANCE CRITERIA (Definition of Done) **[v2 — expanded]**

1. `vismod scan <image>`/`<video>` and `vismod serve` run end-to-end and emit a valid envelope; `serve`
   drains gracefully on SIGINT/SIGTERM (no enqueued job left both unacked and uncleaned).
2. Switching `adapter.name` between `stub`/`azure` selects the model at startup with **no code change**;
   unknown adapter fails fast listing registered names.
3. Azure maps `0/2/4/6`→`severity/6`, applies configurable thresholds; golden-file test proves stability.
4. Video path uses videosift with an **absolute caller-owned `WorkDir`** and `defer cleanup` runs on every
   exit path; `ErrNoBinaries` surfaces at boot; `ErrNoFrames`/extraction errors yield `Verdict=error`
   (never allow), and cleanup completes **before** ack.
5. **Fail-safe:** provider failure → `Verdict=error` (never `allow`) + dead-letter; a partial video with
   any errored frame never yields `allow`; a worker **panic dead-letters and the pool keeps running**;
   sustained outage flips readiness (backpressure) instead of flooding review.
6. **Schema:** every envelope carries `schema_version`; `MaxScore`/`Confidence` are `nil` (not `0.0`) when
   no non-nil score exists; an all-`nil`/unsupported result never yields `allow`; unknown future
   `Category` is tolerated as `OTHER`.
7. **Queue swap is behavior-preserving:** the same handler `Disposition` produces the same retry/DLQ
   outcome on memq and asynq; memq exercises a real DLQ in the prototype; re-enqueuing a completed `JobID`
   does **not** double-write Sink or audit.
8. **Safety:** Apache-2.0; all §G docs ship; the `CSAM_HASH_MATCH` seam + category + `match_*` fields exist
   (no-op matcher); a **descriptive-output adapter does not leak `Raw` free-text/OCR/captions into Sink or
   audit** (audit stores `SHA-256(Raw)` + `ModelIdentity`, not `Raw`); `vismod audit verify` detects a
   tampered link; a high-severity `Sexual` result does not persist the frame into ordinary Sink/audit
   storage; secrets are env-only.
9. Docker image runs both modes, contains `ffmpeg`+`ffprobe`, non-root, writable workdir, `/healthz`
   healthcheck. README documents the FIFO completion-order caveat, memq non-durability, cross-provider
   score non-portability, and ROOST positioning. `/metrics` exposes the named counters.

---

# §L — SCOPE DECISIONS / NON-GOALS **[v2]**

- **v1 = single-replica `serve`.** Multi-replica horizontal scaling is **M5**, and when added MUST: (a)
  require `driver=redis` (memq is single-process); (b) enforce **one active model cluster-wide** — stamp
  `ModelIdentity` into every envelope + audit record (already mandated) and have workers **dead-letter**
  (not silently process) jobs whose required model identity ≠ the worker's loaded model; (c) use
  single-model rolling deploys (drain-and-replace, or asynq queue-name namespacing per model version); (d)
  use a Redis-backed **shared** rate limiter (or documented per-replica budget); (e) document Redis
  topology/HA (it is the SPOF).
- **v1 implements classifier-based moderation only.** CSAM perceptual-hash matcher (PDQ/TMK) is
  architected-for + documented in v1, implemented **v1.1** (§D.7/§G.3).
- **v1 adapters = `stub` + `azure`.** Hive/Google/AWS designed-for via the §E seam, implemented later.
- **Ollama / local-vision adapter** (e.g. `llava`/`llama-vision` via `/api/chat`) is a first-class future
  adapter — but **note the `Raw` hazard (§G.2/§F.6):** its natural-language image descriptions must be
  stripped from `Raw`/logs/audit. v1 follows the explicit "Azure first" requirement.
- **No hot-swapping** the active model (restart-to-change accepted). **No batch path** in v1 (Caps
  `SupportsBatch` dropped). **Non-US legal regimes** (EU CSA Reg, UK OSA, IWF/INHOPE) noted in docs, not
  encoded.

---

## SOURCES (verified during research — do not re-research)
- videosift `github.com/matthupy/videosift` — API surface reviewed at commit `66e5f27` for provenance
  **only** (`go.mod`, `strategies.go`, `types.go`, `extract.go`, `errors.go`, README). **Track latest, do
  not pin** — actively co-developed (§B dependency policy).
- Azure `learn.microsoft.com` Content Safety REST `image:analyze` (`2024-09-01`), harm-categories,
  quickstart-image, overview, FAQ; no Go SDK for the data plane.
- Other providers (normalization design): Hive `docs.thehive.ai`; Google Vision SafeSearch & Video
  Intelligence `cloud.google.com`; AWS Rekognition `DetectModerationLabels`/`GetContentModeration`
  (`AggregateBy` = `TIMESTAMPS|SEGMENTS`, `TaxonomyLevel ≥ 0`).
- Safety/legal: `roost.tools`, `github.com/roostorg/{osprey,awesome-safety-tools}`,
  `facebook/ThreatExchange` (PDQ/TMK/vPDQ/HMA, BSD), PhotoDNA, 18 U.S.C. §2258A / REPORT Act
  (**design drivers, not legal advice**), perceptual-hash evasion research (arXiv:2106.09820).
- Go stack: `spf13/cobra` v1.9.1 + `viper`, `hibiken/asynq`, `golang.org/x/sync/errgroup`,
  `prometheus/client_golang`, distroless/runtime base-image guidance.
- **Architecture review (v2):** 5-lens senior-architect pass (boundaries, dataflow/failure, scale/ops,
  normalization/extensibility, safety/red-team) — all lenses "sound-with-fixes"; this v2 applies the
  must-fix + should-fix set.
```
