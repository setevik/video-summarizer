package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// failures counts consecutive failed attempts per video, so one that can
// never be summarized (no captions and no longer public, say) stops being
// retried after MaxAttempts. Deleting the file retries everything.
//
// Each entry also records the video's subtitle sidecars as they were. A
// change there (a backfilled .srt, say) makes the video worth trying again,
// so the count starts over.
type failures struct {
	path    string
	Entries map[string]failure `json:"entries"`
}

type failure struct {
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
	Error string    `json:"error"`
	// sidecarSignature at the last failure.
	Sidecars string `json:"sidecars"`
}

func loadFailures(dir string) (*failures, error) {
	f := &failures{path: filepath.Join(dir, "failures.json"), Entries: map[string]failure{}}
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	return f, json.Unmarshal(raw, f)
}

// givenUp reports whether a video has failed MaxAttempts times with the
// sidecars it still has. A video whose sidecars changed is forgiven.
func (f *failures) givenUp(key, sidecars string, max int) bool {
	e, ok := f.Entries[key]
	if !ok {
		return false
	}
	if e.Sidecars != sidecars {
		delete(f.Entries, key)
		return false
	}
	return e.Count >= max
}

func (f *failures) record(key, sidecars string, err error) {
	e := f.Entries[key]
	if e.Sidecars != sidecars {
		e.Count = 0
	}
	e.Sidecars = sidecars
	e.Count++
	e.Last = time.Now()
	e.Error = err.Error()
	f.Entries[key] = e
}

func (f *failures) clear(key string) { delete(f.Entries, key) }

func (f *failures) save() error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o775); err != nil {
		return err
	}
	return writeAtomic(f.path, raw)
}

// sidecarSignature identifies a video's .srt sidecars by name and size.
func sidecarSignature(v Video) string {
	entries, _ := os.ReadDir(filepath.Dir(v.Base))
	prefix := v.Name() + "."
	var parts []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".srt") {
			continue
		}
		size := int64(-1)
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		parts = append(parts, fmt.Sprintf("%s:%d", strings.TrimPrefix(e.Name(), prefix), size))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
