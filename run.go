package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

type runStats struct {
	Videos, Summarized, FromTranscript, Reinjected, Failed, GivenUp, Refreshed int
	VideoSeconds, PromptTokens, OutputTokens                                   int
	Stopped                                                                    string
}

type runner struct {
	cfg   Config
	sum   Summarizer
	jf    *jellyfin
	log   *slog.Logger
	sleep func(context.Context, time.Duration) error
}

// errStop ends generation for this run (quota or per-run cap); videos that
// already have a summary are still re-injected.
var errStop = errors.New("stop generating")

func (r *runner) run(ctx context.Context) (runStats, error) {
	var st runStats
	videos, err := scan(r.cfg.Roots)
	if err != nil {
		return st, err
	}
	st.Videos = len(videos)
	fails, err := loadFailures(r.cfg.StateDir)
	if err != nil {
		return st, err
	}
	var changed []string
	calls := 0
	for _, v := range videos {
		if ctx.Err() != nil {
			break
		}
		md, err := os.ReadFile(v.SummaryPath())
		switch {
		case err == nil:
			// Step 7: the summary exists; make sure the .nfo carries it, which
			// it will not after ytdl-sub regenerates it.
			if c, err := r.inject(v, string(md)); err != nil {
				r.log.Warn("inject", "video", v.Name(), "err", err)
			} else if c {
				st.Reinjected++
				changed = append(changed, v.Path)
			}
			continue
		case !errors.Is(err, fs.ErrNotExist):
			r.log.Warn("read summary", "video", v.Name(), "err", err)
			continue
		}
		if st.Stopped != "" {
			continue
		}
		if f := fails.Entries[v.Base]; f.Count >= r.cfg.MaxAttempts {
			st.GivenUp++
			continue
		}
		if calls >= r.cfg.CallsPerRun {
			st.Stopped = "calls per run"
			continue
		}

		res, err := r.summarize(ctx, v, &st, &calls)
		switch {
		case errors.Is(err, errStop):
			st.Stopped = "quota"
			continue
		case isTransient(err):
			// A server-side failure says nothing about the video: retry
			// tomorrow without counting it against MaxAttempts.
			r.log.Warn("summarize, will retry", "video", v.Name(), "err", err)
			continue
		case err != nil:
			st.Failed++
			fails.record(v.Base, err)
			r.log.Warn("summarize", "video", v.Name(), "err", err)
			continue
		case res == "":
			continue // deferred, e.g. over today's video budget
		}
		fails.clear(v.Base)
		st.Summarized++
		if r.cfg.DryRun {
			fmt.Printf("=== %s\n%s\n", v.Name(), res)
			continue
		}
		if err := writeAtomic(v.SummaryPath(), []byte(res)); err != nil {
			return st, err
		}
		if c, err := r.inject(v, res); err != nil {
			r.log.Warn("inject", "video", v.Name(), "err", err)
		} else if c {
			changed = append(changed, v.Path)
		}
	}
	if !r.cfg.DryRun {
		if err := fails.save(); err != nil {
			r.log.Warn("save failures", "err", err)
		}
	}
	if r.jf != nil && len(changed) > 0 && !r.cfg.DryRun {
		n, err := r.jf.refresh(ctx, changed)
		st.Refreshed = n
		if err != nil {
			r.log.Warn("jellyfin refresh", "err", err)
		}
	}
	return st, nil
}

// inject writes the summary into the .nfo plot. A video whose .nfo does not
// exist yet (ytdl-sub writes it after the media file) is left for a later run.
func (r *runner) inject(v Video, md string) (bool, error) {
	if _, err := os.Stat(v.NFOPath()); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return injectPlot(v.NFOPath(), plotText(md), r.cfg.DryRun)
}

// summarize returns the rendered .summary.md, "" to defer the video to a later
// run, or an error. Video input is the default; the transcript is the
// fallback for a video Gemini cannot fetch, and the only route for one longer
// than the context window allows.
func (r *runner) summarize(ctx context.Context, v Video, st *runStats, calls *int) (string, error) {
	m, err := loadMeta(v)
	if err != nil {
		return "", fmt.Errorf("metadata: %w", err)
	}
	tooLong := m.Duration > r.cfg.MaxURLSeconds
	var videoErr error
	if m.URL != "" && !tooLong {
		if st.VideoSeconds+m.Duration > r.cfg.VideoSecondsPerRun {
			// Do not fall back to the transcript just to save budget: the
			// video gives the better summary, and tomorrow is soon enough.
			return "", nil
		}
		st.VideoSeconds += m.Duration
		s, u, err := r.call(ctx, calls, func() (Summary, Usage, error) { return r.sum.FromVideo(ctx, m) })
		r.account(st, u)
		if err == nil {
			r.log.Info("summarized", "video", v.Name(), "source", "video", "prompt_tokens", u.PromptTokens, "latency", u.Latency.Round(time.Second))
			return s.Markdown(Provenance{Model: r.cfg.Model, Source: "video", Generated: time.Now()}), nil
		}
		if errors.Is(err, errStop) || isTransient(err) {
			return "", err
		}
		videoErr = err
	}

	path, lang := pickTranscript(v, m)
	if path == "" {
		if videoErr != nil {
			return "", fmt.Errorf("video: %w; no usable transcript", videoErr)
		}
		return "", errors.New("no URL and no usable transcript")
	}
	// ~4 characters a token; 600k characters stays well inside the window.
	t, err := loadTranscript(path, 600_000)
	if err != nil {
		return "", err
	}
	s, u, err := r.call(ctx, calls, func() (Summary, Usage, error) { return r.sum.FromTranscript(ctx, m, t) })
	r.account(st, u)
	if err != nil {
		return "", err
	}
	st.FromTranscript++
	r.log.Info("summarized", "video", v.Name(), "source", "transcript:"+lang, "video_err", errString(videoErr), "prompt_tokens", u.PromptTokens)
	return s.Markdown(Provenance{Model: r.cfg.Model, Source: "transcript:" + lang, Generated: time.Now()}), nil
}

// call spaces requests out and retries a 429 once after a minute, which is
// enough for the per-minute limit. A second 429 is the daily one: stop.
func (r *runner) call(ctx context.Context, calls *int, f func() (Summary, Usage, error)) (Summary, Usage, error) {
	if *calls >= r.cfg.CallsPerRun {
		return Summary{}, Usage{}, errStop
	}
	for attempt := 0; ; attempt++ {
		if *calls > 0 {
			if err := r.sleep(ctx, r.cfg.CallInterval); err != nil {
				return Summary{}, Usage{}, err
			}
		}
		*calls++
		s, u, err := f()
		if !isQuota(err) {
			return s, u, err
		}
		if attempt == 1 {
			r.log.Warn("quota exhausted, stopping for this run", "err", err)
			return s, u, errStop
		}
		if err := r.sleep(ctx, time.Minute); err != nil {
			return s, u, err
		}
	}
}

func (r *runner) account(st *runStats, u Usage) {
	st.PromptTokens += u.PromptTokens
	st.OutputTokens += u.OutputTokens + u.ThoughtTokens
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
