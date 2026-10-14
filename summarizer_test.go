package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// Taken from a real ytdl-sub .nfo on the NAS, trimmed.
const ytdlNFO = `<?xml version='1.0' encoding='utf-8'?>
<episodedetails>
  <aired>2020-03-15</aired>
  <plot>https://www.youtube.com/watch?v=8idr1WZ1A7Q

Part 2: https://youtu.be/ZA4JkHKZM50 &amp; more</plot>
  <season>15</season>
  <title>2020-03-15 - Binomial distributions</title>
</episodedetails>
`

func TestWithPlotPrependsAndIsIdempotent(t *testing.T) {
	once, err := withPlot(ytdlNFO, "Overview <b>.\n\nKey points:\n• 00:10 x")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<plot>Overview &lt;b&gt;.\n\nKey points:\n• 00:10 x\n\n" + plotMarker + "\nhttps://www.youtube.com/watch?v=8idr1WZ1A7Q",
		"&amp; more</plot>",
		"<season>15</season>",
	} {
		if !strings.Contains(once, want) {
			t.Errorf("missing %q in\n%s", want, once)
		}
	}
	twice, _ := withPlot(once, "Overview <b>.\n\nKey points:\n• 00:10 x")
	if twice != once {
		t.Errorf("second injection changed the file:\n%s", twice)
	}
	// An edited summary replaces the old one instead of stacking.
	edited, _ := withPlot(once, "New.")
	if strings.Count(edited, plotMarker) != 1 || !strings.HasPrefix(strings.SplitN(edited, "<plot>", 2)[1], "New.\n\n"+plotMarker+"\nhttps://") {
		t.Errorf("edit not applied cleanly:\n%s", edited)
	}
}

func TestWithPlotMissingOrEmpty(t *testing.T) {
	noPlot := "<episodedetails>\n  <title>t</title>\n</episodedetails>\n"
	got, err := withPlot(noPlot, "S")
	if err != nil || !strings.Contains(got, "<plot>S\n\n"+plotMarker+"</plot>\n</episodedetails>") {
		t.Errorf("got %q, %v", got, err)
	}
	got, _ = withPlot("<e><plot/></e>", "S")
	if got != "<e><plot>S\n\n"+plotMarker+"</plot></e>" {
		t.Errorf("got %q", got)
	}
}

func TestPlotText(t *testing.T) {
	s := Summary{Overview: "It explains **Bayes**.", KeyPoints: []KeyPoint{{"01:05", "Second"}, {"0:10", "First"}, {"99:00", "Past the end"}, {"bad", "x"}}}
	s.tidy(600)
	md := s.Markdown(Provenance{Model: "m", Source: "video", Generated: time.Unix(0, 0)})
	if !strings.HasPrefix(md, "---\nmodel: m\nsource: video\n") {
		t.Errorf("front matter: %s", md)
	}
	want := "It explains Bayes.\n\nKey points:\n\n• 00:10 First\n• 01:05 Second"
	if got := plotText(md); got != want {
		t.Errorf("plotText:\n%q\nwant\n%q", got, want)
	}
}

func TestLoadTranscript(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.en.srt")
	srt := "\ufeff1\n00:00:01,000 --> 00:00:03,000\nhello <i>there</i>\n\n2\n00:00:03,000 --> 00:00:05,000\nhello there\nnext line\n\n3\n00:00:40,000 --> 00:00:42,000\nlater\n\n4\n01:00:02,000 --> 01:00:03,000\nan hour in\n"
	os.WriteFile(p, []byte(srt), 0o644)
	got, err := loadTranscript(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "[00:01] hello there next line\n[00:40] later\n[1:00:02] an hour in\n"
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func TestPickTranscript(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "s01.e01 - T [abcdefghijk]")
	for _, l := range []string{"en", "ru", "he"} {
		os.WriteFile(base+"."+l+".srt", []byte("x"), 0o644)
	}
	v := Video{Path: base + ".mp4", Base: base}
	cases := []struct {
		name string
		m    Meta
		want string
	}{
		{"original ASR beats manual translation", Meta{HasInfoJSON: true, Language: "ru", Auto: map[string]bool{"ru": true, "en": true}, Manual: map[string]bool{"en": true}}, "ru"},
		{"no original track: manual only", Meta{HasInfoJSON: true, Language: "de", Auto: map[string]bool{"en": true, "ru": true}, Manual: map[string]bool{"he": true}}, "he"},
		{"only machine translations", Meta{HasInfoJSON: true, Language: "de", Auto: map[string]bool{"en": true, "ru": true, "he": true}, Manual: map[string]bool{}}, ""},
		{"no info.json: list order", Meta{}, "en"},
	}
	for _, c := range cases {
		if _, got := pickTranscript(v, c.m); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestLoadMetaOrigFromAutoCaptions(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "x [abcdefghijk]")
	os.WriteFile(base+".info.json", []byte(`{"id":"abcdefghijk","duration":61.5,"automatic_captions":{"iw-orig":[],"iw":[],"en":[]}}`), 0o644)
	m, err := loadMeta(Video{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if m.Language != "he" || m.Duration != 61 || m.URL != "https://www.youtube.com/watch?v=abcdefghijk" || m.Title != "x" {
		t.Errorf("%+v", m)
	}
}

// fake records calls and fails videos whose ID is listed.
type fake struct {
	videoCalls, transcriptCalls int
	videoErr                    map[string]error
}

func (f *fake) FromVideo(_ context.Context, m Meta) (Summary, Usage, error) {
	f.videoCalls++
	if err := f.videoErr[m.ID]; err != nil {
		return Summary{}, Usage{}, err
	}
	return Summary{Overview: "Video summary of " + m.ID}, Usage{PromptTokens: 100}, nil
}

func (f *fake) FromTranscript(_ context.Context, m Meta, t string) (Summary, Usage, error) {
	f.transcriptCalls++
	return Summary{Overview: "Transcript summary of " + m.ID}, Usage{}, nil
}

func newLibrary(t *testing.T, ids ...string) (string, []Video) {
	root := t.TempDir()
	dir := filepath.Join(root, "Chan", "Season 01")
	os.MkdirAll(dir, 0o755)
	var vs []Video
	for i, id := range ids {
		base := filepath.Join(dir, "s01.e0"+string(rune('1'+i))+" - T ["+id+"]")
		os.WriteFile(base+".mp4", nil, 0o644)
		os.WriteFile(base+".nfo", []byte(ytdlNFO), 0o644)
		os.WriteFile(base+".info.json", []byte(`{"id":"`+id+`","duration":600,"language":"en","automatic_captions":{"en":[]}}`), 0o644)
		os.WriteFile(base+".en.srt", []byte("1\n00:00:01,000 --> 00:00:02,000\nhi\n"), 0o644)
		vs = append(vs, Video{Path: base + ".mp4", Base: base})
	}
	return root, vs
}

func testRunner(root string, f Summarizer) *runner {
	cfg := Config{Roots: []string{root}, StateDir: filepath.Join(root, ".state"), Model: "m",
		VideoSecondsPerRun: 27000, CallsPerRun: 100, MaxURLSeconds: 10800, MaxAttempts: 2}
	return &runner{cfg: cfg, sum: f, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sleep: func(context.Context, time.Duration) error { return nil }}
}

func TestRunFallbackAndReinject(t *testing.T) {
	root, vs := newLibrary(t, "aaaaaaaaaaa", "bbbbbbbbbbb")
	f := &fake{videoErr: map[string]error{"bbbbbbbbbbb": genai.APIError{Code: 403, Message: "private"}}}
	r := testRunner(root, f)
	st, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Summarized != 2 || st.FromTranscript != 1 || f.videoCalls != 2 || f.transcriptCalls != 1 {
		t.Fatalf("first run: %+v, fake %+v", st, f)
	}
	md, _ := os.ReadFile(vs[1].SummaryPath())
	if !strings.Contains(string(md), "source: transcript:en") {
		t.Errorf("summary: %s", md)
	}
	nfo, _ := os.ReadFile(vs[0].NFOPath())
	if !strings.Contains(string(nfo), "<plot>Video summary of aaaaaaaaaaa\n\n"+plotMarker) {
		t.Errorf("nfo: %s", nfo)
	}

	// ytdl-sub regenerates one .nfo: the next run restores it without a call.
	os.WriteFile(vs[0].NFOPath(), []byte(ytdlNFO), 0o644)
	st, _ = r.run(context.Background())
	if st.Reinjected != 1 || st.Summarized != 0 || f.videoCalls != 2 {
		t.Errorf("second run: %+v, fake %+v", st, f)
	}
	st, _ = r.run(context.Background())
	if st.Reinjected != 0 {
		t.Errorf("third run should be a no-op: %+v", st)
	}
}

func TestRunQuotaStopsAndBudgetDefers(t *testing.T) {
	root, _ := newLibrary(t, "aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc")
	quota := genai.APIError{Code: 429}
	f := &fake{videoErr: map[string]error{"aaaaaaaaaaa": quota, "bbbbbbbbbbb": quota, "ccccccccccc": quota}}
	r := testRunner(root, f)
	st, _ := r.run(context.Background())
	// One video: a call, a retry after the per-minute wait, then stop.
	if st.Stopped != "quota" || f.videoCalls != 2 || st.Failed != 0 || f.transcriptCalls != 0 {
		t.Errorf("quota: %+v, fake %+v", st, f)
	}

	f2 := &fake{}
	r = testRunner(root, f2)
	r.cfg.VideoSecondsPerRun = 1000 // room for one 600 s video
	st, _ = r.run(context.Background())
	if st.Summarized != 1 || f2.transcriptCalls != 0 {
		t.Errorf("budget: %+v, fake %+v", st, f2)
	}
}

func TestRunGivesUp(t *testing.T) {
	root, vs := newLibrary(t, "aaaaaaaaaaa")
	os.Remove(vs[0].Base + ".en.srt")
	f := &fake{videoErr: map[string]error{"aaaaaaaaaaa": errors.New("deleted")}}
	r := testRunner(root, f)
	for i := 0; i < 3; i++ {
		r.run(context.Background())
	}
	if f.videoCalls != 2 { // MaxAttempts
		t.Errorf("calls %d", f.videoCalls)
	}
}

func TestNextAt(t *testing.T) {
	loc := time.FixedZone("x", 3*3600)
	now := time.Date(2026, 10, 14, 11, 0, 0, 0, loc)
	if got := nextAt(now, "10:30"); !got.Equal(time.Date(2026, 10, 15, 10, 30, 0, 0, loc)) {
		t.Errorf("got %v", got)
	}
	if got := nextAt(now, "12:00"); !got.Equal(time.Date(2026, 10, 14, 12, 0, 0, 0, loc)) {
		t.Errorf("got %v", got)
	}
}

func TestSample(t *testing.T) {
	root := "/lib"
	var vs []Video
	metas := map[string]Meta{}
	for i := 0; i < 10; i++ {
		for _, g := range []string{"A", "B"} {
			b := filepath.Join(root, g, "v"+string(rune('0'+i)))
			vs = append(vs, Video{Path: b + ".mp4", Base: b})
			metas[b] = Meta{Duration: i * 60}
		}
	}
	got := sample([]string{root}, vs, metas, 5)
	if len(got) != 5 || folderOf([]string{root}, got[0]) == folderOf([]string{root}, got[1]) {
		t.Errorf("got %v", got)
	}
}
