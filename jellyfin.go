package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type jellyfin struct {
	base, key string
	http      *http.Client
}

func newJellyfin(c Config) *jellyfin {
	if c.JellyfinURL == "" || c.JellyfinAPIKey == "" {
		return nil
	}
	return &jellyfin{base: c.JellyfinURL, key: c.JellyfinAPIKey, http: &http.Client{Timeout: 60 * time.Second}}
}

func (j *jellyfin) do(ctx context.Context, method, path string, q url.Values, in, out any) error {
	u := j.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", `MediaBrowser Token="`+j.key+`"`)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := j.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// setOverviews makes each video's Jellyfin overview the given plot, and locks
// the field. Items are matched by path, which is why the job mounts the
// library where Jellyfin does. A video Jellyfin has not scanned yet is
// skipped; a later run sets it.
//
// Not a metadata refresh: ytdl-sub also embeds the original description in
// each .mp4 as a "synopsis" tag, and on a refresh Jellyfin's media probe
// takes the overview from that rather than from the .nfo plot. A
// replace-all refresh empties it outright. So the overview is written
// through the API, as the web UI's metadata editor does, and locked so no
// refresh or rescan takes it back. Only items whose overview differs are
// written, so a run also repairs any that drifted.
func (j *jellyfin) setOverviews(ctx context.Context, want map[string]string) (int, error) {
	if len(want) == 0 {
		return 0, nil
	}
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	var folders []struct {
		ItemID    string `json:"ItemId"`
		Locations []string
	}
	if err := j.do(ctx, "GET", "/Library/VirtualFolders", nil, nil, &folders); err != nil {
		return 0, err
	}
	var stale []string // item IDs
	for _, f := range folders {
		if !containsAny(f.Locations, paths) {
			continue
		}
		var items struct {
			Items []struct{ ID, Path, Overview string } `json:"Items"`
		}
		q := url.Values{
			"ParentId": {f.ItemID}, "Recursive": {"true"}, "IncludeItemTypes": {"Episode,MusicVideo,Video"},
			"Fields": {"Path,Overview"}, "EnableImages": {"false"}, "EnableUserData": {"false"},
		}
		if err := j.do(ctx, "GET", "/Items", q, nil, &items); err != nil {
			return 0, err
		}
		for _, it := range items.Items {
			if plot, ok := want[it.Path]; ok && it.Overview != plot {
				stale = append(stale, it.ID+"\x00"+plot)
			}
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}
	// The item editor endpoint takes the full item; any user can read it.
	var users []struct{ ID string }
	if err := j.do(ctx, "GET", "/Users", nil, nil, &users); err != nil {
		return 0, err
	}
	if len(users) == 0 {
		return 0, fmt.Errorf("jellyfin has no users")
	}
	n := 0
	for _, s := range stale {
		id, plot, _ := strings.Cut(s, "\x00")
		// A map, not a struct: the item goes back whole, with every field
		// this code does not know about intact.
		var item map[string]any
		if err := j.do(ctx, "GET", "/Users/"+users[0].ID+"/Items/"+id, nil, nil, &item); err != nil {
			return n, err
		}
		item["Overview"] = plot
		item["LockedFields"] = withLock(item["LockedFields"], "Overview")
		if err := j.do(ctx, "POST", "/Items/"+id, nil, item, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func withLock(fields any, name string) []string {
	var out []string
	if list, ok := fields.([]any); ok {
		for _, f := range list {
			if s, ok := f.(string); ok {
				if s == name {
					return toStrings(list)
				}
				out = append(out, s)
			}
		}
	}
	return append(out, name)
}

func toStrings(list []any) []string {
	out := make([]string, 0, len(list))
	for _, f := range list {
		if s, ok := f.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func containsAny(locations, paths []string) bool {
	for _, l := range locations {
		l = strings.TrimRight(l, "/") + "/"
		for _, p := range paths {
			if strings.HasPrefix(p, l) {
				return true
			}
		}
	}
	return false
}
