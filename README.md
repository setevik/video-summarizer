# video-summarizer

Writes a Gemini summary beside each video in a Jellyfin library, and prepends
it to the video's `.nfo` plot so Jellyfin shows it as the overview. Built for
a [ytdl-sub](https://github.com/jmbannon/ytdl-sub) YouTube library. It runs as
a separate daily job that scans the library, not as a download hook.

## What a run does

For each video, newest first:

1. **No `<video>.summary.md`:** send the YouTube URL, title and description to
   Gemini at low media resolution, and write `<video>.summary.md`. Then
   prepend the summary to the `.nfo` `<plot>`, above a
   `──── YouTube description ────` line with the original plot below it.
2. **`.summary.md` exists:** re-inject it if the `.nfo` lacks it (ytdl-sub
   regenerated the file) or if it differs (the summary was edited). No API call.
3. Ask Jellyfin to refresh every item whose `.nfo` changed.

- **`.summary.md` is the source of truth.** Hand edits reach the `.nfo` on the
  next run. Delete the file to regenerate the summary.
- **Video ID and URL:** from yt-dlp's `.info.json`, else the `[id]` at the end
  of the file name.
- **Transcript fallback:** used when Gemini cannot fetch the video (private or
  deleted) and for videos over `MAX_URL_SECONDS`. It sends the
  original-language `.srt` sidecar (manual or ASR), else a creator-provided
  one. Auto-translated tracks are never sent.
- **Failures** are counted in `$STATE_DIR/failures.json`. After `MAX_ATTEMPTS`
  the job stops trying a video; delete its entry to retry. Quota errors, 5xx
  responses, timeouts and network errors don't count.
- **Run-wide failures stop generation, not re-injection:** a rejected API key
  or project (the run exits non-zero), five transient errors in a row, or a
  summary that cannot be written.
- **Quota:** a 429 is retried once after a minute, then the run stops. A video
  over the run's video budget waits for the next run. It is never downgraded
  to its transcript just to save budget.

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
| `MAX_URL_SECONDS` | `10800` | longer videos go by transcript |
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
video-summarizer eval [-n 15] [-models a,b] [-modes video,transcript] OUTDIR
```

Run an `eval` before changing the model or prompt. It summarizes a fixed
sample spread across folders and lengths into `OUTDIR`, once per model and
input mode, and writes `report.md` with tokens, latency and links. It never
touches the library.

## Build

```
./build.sh v0.1.1 --push
```

The script runs the tests, then builds `ghcr.io/setevik/video-summarizer:<version>`
for amd64 and pushes it. Pushing needs `docker login ghcr.io` with a PAT that
has `write:packages`. The package is public: the code holds no secrets, and
hosts can pull without credentials.
