package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Sidecar order when nothing says which track is original: the order of the
// ytdl-sub `languages` list.
var fallbackLangs = []string{"en", "he", "ru"}

// pickTranscript chooses the .srt sidecar to send as a transcript, or "" when
// there is none worth sending. A machine translation is never chosen: it is a
// summary of a summary, and the timestamps drift.
func pickTranscript(v Video, m Meta) (path, lang string) {
	have := map[string]string{}
	entries, _ := os.ReadDir(filepath.Dir(v.Base))
	prefix := v.Name() + "."
	for _, e := range entries {
		l, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || !strings.HasSuffix(l, ".srt") {
			continue
		}
		if l = strings.TrimSuffix(l, ".srt"); !strings.Contains(l, ".") {
			have[normLang(l)] = filepath.Join(filepath.Dir(v.Base), e.Name())
		}
	}
	if len(have) == 0 {
		return "", ""
	}
	if !m.HasInfoJSON {
		for _, l := range fallbackLangs {
			if p, ok := have[l]; ok {
				return p, l
			}
		}
		return "", ""
	}
	// The original language, whether its track is manual or ASR.
	if p, ok := have[m.Language]; ok && m.Language != "" && (m.Manual[m.Language] || m.Auto[m.Language]) {
		return p, m.Language
	}
	// Otherwise a creator-provided track: a human translation is acceptable.
	for _, l := range fallbackLangs {
		if p, ok := have[l]; ok && m.Manual[l] {
			return p, l
		}
	}
	return "", ""
}

var (
	srtTime = regexp.MustCompile(`^(\d+):(\d{2}):(\d{2})[,.]\d+\s+-->`)
	srtTag  = regexp.MustCompile(`<[^>]*>|\{\\[^}]*\}`)
)

// loadTranscript renders an .srt as "[MM:SS] text" lines, one per ~30 s of
// speech. Cue-level granularity wastes tokens on timestamps, and YouTube's
// roll-up ASR captions repeat each line two or three times, so repeats are
// dropped.
func loadTranscript(path string, maxChars int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var (
		out     strings.Builder
		chunk   []string
		chunkAt = -1
		cueAt   int
		last    string
	)
	flush := func() {
		if len(chunk) > 0 {
			fmt.Fprintf(&out, "[%s] %s\n", formatTS(chunkAt), strings.Join(chunk, " "))
		}
		chunk, chunkAt = nil, -1
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if s := srtTime.FindStringSubmatch(line); s != nil {
			h, _ := strconv.Atoi(s[1])
			mi, _ := strconv.Atoi(s[2])
			se, _ := strconv.Atoi(s[3])
			cueAt = h*3600 + mi*60 + se
			if chunkAt >= 0 && cueAt-chunkAt >= 30 {
				flush()
			}
			continue
		}
		if line == "" || isDigits(line) {
			continue
		}
		line = strings.TrimSpace(srtTag.ReplaceAllString(line, ""))
		if line == "" || line == last {
			continue
		}
		last = line
		if chunkAt < 0 {
			chunkAt = cueAt
		}
		chunk = append(chunk, line)
	}
	flush()
	if err := sc.Err(); err != nil {
		return "", err
	}
	s := out.String()
	if maxChars > 0 && len(s) > maxChars {
		s = truncate(s, maxChars) + "\n[transcript truncated]\n"
	}
	return s, nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// formatTS is MM:SS, or H:MM:SS from an hour on -- the form Gemini uses for
// video timestamps, so both sources produce the same notation.
func formatTS(sec int) string {
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%02d:%02d", sec/60, sec%60)
}

// parseTS accepts MM:SS, M:SS and H:MM:SS.
func parseTS(s string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (i > 0 && n > 59) {
			return 0, false
		}
		total = total*60 + n
	}
	return total, true
}
