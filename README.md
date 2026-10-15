# video-summarizer

Writes a Gemini summary beside each video in a Jellyfin library, and prepends
it to the video's `.nfo` plot so Jellyfin shows it as the overview. Built for
a [ytdl-sub](https://github.com/jmbannon/ytdl-sub) YouTube library. It runs as
a separate daily job that scans the library, not as a download hook.

## What a run does

For each video, newest first:

1. **No `<video>.summary.md`:** summarize it (see [Sources](#sources)), with
   the title, description and creator chapters as context, and write
   `<video>.summary.md`. Then put the summary at the top of the `.nfo`
   `<plot>`, with the original plot below it (see [Plot layout](#plot-layout)).
2. **`.summary.md` exists:** re-inject it if the `.nfo` lacks it (ytdl-sub
   regenerated the file) or if it differs (the summary was edited). No API call.
3. Ask Jellyfin to refresh every item whose `.nfo` changed.

- **`.summary.md` is the source of truth.** Hand edits reach the `.nfo` on the
  next run. Delete the file to regenerate the summary.
- **Video ID and URL:** from yt-dlp's `.info.json`, else the `[id]` at the end
  of the file name.
- **Shorts** are skipped with `SKIP_SHORTS=true`: no call, no summary, no
  failure. A Short is a video yt-dlp fetched from a channel's `/shorts` tab,
  or one that is vertical and at most 3 minutes long. That needs
  `.info.json`; without it a video is never taken for a Short. `plan` lists
  them as `skip:short`.
- **Failures** are counted in `$STATE_DIR/failures.json`, with the reason
  each source was passed over. After `MAX_ATTEMPTS` the job stops trying a
  video, until its `.srt` sidecars change (a backfilled subtitle, say), which
  starts the count over. Delete its entry to retry sooner. Quota errors, 5xx
  responses, timeouts and network errors don't count.
- **Run-wide failures stop generation, not re-injection:** a rejected API key
  or project (the run exits non-zero), five transient errors in a row, or a
  summary that cannot be written.
- **Quota:** a 429 is retried once after a minute, then the run stops. A video
  that needs video input but is over the run's video budget waits for the
  next run. The run's first video always fits, so one longer than the budget
  still gets its turn.

## Prompt context

- **Description:** sent as context. The model is told viewers see it right
  below the summary, so it shouldn't restate it.
- **Creator chapters** (from `.info.json`): key points follow them and use
  their exact start times. Intro, sponsor and outro chapters are skipped.
  Without chapters, the model places timestamps itself.

## Plot layout

`PLOT_FORMAT` sets how the summary and the original plot share the `.nfo`
`<plot>`. Switching re-injects every `.nfo` once, with no API calls.

- **`text`** (default): plain text, `•` bullets, then a
  `──── YouTube description ────` line and the original plot. Reads the same
  in every client.
- **`markdown`:** jellyfin-web renders the overview as Markdown (markdown-it
  with HTML, then DOMPurify), and so do the apps built on it (Android, iOS,
  desktop). The plot is the summary paragraph, a `#### Key points` list, and
  the original description folded in a `<details>` block, all on one line
  with `<br>` breaks, so Markdown can't reflow it. An invisible
  `<!-- video-summarizer -->` comment marks the plot as injected. Native
  clients (Android TV, Swiftfin, Kodi) show the raw markup.

The summary opens with its paragraph, not a heading, because season episode
lists render the overview too.

## Sources

Two inputs, tried in `PREFER_SOURCE` order (default `transcript`):

- **Transcript:** the video's original-language `.srt` sidecar, manual or
  speech recognition, else a creator-provided one. Machine translations are
  never sent.
- **Video:** the YouTube URL at low media resolution (~95 tokens/s). Needs a
  URL, a length within `MAX_URL_SECONDS`, and room in `VIDEO_SECONDS_PER_RUN`.

Transcripts cost about 1/25 of video and, in an eval on 15 library videos,
summarized speech-led content (lectures, explainers, musicals) about as well.
Video adds only what is on screen. So the transcript goes first, and video
covers what it cannot:

| Transcript | What happens |
|---|---|
| none in the original language | video |
| no speech, or under `MIN_TRANSCRIPT_WPM` (10) | video: an animation montage or a visual-gag sketch |
| last caption before `MIN_TRANSCRIPT_COVERAGE` (50%) of the video | video: captions that stop partway. Skipped under 2 min |
| unreadable | video |
| usable, but the model call fails on it | video |
| no usable transcript, and no URL or over `MAX_URL_SECONDS` | failure |

Words are counted as letters / 5, so Russian, Hebrew and scripts without
spaces measure like English, and sound cues (`[Music]`, `(laughs)`) don't
count. The thresholds are calibrated on a 769-video library: speech runs
40–250 words/min and a sung musical 21, while montages and sketches sit at
0–8. `plan` prints each video's route and its transcript's words/min and
coverage.

## Configuration

Environment only.

| Variable | Default | |
|---|---|---|
| `GEMINI_API_KEY` | — | Gemini API key |
| `LIBRARY_ROOTS` | `/shared/Media/YouTube` | comma-separated; must be the paths Jellyfin sees |
| `STATE_DIR` | `/state` | writable |
| `MODEL` | `gemini-3.8-flash` | |
| `SUMMARY_LANGUAGE` | `English` | whatever the video's language |
| `THINKING_LEVEL` | `LOW` | |
| `VIDEO_SECONDS_PER_RUN` | `27000` | URL input budget; the free tier allows 8 h/day |
| `CALLS_PER_RUN` | `200` | `0` pauses generation; re-injection still runs |
| `PLOT_FORMAT` | `text` | `text` for any client, `markdown` for jellyfin-web and the apps built on it (see below) |
| `SKIP_SHORTS` | `false` | leave YouTube Shorts unsummarized (see below) |
| `PREFER_SOURCE` | `transcript` | `transcript` or `video`: which source to try first |
| `MIN_TRANSCRIPT_WPM` | `10` | below this, a transcript is not used |
| `MIN_TRANSCRIPT_COVERAGE` | `0.5` | last caption before this fraction of the video: not used |
| `MAX_URL_SECONDS` | `10800` | longer videos can only go by transcript |
| `CALL_INTERVAL` | `7s` | spacing between calls |
| `CALL_TIMEOUT` | `10m` | upper bound on one call; a timeout is retried next run |
| `MAX_ATTEMPTS` | `3` | |
| `JELLYFIN_URL`, `JELLYFIN_API_KEY` | — | unset: no refresh, Jellyfin picks changes up on its next scan |
| `RUN_AT` | `10:30` | daemon's daily run, container `TZ` |
| `RUN_ON_START` | `false` | |
| `DRY_RUN` | `false` | print summaries, write nothing |

**Vertex AI instead of an API key:** set `GOOGLE_GENAI_USE_VERTEXAI=true`,
`GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION` and Application Default
Credentials. The `genai` SDK reads these itself. The Go SDK only accepts ADC
for Vertex: the Gemini API backend requires a key.

## Jellyfin requirements

- Items are matched by path, so mount the library where Jellyfin has it.
- The refresh uses `FullRefresh` + `replaceAllMetadata`, the only mode
  guaranteed to re-read a changed `.nfo`. That is safe only in a library with
  **no metadata fetchers** enabled.
- Turn **save metadata to media folders** off, or Jellyfin may write over the
  injected plot.

## Commands

```
video-summarizer daemon   # a pass every day at RUN_AT (the image's default)
video-summarizer run      # one pass now
video-summarizer plan     # backlog and each video's route, no API calls
video-summarizer eval [-n 15] [-models a,b] [-modes video,transcript] [-dry-run] OUTDIR
```

Run an `eval` before changing the model or prompt. It summarizes a fixed
sample spread across folders and lengths into `OUTDIR`, once per model and
input mode, and writes `report.md` with tokens, latency and links. It never
touches the library.

## Build

```
./build.sh --push
```

The script runs the tests, then builds `ghcr.io/setevik/video-summarizer:<version>`
for amd64 and pushes it, moving `:latest` to it unless it is older than an
existing version. The version is the latest `vX.Y.Z` git tag with its
patch bumped; pass `minor`, `major` or an explicit `vX.Y.Z` to override. A push
also tags the commit with the version and pushes the tag, so it needs a clean
working tree. Pin the printed version in the deployment.

Pushing needs `docker login ghcr.io` with a PAT that has `write:packages`. The
package is public: the code holds no secrets, and hosts can pull without
credentials.
