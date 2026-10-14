package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var videoExts = map[string]bool{".mp4": true, ".mkv": true, ".webm": true}

// ytdl-sub's Homelab presets end every file name in "[<11-char id>]".
var idInName = regexp.MustCompile(`\[([A-Za-z0-9_-]{11})\]$`)

// Video is one media file and the sidecars that share its base name.
type Video struct {
	Path    string // the media file
	Base    string // Path without its extension
	ModTime time.Time
}

func (v Video) SummaryPath() string  { return v.Base + ".summary.md" }
func (v Video) NFOPath() string      { return v.Base + ".nfo" }
func (v Video) InfoJSONPath() string { return v.Base + ".info.json" }
func (v Video) Name() string         { return filepath.Base(v.Base) }

// scan returns every video under roots, newest first, so a fresh download is
// summarized before the backlog.
func scan(roots []string) ([]Video, error) {
	var out []Video
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != root && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if !videoExts[ext] {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			out = append(out, Video{Path: p, Base: strings.TrimSuffix(p, filepath.Ext(p)), ModTime: info.ModTime()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// Meta is what the summarizer needs to know about a video, from yt-dlp's
// .info.json when there is one.
type Meta struct {
	ID          string
	URL         string
	Title       string
	Channel     string
	Description string
	Duration    int // seconds; 0 when unknown
	// Original spoken language as a bare subtag ("en", "he"), or "".
	Language string
	// Languages with creator-provided subtitles, and with automatic captions.
	// Automatic captions include machine translations; only the original
	// language's track is a real transcript.
	Manual      map[string]bool
	Auto        map[string]bool
	HasInfoJSON bool
}

type infoJSON struct {
	ID                string                     `json:"id"`
	Title             string                     `json:"title"`
	Channel           string                     `json:"channel"`
	Description       string                     `json:"description"`
	WebpageURL        string                     `json:"webpage_url"`
	Duration          float64                    `json:"duration"`
	Language          string                     `json:"language"`
	Subtitles         map[string]json.RawMessage `json:"subtitles"`
	AutomaticCaptions map[string]json.RawMessage `json:"automatic_captions"`
}

func loadMeta(v Video) (Meta, error) {
	m := Meta{Manual: map[string]bool{}, Auto: map[string]bool{}}
	f, err := os.Open(v.InfoJSONPath())
	switch {
	case err == nil:
		defer f.Close()
		var ij infoJSON
		if err := json.NewDecoder(f).Decode(&ij); err != nil {
			return m, err
		}
		m.HasInfoJSON = true
		m.ID, m.Title, m.Channel, m.Description = ij.ID, ij.Title, ij.Channel, ij.Description
		m.URL = ij.WebpageURL
		m.Duration = int(ij.Duration)
		for k := range ij.Subtitles {
			m.Manual[normLang(k)] = true
		}
		for k := range ij.AutomaticCaptions {
			// yt-dlp marks the untranslated ASR track "<lang>-orig".
			if l, ok := strings.CutSuffix(k, "-orig"); ok && ij.Language == "" {
				ij.Language = l
			}
			m.Auto[normLang(k)] = true
		}
		m.Language = normLang(ij.Language)
	case errors.Is(err, fs.ErrNotExist):
	default:
		return m, err
	}

	if m.ID == "" {
		if s := idInName.FindStringSubmatch(v.Base); s != nil {
			m.ID = s[1]
		}
	}
	if m.URL == "" && m.ID != "" {
		m.URL = "https://www.youtube.com/watch?v=" + m.ID
	}
	if m.Title == "" {
		m.Title = strings.TrimSpace(idInName.ReplaceAllString(v.Name(), ""))
	}
	return m, nil
}

// normLang reduces a language tag to its primary subtag, and maps the
// deprecated "iw" YouTube still uses to "he" (the code ytdl-sub names the
// sidecar with).
func normLang(tag string) string {
	tag = strings.ToLower(tag)
	if i := strings.IndexAny(tag, "-_"); i > 0 {
		tag = tag[:i]
	}
	if tag == "iw" {
		return "he"
	}
	return tag
}
