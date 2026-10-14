package main

import (
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

func (j *jellyfin) do(ctx context.Context, method, path string, q url.Values, out any) error {
	u := j.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", `MediaBrowser Token="`+j.key+`"`)
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

// refresh makes Jellyfin re-read the .nfo of each video. Items are matched by
// path, which is why the job mounts the library where Jellyfin does. A video
// Jellyfin has not scanned yet is skipped: its first scan reads the .nfo as
// it already is.
//
// The refresh is a full one with replaceAllMetadata, because only that is
// guaranteed to re-read an .nfo that changed under an existing item. It
// cannot pull anything from the internet as long as the library has no metadata
// fetchers enabled (see README.md).
func (j *jellyfin) refresh(ctx context.Context, paths []string) (int, error) {
	var folders []struct {
		ItemID    string `json:"ItemId"`
		Locations []string
	}
	if err := j.do(ctx, "GET", "/Library/VirtualFolders", nil, &folders); err != nil {
		return 0, err
	}
	byPath := map[string]string{}
	for _, f := range folders {
		if !containsAny(f.Locations, paths) {
			continue
		}
		var items struct {
			Items []struct{ ID, Path string } `json:"Items"`
		}
		q := url.Values{
			"ParentId": {f.ItemID}, "Recursive": {"true"}, "IncludeItemTypes": {"Episode,MusicVideo,Video"},
			"Fields": {"Path"}, "EnableImages": {"false"}, "EnableUserData": {"false"},
		}
		if err := j.do(ctx, "GET", "/Items", q, &items); err != nil {
			return 0, err
		}
		for _, it := range items.Items {
			byPath[it.Path] = it.ID
		}
	}
	n := 0
	for _, p := range paths {
		id, ok := byPath[p]
		if !ok {
			continue
		}
		q := url.Values{
			"metadataRefreshMode": {"FullRefresh"}, "replaceAllMetadata": {"true"},
			"imageRefreshMode": {"Default"}, "replaceAllImages": {"false"},
		}
		if err := j.do(ctx, "POST", "/Items/"+id+"/Refresh", q, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
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
