package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
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
	once, err := withPlot(ytdlNFO, "Overview <b>.\n\nKey points:\n• 00:10 x", "text")
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
	twice, _ := withPlot(once, "Overview <b>.\n\nKey points:\n• 00:10 x", "text")
	if twice != once {
		t.Errorf("second injection changed the file:\n%s", twice)
	}
	// An edited summary replaces the old one instead of stacking.
	edited, _ := withPlot(once, "New.", "text")
	if strings.Count(edited, plotMarker) != 1 || !strings.HasPrefix(strings.SplitN(edited, "<plot>", 2)[1], "New.\n\n"+plotMarker+"\nhttps://") {
		t.Errorf("edit not applied cleanly:\n%s", edited)
	}
}

func TestWithPlotMissingOrEmpty(t *testing.T) {
	noPlot := "<episodedetails>\n  <title>t</title>\n</episodedetails>\n"
	got, err := withPlot(noPlot, "S", "text")
	if err != nil || !strings.Contains(got, "<plot>S\n\n"+plotMarker+"</plot>\n</episodedetails>") {
		t.Errorf("got %q, %v", got, err)
	}
	got, _ = withPlot("<e><plot/></e>", "S", "text")
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
	got, st, err := loadTranscript(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "[00:01] hello there next line\n[00:40] later\n[1:00:02] an hour in\n"
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	// 31 letters: hellothere nextline later anhourin.
	if st.Words != 6 || st.LastAt != 3602 {
		t.Errorf("stats %+v", st)
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
	videoErr, transcriptErr     map[string]error
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
	if err := f.transcriptErr[m.ID]; err != nil {
		return Summary{}, Usage{}, err
	}
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
		os.WriteFile(base+".en.srt", []byte(speechSRT(600, 600)), 0o644)
		vs = append(vs, Video{Path: base + ".mp4", Base: base})
	}
	return root, vs
}

// speechSRT is a transcript with a cue of ordinary speech every 30 s up to
// until, for a video of the given length: about 30 words/min.
func speechSRT(duration, until int) string {
	var b strings.Builder
	for i, at := 1, 0; at < until && at < duration; i, at = i+1, at+30 {
		// Numbered: the loader drops a line identical to the one before it.
		fmt.Fprintf(&b, "%d\n%s --> %s\nsentence %d is ordinary speech, long enough to count as words\n\n",
			i, srtTS(at), srtTS(at+2), i)
	}
	return b.String()
}

func srtTS(sec int) string {
	return fmt.Sprintf("%02d:%02d:%02d,000", sec/3600, sec/60%60, sec%60)
}

// testRunner prefers video, so the run-loop tests below exercise the video
// path and its error handling; the transcript-first tests set it themselves.
func testRunner(root string, f Summarizer) *runner {
	cfg := Config{Roots: []string{root}, StateDir: filepath.Join(root, ".state"), Model: "m",
		VideoSecondsPerRun: 27000, CallsPerRun: 100, MaxURLSeconds: 10800, MaxAttempts: 2,
		PreferSource: "video", MinTranscriptWPM: 10, MinTranscriptCoverage: 0.5}
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

	// A budget smaller than any one video still lets the run's first through.
	f3 := &fake{}
	root3, _ := newLibrary(t, "aaaaaaaaaaa", "bbbbbbbbbbb")
	r = testRunner(root3, f3)
	r.cfg.VideoSecondsPerRun = 300
	st, _ = r.run(context.Background())
	if st.Summarized != 1 || f3.videoCalls != 1 {
		t.Errorf("long video: %+v, fake %+v", st, f3)
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

	// A subtitle appears (a backfill): the video is tried again, and the
	// new transcript carries it.
	os.WriteFile(vs[0].Base+".en.srt", []byte(speechSRT(600, 600)), 0o644)
	st, _ := r.run(context.Background())
	if st.Summarized != 1 || f.transcriptCalls != 1 {
		t.Errorf("after backfill: %+v, fake %+v", st, f)
	}
}

func TestRunTranscriptFirst(t *testing.T) {
	root, vs := newLibrary(t, "aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc", "ddddddddddd")
	os.WriteFile(vs[1].Base+".en.srt", []byte("1\n00:00:01,000 --> 00:00:02,000\n[Music]\n\n2\n00:00:05,000 --> 00:00:06,000\nhi\n"), 0o644)
	os.WriteFile(vs[2].Base+".en.srt", []byte(speechSRT(600, 120)), 0o644) // stops at 1:30 of 10:00
	os.Remove(vs[3].Base + ".en.srt")
	f := &fake{}
	r := testRunner(root, f)
	r.cfg.PreferSource = "transcript"
	st, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Summarized != 4 || f.transcriptCalls != 1 || f.videoCalls != 3 || st.FromTranscript != 1 {
		t.Fatalf("%+v, fake %+v", st, f)
	}
	for i, want := range []string{"transcript:en", "video", "video", "video"} {
		md, _ := os.ReadFile(vs[i].SummaryPath())
		if !strings.Contains(string(md), "source: "+want+"\n") {
			t.Errorf("video %d: want %s, got\n%s", i, want, md)
		}
	}
}

func TestRunTranscriptErrorFallsBackToVideo(t *testing.T) {
	root, vs := newLibrary(t, "aaaaaaaaaaa")
	f := &fake{transcriptErr: map[string]error{"aaaaaaaaaaa": errors.New("decoding model output")}}
	r := testRunner(root, f)
	r.cfg.PreferSource = "transcript"
	st, _ := r.run(context.Background())
	md, _ := os.ReadFile(vs[0].SummaryPath())
	if st.Summarized != 1 || f.transcriptCalls != 1 || f.videoCalls != 1 || !strings.Contains(string(md), "source: video\n") {
		t.Errorf("%+v, fake %+v\n%s", st, f, md)
	}
}

func TestRunNoUsableSourceRecordsWhy(t *testing.T) {
	root, vs := newLibrary(t, "aaaaaaaaaaa")
	os.WriteFile(vs[0].Base+".info.json", []byte(`{"id":"aaaaaaaaaaa","duration":20000,"language":"en","automatic_captions":{"en":[]}}`), 0o644)
	os.Remove(vs[0].Base + ".en.srt")
	f := &fake{}
	r := testRunner(root, f)
	r.cfg.PreferSource = "transcript"
	st, _ := r.run(context.Background())
	fails, _ := loadFailures(r.cfg.StateDir)
	e := fails.Entries[vs[0].Base]
	if st.Failed != 1 || f.videoCalls+f.transcriptCalls != 0 ||
		!strings.Contains(e.Error, "transcript: no original-language transcript") || !strings.Contains(e.Error, "video: longer than MAX_URL_SECONDS") {
		t.Errorf("%+v, fake %+v, entry %+v", st, f, e)
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

func TestRunAuthStopsWithoutRecording(t *testing.T) {
	root, _ := newLibrary(t, "aaaaaaaaaaa", "bbbbbbbbbbb")
	badKey := genai.APIError{Code: 400, Status: "INVALID_ARGUMENT",
		Details: []map[string]any{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "API_KEY_INVALID"}}}
	f := &fake{videoErr: map[string]error{"aaaaaaaaaaa": badKey, "bbbbbbbbbbb": badKey}}
	r := testRunner(root, f)
	st, err := r.run(context.Background())
	if !errors.Is(err, errAuth) || st.Stopped != "auth" || f.videoCalls != 1 || f.transcriptCalls != 0 || st.Failed != 0 {
		t.Fatalf("err %v, %+v, fake %+v", err, st, f)
	}
	fails, _ := loadFailures(r.cfg.StateDir)
	if len(fails.Entries) != 0 {
		t.Errorf("recorded %v", fails.Entries)
	}
}

func TestIsAuth(t *testing.T) {
	private := genai.APIError{Code: 403, Status: "PERMISSION_DENIED", Message: "private"}
	if isAuth(private) {
		t.Error("a bare 403 is a private video, not an auth error")
	}
	if !isAuth(genai.APIError{Code: 401}) {
		t.Error("401")
	}
}

func TestRunNetworkErrorsAreTransient(t *testing.T) {
	ids := []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc", "ddddddddddd", "eeeeeeeeeee", "fffffffffff"}
	root, _ := newLibrary(t, ids...)
	down := &url.Error{Op: "Post", URL: "https://x", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}
	f := &fake{videoErr: map[string]error{}}
	for _, id := range ids {
		f.videoErr[id] = down
	}
	r := testRunner(root, f)
	st, err := r.run(context.Background())
	if err != nil || st.Failed != 0 || st.Stopped != "transient errors" || f.videoCalls != maxTransient || f.transcriptCalls != 0 {
		t.Fatalf("err %v, %+v, fake %+v", err, st, f)
	}
}

func TestRunWriteErrorStopsAndSavesState(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root, vs := newLibrary(t, "aaaaaaaaaaa", "bbbbbbbbbbb")
	dir := filepath.Dir(vs[0].Base)
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	f := &fake{}
	r := testRunner(root, f)
	st, err := r.run(context.Background())
	if err == nil || st.Stopped != "write error" || f.videoCalls != 1 {
		t.Fatalf("err %v, %+v, fake %+v", err, st, f)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.StateDir, "failures.json")); err != nil {
		t.Errorf("failures not saved: %v", err)
	}
}

func TestRunSkipsShorts(t *testing.T) {
	root, vs := newLibrary(t, "aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc")
	// From the /shorts tab; vertical and short; landscape and short (kept).
	os.WriteFile(vs[0].Base+".info.json", []byte(`{"id":"aaaaaaaaaaa","duration":600,"language":"en","automatic_captions":{"en":[]},"playlist_webpage_url":"https://www.youtube.com/@x/shorts"}`), 0o644)
	os.WriteFile(vs[1].Base+".info.json", []byte(`{"id":"bbbbbbbbbbb","duration":59,"language":"en","automatic_captions":{"en":[]},"width":1080,"height":1920}`), 0o644)
	os.WriteFile(vs[2].Base+".info.json", []byte(`{"id":"ccccccccccc","duration":59,"language":"en","automatic_captions":{"en":[]},"width":1920,"height":1080}`), 0o644)
	f := &fake{}
	r := testRunner(root, f)
	r.cfg.SkipShorts = true
	st, err := r.run(context.Background())
	if err != nil || st.Skipped != 2 || st.Summarized != 1 || st.Failed != 0 {
		t.Fatalf("err %v, %+v", err, st)
	}
	if _, err := os.Stat(vs[2].SummaryPath()); err != nil {
		t.Error("the landscape clip should be summarized")
	}
	for _, v := range vs[:2] {
		if _, err := os.Stat(v.SummaryPath()); err == nil {
			t.Errorf("%s: a Short was summarized", v.Name())
		}
	}
	fails, _ := loadFailures(r.cfg.StateDir)
	if len(fails.Entries) != 0 {
		t.Errorf("skips recorded as failures: %v", fails.Entries)
	}
}

func TestWithPlotMarkdown(t *testing.T) {
	original := "https://www.youtube.com/watch?v=8idr1WZ1A7Q\n\n#hashtag line\na <br> & <b>tag</b>\n\nlast"
	doc := "<episodedetails>\n  <plot>" + escapeText(original) + "</plot>\n</episodedetails>\n"
	summary := "Overview.\n\n#### Key points\n\n- **00:10** First"

	md, err := withPlot(doc, summary, "markdown")
	if err != nil {
		t.Fatal(err)
	}
	_, _, inner, _ := findPlot(md)
	plot, _ := xmlText(inner)
	want := summary + "\n\n" + detailsOpen +
		"https://www.youtube.com/watch?v=8idr1WZ1A7Q<br><br>#hashtag line<br>a &lt;br&gt; &amp; &lt;b&gt;tag&lt;/b&gt;<br><br>last" +
		detailsEnd + "\n\n" + mdMarker
	if plot != want {
		t.Fatalf("plot:\n%q\nwant\n%q", plot, want)
	}
	// The folded description is one line: Markdown cannot reflow it.
	if strings.Contains(strings.SplitN(plot, detailsOpen, 2)[1], "\n\n#") {
		t.Error("description escaped its HTML block")
	}
	if again, _ := withPlot(md, summary, "markdown"); again != md {
		t.Error("second markdown injection changed the file")
	}
	// Back to text: the original comes out exactly.
	text, _ := withPlot(md, "S", "text")
	_, _, inner, _ = findPlot(text)
	plot, _ = xmlText(inner)
	if plot != "S\n\n"+plotMarker+"\n"+original {
		t.Errorf("text after markdown:\n%q", plot)
	}
	// And text-injected plots (the v0.1.4 layout) convert to markdown.
	if conv, _ := withPlot(text, summary, "markdown"); conv != md {
		t.Errorf("text→markdown differs from a fresh markdown injection:\n%s", conv)
	}
}

func TestWithPlotMarkdownEmptyOriginal(t *testing.T) {
	got, _ := withPlot("<e><plot/></e>", "S", "markdown")
	if got != "<e><plot>S\n\n"+escapeText(mdMarker)+"</plot></e>" {
		t.Errorf("got %q", got)
	}
	if again, _ := withPlot(got, "S", "markdown"); again != got {
		t.Errorf("not idempotent: %q", again)
	}
}

func TestPlotMarkdown(t *testing.T) {
	s := Summary{Overview: "It explains **Bayes**.", KeyPoints: []KeyPoint{{"0:10", "First"}}}
	s.tidy(600)
	md := s.Markdown(Provenance{Model: "m", Source: "video", Generated: time.Unix(0, 0)})
	want := "It explains **Bayes**.\n\n#### Key points\n\n- **00:10** First"
	if got := plotMarkdown(md); got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func TestChaptersReachThePrompt(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "x [abcdefghijk]")
	os.WriteFile(base+".info.json", []byte(`{"id":"abcdefghijk","duration":900,"description":"About it.","chapters":[{"start_time":0,"title":"Intro"},{"start_time":95.5,"title":"The trick"},{"start_time":600,"title":" "}]}`), 0o644)
	m, err := loadMeta(Video{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chapters) != 2 || m.Chapters[1] != (Chapter{95, "The trick"}) {
		t.Fatalf("chapters %+v", m.Chapters)
	}
	p := prompt(m, "")
	for _, want := range []string{"00:00 Intro\n01:35 The trick\n", "Follow the creator's chapters", "Do not restate the description"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if p2 := prompt(Meta{Title: "t"}, ""); strings.Contains(p2, "chapters") || strings.Contains(p2, "restate") {
		t.Errorf("no chapters or description, yet:\n%s", p2)
	}
}
