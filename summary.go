package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Summary is what the model returns, before it is rendered to Markdown.
type Summary struct {
	Overview  string     `json:"overview"`
	KeyPoints []KeyPoint `json:"key_points"`
}

type KeyPoint struct {
	Timestamp string `json:"timestamp"`
	Point     string `json:"point"`
}

// Provenance is recorded in the file's front matter, so an eval or a model
// change can later find what produced a given summary.
type Provenance struct {
	Model     string
	Source    string // "video", or "transcript:<lang>"
	Generated time.Time
}

// tidy normalizes timestamps, drops points the model placed past the end of
// the video (a hallucination, not a rounding error), and sorts the rest.
func (s *Summary) tidy(duration int) {
	type tp struct {
		at int
		KeyPoint
	}
	var kept []tp
	for _, k := range s.KeyPoints {
		at, ok := parseTS(k.Timestamp)
		if !ok || (duration > 0 && at > duration) || strings.TrimSpace(k.Point) == "" {
			continue
		}
		kept = append(kept, tp{at, KeyPoint{formatTS(at), strings.TrimSpace(k.Point)}})
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].at < kept[j].at })
	s.KeyPoints = s.KeyPoints[:0]
	for _, k := range kept {
		s.KeyPoints = append(s.KeyPoints, k.KeyPoint)
	}
	s.Overview = strings.TrimSpace(s.Overview)
}

func (s Summary) Markdown(p Provenance) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nmodel: %s\nsource: %s\ngenerated: %s\n---\n\n", p.Model, p.Source, p.Generated.UTC().Format(time.RFC3339))
	b.WriteString(s.Overview)
	b.WriteString("\n")
	if len(s.KeyPoints) > 0 {
		b.WriteString("\n## Key points\n\n")
		for _, k := range s.KeyPoints {
			fmt.Fprintf(&b, "- **%s** %s\n", k.Timestamp, k.Point)
		}
	}
	return b.String()
}

// summaryBody is a .summary.md without its front matter.
func summaryBody(md string) string {
	if rest, ok := strings.CutPrefix(md, "---\n"); ok {
		if i := strings.Index(rest, "\n---\n"); i >= 0 {
			return strings.TrimSpace(rest[i+len("\n---\n"):])
		}
	}
	return strings.TrimSpace(md)
}

// plotMarkdown is the summary as the Markdown jellyfin-web renders: the
// file's own body, with the section heading made small enough for an
// overview (and for the episode lists, which render it too).
func plotMarkdown(md string) string {
	var lines []string
	for _, l := range strings.Split(summaryBody(md), "\n") {
		if h, ok := strings.CutPrefix(l, "## "); ok {
			l = "#### " + h
		}
		lines = append(lines, strings.TrimRight(l, " \t"))
	}
	return strings.Join(lines, "\n")
}

// plotText turns a .summary.md into the plain text that goes into the .nfo
// plot. It works on whatever the file holds rather than on a Summary, because
// the file is the source of truth and may have been edited by hand.
func plotText(md string) string {
	var lines []string
	for _, l := range strings.Split(summaryBody(md), "\n") {
		l = strings.TrimRight(l, " \t")
		if h, ok := strings.CutPrefix(l, "## "); ok {
			l = h + ":"
		} else if item, ok := strings.CutPrefix(l, "- "); ok {
			l = "• " + item
		}
		lines = append(lines, strings.ReplaceAll(l, "**", ""))
	}
	return strings.Join(lines, "\n")
}

// writeAtomic replaces path without ever leaving a half-written file for
// Jellyfin or ytdl-sub to read.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Group-writable like the rest of the library, so ytdl-sub can still
	// replace a file this job wrote.
	if err := os.Chmod(tmp.Name(), 0o664); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
