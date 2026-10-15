package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is read from the environment only, so the stack file is the single
// place where a deployment's settings live.
type Config struct {
	// Library directories to scan. Paths are passed to Jellyfin verbatim, so
	// the container must mount the library at the path Jellyfin uses.
	Roots []string
	// Failure counts, so a video that cannot be summarized stops costing a
	// request every night.
	StateDir string

	Model           string
	SummaryLanguage string
	ThinkingLevel   string

	// Per-run caps. Sized for the Gemini free tier: 8 h of YouTube video a
	// day, and a few hundred requests.
	VideoSecondsPerRun int
	CallsPerRun        int
	// Longer videos skip URL input (over the context window at low media
	// resolution) and go straight to the transcript.
	MaxURLSeconds int
	// Spacing between Gemini calls, to stay under the per-minute limit.
	CallInterval time.Duration
	// Upper bound on one Gemini call, so a hung request cannot stall the
	// daemon. A timeout counts as transient, like a 5xx.
	CallTimeout time.Duration
	MaxAttempts int

	// How the .nfo plot is laid out: "text" (any client) or "markdown"
	// (jellyfin-web and the apps built on it: headings, and the original
	// description folded away). See nfo.go.
	PlotFormat string

	// Leave YouTube Shorts unsummarized: a one-minute vertical clip needs no
	// overview, and in a Shorts-heavy channel they are most of the calls.
	SkipShorts bool

	// "transcript": summarize from the original-language .srt when it passes
	// the checks below, and send the video only when it does not. "video":
	// the reverse. Transcripts cost ~1/25 of video at low media resolution
	// and summarize speech-led content about as well; video adds what is
	// only on screen.
	PreferSource string
	// A transcript below either bound is treated as unusable: too little
	// speech for its length (empty, mostly "[Music]", or a stub track), or
	// captions that stop early. Words are letters / 5.
	//
	// Calibrated on the 769-video YouTube library: speech-led videos run
	// 40-250 words/min and a sung musical 21, while animation montages and
	// visual-gag sketches sit at 0-8. Every long video's captions reach
	// 95-100% of its length.
	MinTranscriptWPM      float64
	MinTranscriptCoverage float64

	JellyfinURL    string
	JellyfinAPIKey string

	// Daemon mode: time of day, in the container's TZ.
	RunAt      string
	RunOnStart bool
	DryRun     bool
}

func loadConfig() (Config, error) {
	c := Config{
		Roots:           splitList(env("LIBRARY_ROOTS", "/shared/Media/YouTube")),
		StateDir:        env("STATE_DIR", "/state"),
		Model:           env("MODEL", "gemini-3.8-flash"),
		SummaryLanguage: env("SUMMARY_LANGUAGE", "English"),
		ThinkingLevel:   env("THINKING_LEVEL", "LOW"),
		JellyfinURL:     strings.TrimRight(env("JELLYFIN_URL", ""), "/"),
		JellyfinAPIKey:  env("JELLYFIN_API_KEY", ""),
		RunAt:           env("RUN_AT", "10:30"),
	}
	var err error
	ints := []struct {
		name string
		def  int
		dst  *int
	}{
		{"VIDEO_SECONDS_PER_RUN", 27000, &c.VideoSecondsPerRun},
		{"CALLS_PER_RUN", 200, &c.CallsPerRun},
		{"MAX_URL_SECONDS", 10800, &c.MaxURLSeconds},
		{"MAX_ATTEMPTS", 3, &c.MaxAttempts},
	}
	for _, i := range ints {
		if *i.dst, err = envInt(i.name, i.def); err != nil {
			return c, err
		}
	}
	c.PlotFormat = env("PLOT_FORMAT", "text")
	if c.PlotFormat != "text" && c.PlotFormat != "markdown" {
		return c, fmt.Errorf("PLOT_FORMAT %q: want text or markdown", c.PlotFormat)
	}
	c.PreferSource = env("PREFER_SOURCE", "transcript")
	if c.PreferSource != "transcript" && c.PreferSource != "video" {
		return c, fmt.Errorf("PREFER_SOURCE %q: want transcript or video", c.PreferSource)
	}
	if c.MinTranscriptWPM, err = envFloat("MIN_TRANSCRIPT_WPM", 10); err != nil {
		return c, err
	}
	if c.MinTranscriptCoverage, err = envFloat("MIN_TRANSCRIPT_COVERAGE", 0.5); err != nil {
		return c, err
	}
	if c.CallInterval, err = time.ParseDuration(env("CALL_INTERVAL", "7s")); err != nil {
		return c, fmt.Errorf("CALL_INTERVAL: %w", err)
	}
	if c.CallTimeout, err = time.ParseDuration(env("CALL_TIMEOUT", "10m")); err != nil {
		return c, fmt.Errorf("CALL_TIMEOUT: %w", err)
	}
	if c.RunOnStart, err = envBool("RUN_ON_START", false); err != nil {
		return c, err
	}
	if c.SkipShorts, err = envBool("SKIP_SHORTS", false); err != nil {
		return c, err
	}
	if c.DryRun, err = envBool("DRY_RUN", false); err != nil {
		return c, err
	}
	if _, err := time.Parse("15:04", c.RunAt); err != nil {
		return c, fmt.Errorf("RUN_AT %q: want HH:MM", c.RunAt)
	}
	if len(c.Roots) == 0 {
		return c, fmt.Errorf("LIBRARY_ROOTS is empty")
	}
	return c, nil
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

func envFloat(name string, def float64) (float64, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return f, nil
}

func envBool(name string, def bool) (bool, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return b, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
