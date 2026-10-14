package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sample picks n videos spread across folders and lengths: each round takes
// one video per folder, cycling through the median, longest, shortest and
// quartiles, so a 15-video sample covers a lecture series and a channel of
// one-minute clips alike. Deterministic, so two evals see the same videos.
func sample(roots []string, videos []Video, metas map[string]Meta, n int) []Video {
	groups := map[string][]Video{}
	for _, v := range videos {
		groups[folderOf(roots, v)] = append(groups[folderOf(roots, v)], v)
	}
	var names []string
	for g, vs := range groups {
		names = append(names, g)
		sort.Slice(vs, func(i, j int) bool {
			if metas[vs[i].Base].Duration != metas[vs[j].Base].Duration {
				return metas[vs[i].Base].Duration < metas[vs[j].Base].Duration
			}
			return vs[i].Base < vs[j].Base
		})
	}
	sort.Slice(names, func(i, j int) bool {
		if len(groups[names[i]]) != len(groups[names[j]]) {
			return len(groups[names[i]]) > len(groups[names[j]])
		}
		return names[i] < names[j]
	})
	positions := []float64{0.5, 1, 0, 0.25, 0.75, 0.1, 0.9}
	picked := map[string]bool{}
	var out []Video
	for _, pos := range positions {
		for _, g := range names {
			if len(out) == n {
				return out
			}
			vs := groups[g]
			i := int(pos * float64(len(vs)-1))
			if !picked[vs[i].Base] {
				picked[vs[i].Base] = true
				out = append(out, vs[i])
			}
		}
	}
	return out
}

func folderOf(roots []string, v Video) string {
	for _, r := range roots {
		if rel, err := filepath.Rel(r, v.Path); err == nil && !strings.HasPrefix(rel, "..") {
			return strings.SplitN(rel, string(filepath.Separator), 2)[0]
		}
	}
	return filepath.Dir(v.Path)
}

type evalRow struct {
	video, folder, lang, model, mode, file, err string
	duration                                    int
	u                                           Usage
}

// runEval summarizes a sample with each model and mode into outDir and writes
// report.md beside the results. It never touches the library.
// Gemini 3 at low media resolution and the default 1 fps: 70 tokens per frame
// plus 25 per second of audio. Only used to size an eval before it runs.
const videoTokensPerSecond = 95

func runEval(ctx context.Context, cfg Config, models, modes []string, n int, outDir string, dryRun bool) error {
	videos, err := scan(cfg.Roots)
	if err != nil {
		return err
	}
	metas := map[string]Meta{}
	for _, v := range videos {
		m, err := loadMeta(v)
		if err != nil {
			return fmt.Errorf("%s: %w", v.Name(), err)
		}
		metas[v.Base] = m
	}
	picked := sample(cfg.Roots, videos, metas, n)
	total := 0
	for _, v := range picked {
		total += metas[v.Base].Duration
	}
	fmt.Fprintf(os.Stderr, "eval: %d videos, %s of video, models %v, modes %v\n", len(picked), formatTS(total), models, modes)
	if dryRun {
		return planEval(cfg, picked, metas, models, modes)
	}
	if err := os.MkdirAll(outDir, 0o775); err != nil {
		return err
	}

	var rows []evalRow
	for _, model := range models {
		c := cfg
		c.Model = model
		g, err := newGemini(ctx, c)
		if err != nil {
			return err
		}
		for _, v := range picked {
			m := metas[v.Base]
			for _, mode := range modes {
				row := evalRow{video: v.Name(), folder: folderOf(cfg.Roots, v), lang: m.Language, model: model, mode: mode, duration: m.Duration}
				var s Summary
				switch mode {
				case "video":
					// Same rule as a run: past the context window, a run
					// would use the transcript, so there is nothing to test.
					if m.Duration > cfg.MaxURLSeconds {
						row.err = "over MAX_URL_SECONDS, a run uses the transcript"
						rows = append(rows, row)
						continue
					}
					s, row.u, err = g.FromVideo(ctx, m)
				case "transcript":
					path, lang := pickTranscript(v, m)
					if path == "" {
						row.err = "no usable transcript"
						rows = append(rows, row)
						continue
					}
					row.mode += ":" + lang
					var t string
					if t, err = loadTranscript(path, 600_000); err == nil {
						s, row.u, err = g.FromTranscript(ctx, m, t)
					}
				default:
					return fmt.Errorf("unknown mode %q", mode)
				}
				if err != nil {
					row.err = err.Error()
					fmt.Fprintf(os.Stderr, "  %s %s %s: %v\n", model, mode, v.Name(), err)
				} else {
					row.file = filepath.Join(model, mode, v.Name()+".summary.md")
					p := filepath.Join(outDir, row.file)
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						return err
					}
					md := s.Markdown(Provenance{Model: model, Source: row.mode, Generated: time.Now()})
					if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
						return err
					}
					fmt.Fprintf(os.Stderr, "  %s %s %s: %d tokens in, %s\n", model, mode, v.Name(), row.u.PromptTokens, row.u.Latency.Round(time.Second))
				}
				rows = append(rows, row)
				if err := sleepCtx(ctx, cfg.CallInterval); err != nil {
					return err
				}
			}
		}
	}
	return writeReport(filepath.Join(outDir, "report.md"), rows)
}

// planEval prints the sample and the input tokens each model would be sent,
// without calling the model.
func planEval(cfg Config, picked []Video, metas map[string]Meta, models, modes []string) error {
	var videoTok, textTok int
	for _, v := range picked {
		m := metas[v.Base]
		route := []string{}
		for _, mode := range modes {
			switch mode {
			case "video":
				if m.Duration > cfg.MaxURLSeconds {
					route = append(route, "video:skipped")
					continue
				}
				videoTok += m.Duration * videoTokensPerSecond
				route = append(route, "video")
			case "transcript":
				path, lang := pickTranscript(v, m)
				if path == "" {
					route = append(route, "transcript:none")
					continue
				}
				t, err := loadTranscript(path, 600_000)
				if err != nil {
					return err
				}
				textTok += len(t) / 4 // ~4 characters a token
				route = append(route, "transcript:"+lang)
			}
		}
		fmt.Printf("%8s  orig=%-3s %-32s %s/%s\n", formatTS(m.Duration), m.Language, strings.Join(route, ","), folderOf(cfg.Roots, v), v.Name())
	}
	fmt.Printf("\nper model: ~%d video tokens + ~%d transcript tokens (+ ~1.5k prompt and ~1.5k output per call); %d models\n", videoTok, textTok, len(models))
	return nil
}

func writeReport(path string, rows []evalRow) error {
	var b strings.Builder
	b.WriteString("| Video | Folder | Lang | Length | Model | Mode | Tokens in | Tokens out | Latency | Result |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		res := fmt.Sprintf("[summary](%s)", strings.ReplaceAll(r.file, " ", "%20"))
		if r.err != "" {
			res = "error: " + strings.ReplaceAll(truncate(r.err, 120), "|", "/")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d | %d | %s | %s |\n",
			strings.ReplaceAll(r.video, "|", "/"), r.folder, r.lang, formatTS(r.duration), r.model, r.mode,
			r.u.PromptTokens, r.u.OutputTokens+r.u.ThoughtTokens, r.u.Latency.Round(time.Second), res)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// runPlan prints what a run would do for each unsummarized video, and the
// backlog it adds up to, without calling the model.
func runPlan(cfg Config) error {
	videos, err := scan(cfg.Roots)
	if err != nil {
		return err
	}
	var pending, seconds, viaTranscript, noFallback int
	for _, v := range videos {
		if _, err := os.Stat(v.SummaryPath()); err == nil {
			continue
		}
		m, err := loadMeta(v)
		if err != nil {
			return fmt.Errorf("%s: %w", v.Name(), err)
		}
		pending++
		source := "video"
		if m.URL == "" || m.Duration > cfg.MaxURLSeconds {
			source = "transcript"
			viaTranscript++
		} else {
			seconds += m.Duration
		}
		_, lang := pickTranscript(v, m)
		if lang == "" {
			lang = "-"
			noFallback++
		}
		fmt.Printf("%-10s %8s  orig=%-3s fallback=%-3s  %s\n", source, formatTS(m.Duration), m.Language, lang, folderOf(cfg.Roots, v)+"/"+v.Name())
	}
	days := 0
	if cfg.VideoSecondsPerRun > 0 {
		days = (seconds + cfg.VideoSecondsPerRun - 1) / cfg.VideoSecondsPerRun
	}
	fmt.Printf("\n%d pending: %s of video input (%d daily runs at VIDEO_SECONDS_PER_RUN=%d), %d by transcript, %d with no transcript fallback\n",
		pending, formatTS(seconds), days, cfg.VideoSecondsPerRun, viaTranscript, noFallback)
	return nil
}
