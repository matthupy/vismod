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

## What each workflow samples

The diagrams below are **reference sketches, not measurements** — no real
video was scanned to produce them. They exist to show the *shape* of each
sampling strategy against one imaginary clip.

The reference clip is 2:00 long. Each column is 2 seconds, and `^` marks
a shot boundary (eight of them, with scene scores between 0.56 and 0.81):

```
|      ^          ^^^      ^            ^        ^   ^       |
0:00                                                      2:00
```

In the timelines that follow, `F` is a frame that reaches moderation and
`x` is a frame ffmpeg materialized that the scan cap discarded. The cap
shown is `max_frames: 50` from config.example.yaml.

**`scene-detect`** — `select='gt(scene,0.55)'`. One frame per shot
boundary; a static clip yields almost nothing, a fast-cut clip yields a
burst. Sampling tracks content, so frame count is unpredictable up front:

```
|      F          FFF      F            F        F   F       |
0:00                                                      2:00
```

Note the absence of a frame at 0:00 — the scene score is undefined for
the first frame, so it is not selected. Add `+eq(n,0)` to the `select`
expression if you need the opening frame.

**`keyframe`** — `-skip_frame nokey`. Samples I-frames, so the pattern is
dictated by the *encoder's* GOP structure, not by content. Sketched here
as a ~10s GOP plus the keyframes an encoder inserts at cuts:

```
|F    FF   F    F FFFF    FF   F    F   FF    F  F F F  F    |
0:00                                                      2:00
```

Cheapest to decode, but a re-encode with different GOP settings changes
the sample set for the same footage.

**`interval`** — `fps=1/2`. One frame every 2 seconds, regardless of
content. Predictable and uniform, and on this clip it is the only
standard workflow that overruns the cap: 60 frames extracted, the last
10 discarded, so nothing after 1:40 is ever scanned:

```
|FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFxxxxxxxxxx|
0:00                                                      2:00
```

Truncation keeps the earliest frames, which is why the scan cap is a cost
backstop and the fps value is the tuning lever (see rule 5 below).

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

Against the reference clip, raising the threshold from 0.55 to 0.6 drops
the three softest boundaries — the dissolves at 0:34, 0:52 and 1:36 —
leaving five frames where `scene-detect` took eight:

```
|      F           FF                   F            F       |
0:00                                                      2:00
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

`-t 30` sits *before* `-i`, so ffmpeg stops reading the input at 0:30 and
the remaining 90 seconds are never decoded. At 2 fps that is 4 frames per
column here, dense enough that the 50-frame scan cap lands at 0:25:

```
|FFFFFFFFFFFFFxx                                             |
0:00                                                      2:00
```

Triage workflows are the easiest way to blow the cap. Either lower the
fps or raise `max_frames` deliberately — the tail you lose is silent
apart from a warning.

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
