package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"
)

type runStats struct {
	Videos, Summarized, FromTranscript, Reinjected, Failed, GivenUp, OverviewsSet, Skipped int
	VideoSeconds, PromptTokens, OutputTokens                                               int
	Stopped                                                                                string
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

// errSkip means the video is excluded by configuration (SKIP_SHORTS,
// SKIP_VIDEO_IDS): no call, no summary, no failure.
var errSkip = errors.New("excluded")

// errAuth also ends generation, without counting against the video: the
// credentials or project are at fault, and every video would fail alike.
var errAuth = errors.New("gemini credentials or project rejected")

// maxTransient consecutive transient errors end generation for the run: an
// outage would otherwise cost a call per video for nothing.
const maxTransient = 5

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
	var (
		// Every summarized video's plot, for Jellyfin: all of them, not just
		// those changed this run, so a drifted overview is set again.
		overviews = map[string]string{}
		calls     int
		transient int
		runErr    error
	)
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
			}
			r.collect(overviews, v)
			continue
		case !errors.Is(err, fs.ErrNotExist):
			r.log.Warn("read summary", "video", v.Name(), "err", err)
			continue
		}
		if st.Stopped != "" {
			continue
		}
		sidecars := sidecarSignature(v)
		if fails.givenUp(v.Base, sidecars, r.cfg.MaxAttempts) {
			st.GivenUp++
			continue
		}
		if calls >= r.cfg.CallsPerRun {
			st.Stopped = "calls per run"
			continue
		}

		res, err := r.summarize(ctx, v, &st, &calls)
		switch {
		case errors.Is(err, errSkip):
			st.Skipped++
			continue
		case errors.Is(err, errStop):
			st.Stopped = "quota"
			continue
		case errors.Is(err, errAuth):
			st.Stopped = "auth"
			runErr = err
			continue
		case isTransient(err):
			// A server-side or network failure says nothing about the video:
			// retry tomorrow without counting it against MaxAttempts.
			r.log.Warn("summarize, will retry", "video", v.Name(), "err", err)
			if transient++; transient >= maxTransient {
				st.Stopped = "transient errors"
			}
			continue
		case err != nil:
			st.Failed++
			fails.record(v.Base, sidecars, err)
			r.log.Warn("summarize", "video", v.Name(), "err", err)
			continue
		case res == "":
			continue // deferred, e.g. over today's video budget
		}
		transient = 0
		fails.clear(v.Base)
		st.Summarized++
		if r.cfg.DryRun {
			fmt.Printf("=== %s\n%s\n", v.Name(), res)
			continue
		}
		if err := writeAtomic(v.SummaryPath(), []byte(res)); err != nil {
			// Likely the disk or permissions, so the next video would fail
			// too. Stop generating, but still save state and update what
			// already changed.
			st.Stopped = "write error"
			runErr = fmt.Errorf("writing summary: %w", err)
			continue
		}
		if _, err := r.inject(v, res); err != nil {
			r.log.Warn("inject", "video", v.Name(), "err", err)
		}
		r.collect(overviews, v)
	}
	if !r.cfg.DryRun {
		if err := fails.save(); err != nil {
			r.log.Warn("save failures", "err", err)
		}
	}
	if r.jf != nil && !r.cfg.DryRun {
		n, err := r.jf.setOverviews(ctx, overviews)
		st.OverviewsSet = n
		if err != nil {
			r.log.Warn("jellyfin overviews", "err", err)
		}
	}
	return st, runErr
}

// collect records the plot a video's .nfo now holds, for Jellyfin.
func (r *runner) collect(overviews map[string]string, v Video) {
	plot, err := nfoPlot(v.NFOPath())
	if err == nil && plot != "" {
		overviews[v.Path] = plot
	}
}

// inject writes the summary into the .nfo plot. A video whose .nfo does not
// exist yet (ytdl-sub writes it after the media file) is left for a later run.
func (r *runner) inject(v Video, md string) (bool, error) {
	if _, err := os.Stat(v.NFOPath()); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	plot := plotText(md)
	if r.cfg.PlotFormat == "markdown" {
		plot = plotMarkdown(md)
	}
	return injectPlot(v.NFOPath(), plot, r.cfg.PlotFormat, r.cfg.DryRun)
}

// summarize returns the rendered .summary.md, "" to defer the video to a later
// run, or an error. Sources are tried in PREFER_SOURCE order:
//
//   - transcript: the original-language .srt, if checkTranscript passes it;
//   - video: the YouTube URL, if there is one, it is no longer than
//     MAX_URL_SECONDS, and today's video budget has room.
//
// A source that is unavailable or fails with an error about the video moves
// on to the other one. Quota, auth and transient errors end the attempt
// instead: they say nothing about the video.
func (r *runner) summarize(ctx context.Context, v Video, st *runStats, calls *int) (string, error) {
	m, err := loadMeta(v)
	if err != nil {
		return "", fmt.Errorf("metadata: %w", err)
	}
	if r.cfg.skipReason(m) != "" {
		return "", errSkip
	}
	tc := checkTranscript(r.cfg, v, m)
	order := []string{"transcript", "video"}
	if r.cfg.PreferSource == "video" {
		order = []string{"video", "transcript"}
	}
	var why []string // why each source was passed over, for the log or the error
	for _, src := range order {
		var (
			s   Summary
			u   Usage
			err error
		)
		switch src {
		case "transcript":
			if tc.Problem != "" {
				why = append(why, "transcript: "+tc.Problem)
				continue
			}
			s, u, err = r.call(ctx, calls, func() (Summary, Usage, error) { return r.sum.FromTranscript(ctx, m, tc.Text) })
			src = tc.Source()
		case "video":
			switch {
			case m.URL == "":
				why = append(why, "video: no URL")
				continue
			case m.Duration > r.cfg.MaxURLSeconds:
				why = append(why, "video: longer than MAX_URL_SECONDS")
				continue
			case st.VideoSeconds > 0 && st.VideoSeconds+m.Duration > r.cfg.VideoSecondsPerRun:
				// Over today's budget: try again tomorrow rather than settle
				// for a source already found wanting. The run's first video
				// always fits, or one longer than the budget never would.
				return "", nil
			}
			st.VideoSeconds += m.Duration
			s, u, err = r.call(ctx, calls, func() (Summary, Usage, error) { return r.sum.FromVideo(ctx, m) })
		}
		r.account(st, u)
		if err != nil {
			if errors.Is(err, errStop) || errors.Is(err, errAuth) || isTransient(err) {
				return "", err
			}
			why = append(why, src+": "+err.Error())
			continue
		}
		if src != "video" {
			st.FromTranscript++
		}
		r.log.Info("summarized", "video", v.Name(), "source", src, "passed_over", strings.Join(why, "; "),
			"prompt_tokens", u.PromptTokens, "latency", u.Latency.Round(time.Second))
		return s.Markdown(Provenance{Model: r.cfg.Model, Source: src, Generated: time.Now()}), nil
	}
	return "", errors.New(strings.Join(why, "; "))
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
		if isAuth(err) {
			return s, u, fmt.Errorf("%w: %w", errAuth, err)
		}
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
