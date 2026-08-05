---
title: Custom FFmpeg workflows
nav_order: 11
---

# Writing custom FFmpeg workflows

Frame extraction is configuration-driven: a **workflow** is a named,
parameterized FFmpeg **argument-list template**. Three standard
workflows (`scene-detect`, `keyframe`, `interval`) are part of the
built-in default configuration — ordinary `ffmpeg.workflows` entries you
can override by name, replace, or extend with your own (see
config.example.yaml, where they appear in full).

> **Trust boundary.** Workflows are operator-authored configuration.
> vismod validates them hard (below), but treat workflow config like
> deployment code: review changes, and never template untrusted input
> into a workflow. The guardrails are the security contract
> (SECURITY.md).

## Anatomy

```yaml
ffmpeg:
  default_workflow: every-5s
  max_frames: 64          # REQUIRED > 0; hard cap regardless of workflow
  timeout: 120s           # extraction wall-clock cap
  workflows:
    every-5s:
      description: "One frame every 5 seconds, downscaled."
      args: ["-hide_banner","-nostdin","-y",
             "-i","{{.Input}}",
             "-vf","fps=1/5,scale={{.MaxWidth}}:-1,showinfo",
             "-frames:v","{{.MaxFrames}}",
             "{{.WorkDir}}/frame-%06d.png"]
```

Each element of `args` is rendered through Go `text/template` and passed
to `exec.CommandContext` as one argument. **There is no shell**: no
quoting tricks, no `$()`, no pipes.

## The placeholder set (complete)

| Placeholder      | Meaning                                                        |
|------------------|----------------------------------------------------------------|
| `{{.Input}}`     | absolute path of the job's local video file                    |
| `{{.WorkDir}}`   | absolute, pipeline-owned output directory                      |
| `{{.MaxFrames}}` | the EXTRACTION budget (`max_extract_frames`, default 4×`max_frames`) |
| `{{.MaxWidth}}`  | `ffmpeg.max_width` for scale filters                           |

Any other placeholder fails validation.

## Rules enforced by `vismod workflows validate`

1. Exactly one `-i` followed by exactly `{{.Input}}`, and `{{.Input}}`
   appears nowhere else. You cannot add a second input.
2. No remote or indirect protocols anywhere: `http:`, `https:`,
   `rtmp*:`, `concat:`, `pipe:`, `subfile:` (any comma-option form),
   `data:`, `tcp:`, `udp:`, `file:`, chained protocols, or `://`. Only
   plain local paths.
3. The **last argument is the output pattern** and must start with
   `{{.WorkDir}}/`. `{{.WorkDir}}` may not appear in any other argument,
   no other argument may be an absolute path, and `..` traversal is
   rejected.
4. `-nostdin` is enforced (prepended automatically if you omit it).
5. `ffmpeg.max_frames > 0` is required. Two caps apply in sequence:
   the **extraction budget** (`max_extract_frames`, default
   4×`max_frames`) bounds how many PNGs may be materialized on disk —
   excess is deleted with a warning; then, AFTER dedup and any other
   post-processing, the **scan cap** (`max_frames`) bounds how many
   frames reach moderation. Cap-after-dedup means near-duplicates never
   consume scan budget. Hitting the scan cap logs a warning because
   truncation keeps the earliest frames — the video's tail goes
   unscanned, so treat `max_frames` as a cost backstop, not the tuning
   lever.

Validation runs at `serve` boot, before any video `scan`, and on demand
via `vismod workflows validate`. A workflow that fails validation stops
the process — by design, misconfiguration is an operator error, not a
per-job mystery.

## Selecting workflows per job

The config's `default_workflow` applies when a job doesn't say
otherwise. Any job may select one or MORE workflows explicitly; frames
are the union across them, in selection order, each workflow writing to
its own subdirectory of the job WorkDir:

- CLI: `vismod scan --workflow interval --workflow keyframe clip.mp4`
  (repeatable or comma-separated)
- HTTP intake: `{"kind":"file","ref":"/data/clip.mp4",
  "media_type":"video","workflows":["interval","keyframe"]}`

Selections are validated against the configured workflow set at intake
(unknown names are rejected with 400 / a CLI error) and re-checked at
execution. `ffmpeg.max_frames` caps the TOTAL frames per video across
all selected workflows.

Combining workflows often re-extracts the same moments. Enable
`frames.dedup` to drop near-duplicate frames (dHash Hamming distance ≤
`hamming_threshold`) before they spend moderation calls. The threshold
is also configurable per job: `"dedup_threshold": 0..64` in the intake
body (or `scan --dedup-threshold N`) enables dedup at that distance for
that job even when it's off globally, and `-1` disables it for the job.

## Timestamps

Include `showinfo` at the end of your `-vf` filter graph (the shipped
defaults do) and vismod recovers real per-frame timestamps (`pts_time`)
from ffmpeg's log. Without `showinfo`, `timestamp_sec` degrades to the
frame's ordinal index.

## Worked examples

One frame per shot, capped, high threshold for fast cuts:

```yaml
hard-cuts:
  description: "Frames on hard cuts only (scene threshold 0.6)."
  args: ["-hide_banner","-nostdin","-y","-i","{{.Input}}",
         "-vf","select='gt(scene,0.6)',scale={{.MaxWidth}}:-1,showinfo",
         "-vsync","vfr","-frames:v","{{.MaxFrames}}",
         "{{.WorkDir}}/frame-%06d.png"]
```

First 30 seconds only (cheap triage of long uploads):

```yaml
head-sample:
  description: "2 fps over the first 30 seconds."
  args: ["-hide_banner","-nostdin","-y","-t","30","-i","{{.Input}}",
         "-vf","fps=2,scale={{.MaxWidth}}:-1,showinfo",
         "-frames:v","{{.MaxFrames}}",
         "{{.WorkDir}}/frame-%06d.png"]
```

Things that will (correctly) fail validation:

```yaml
# second input          args: [...,"-i","{{.Input}}","-i","overlay.png",...]
# network output        args: [...,"http://example.com/upload"]
# reading other files   args: [...,"-f","concat:/etc/passwd",...]
# escaping the workdir  args: [...,"/tmp/frames-%d.png"]
```

## Fail-safe behavior

Any ffmpeg/ffprobe failure, a missing binary, or **zero frames
extracted** yields `verdict: "error"` and dead-letters the job. Zero
frames is never treated as clean — a static or looping harmful video
must not pass by producing no frames.
