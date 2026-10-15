// video-summarizer writes a Gemini summary beside each video in a Jellyfin
// library and prepends it to the video's .nfo plot. See
// README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const usage = `usage:
  video-summarizer run                 one pass over the library
  video-summarizer daemon              a pass every day at RUN_AT
  video-summarizer plan                what a run would do, without calling the model
  video-summarizer eval [flags] OUTDIR summarize a sample into OUTDIR, library untouched
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := dispatch(ctx, log, os.Args[1], os.Args[2:]); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, log *slog.Logger, cmd string, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch cmd {
	case "run", "daemon":
		g, err := newGemini(ctx, cfg)
		if err != nil {
			return err
		}
		r := &runner{cfg: cfg, sum: g, jf: newJellyfin(cfg), log: log, sleep: sleepCtx}
		if r.jf == nil {
			log.Warn("JELLYFIN_URL or JELLYFIN_API_KEY unset; Jellyfin will pick up summaries on its next scan only")
		}
		if cmd == "run" {
			return pass(ctx, r)
		}
		return daemon(ctx, r)
	case "plan":
		return runPlan(cfg)
	case "eval":
		fs := flag.NewFlagSet("eval", flag.ExitOnError)
		n := fs.Int("n", 15, "videos to sample")
		models := fs.String("models", cfg.Model, "comma-separated model IDs")
		modes := fs.String("modes", "video,transcript", "comma-separated: video, transcript")
		dryRun := fs.Bool("dry-run", false, "print the sample and estimated tokens, call nothing")
		fs.Parse(args)
		if fs.NArg() != 1 {
			return fmt.Errorf("eval: want one OUTDIR")
		}
		return runEval(ctx, cfg, splitList(*models), splitList(*modes), *n, fs.Arg(0), *dryRun)
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
}

func pass(ctx context.Context, r *runner) error {
	start := time.Now()
	st, err := r.run(ctx)
	r.log.Info("run done", "took", time.Since(start).Round(time.Second),
		"videos", st.Videos, "summarized", st.Summarized, "from_transcript", st.FromTranscript,
		"reinjected", st.Reinjected, "failed", st.Failed, "given_up", st.GivenUp, "skipped", st.Skipped,
		"jellyfin_overviews_set", st.OverviewsSet, "video_seconds", st.VideoSeconds,
		"prompt_tokens", st.PromptTokens, "output_tokens", st.OutputTokens, "stopped", st.Stopped)
	return err
}

// daemon runs a pass every day at RUN_AT, in-process rather than via cron: one
// static binary, nothing else in the image, and a missed run (container down)
// is simply caught up by the next one.
func daemon(ctx context.Context, r *runner) error {
	if r.cfg.RunOnStart {
		if err := pass(ctx, r); err != nil {
			r.log.Error("run", "err", err)
		}
	}
	for {
		next := nextAt(time.Now(), r.cfg.RunAt)
		r.log.Info("next run", "at", next.Format(time.RFC3339))
		if err := sleepCtx(ctx, time.Until(next)); err != nil {
			return nil // shutdown
		}
		if err := pass(ctx, r); err != nil {
			r.log.Error("run", "err", err)
		}
	}
}

func nextAt(now time.Time, hhmm string) time.Time {
	t, _ := time.Parse("15:04", hhmm)
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
