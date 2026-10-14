package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// failures counts consecutive failed attempts per video, so one that can
// never be summarized (no captions and no longer public, say) stops being
// retried after MaxAttempts. Deleting the file retries everything.
type failures struct {
	path    string
	Entries map[string]failure `json:"entries"`
}

type failure struct {
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
	Error string    `json:"error"`
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

func (f *failures) record(key string, err error) {
	e := f.Entries[key]
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
