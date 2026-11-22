package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A recording split into "<name> - partNN" files is one stacked item in
// Jellyfin, whose path is the first part's.
var partName = regexp.MustCompile(`^(.*) - part(\d+)$`)

type videoPart struct {
	number int
	video  Video
}

// stackedParts groups videos by recording, each in part order. A lone
// "part01" is not a stack.
func stackedParts(videos []Video) [][]videoPart {
	groups := map[string][]videoPart{}
	for _, v := range videos {
		m := partName.FindStringSubmatch(v.Base)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		groups[m[1]] = append(groups[m[1]], videoPart{n, v})
	}
	var out [][]videoPart
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].number < g[j].number })
		out = append(out, g)
	}
	return out
}

// combineParts replaces each stack's overviews with one for its first part:
// the opening paragraph of every part's existing summary. It makes no model
// calls and leaves each part's .nfo and .summary.md as they are.
func (r *runner) combineParts(videos []Video, overviews map[string]string) {
	for _, parts := range stackedParts(videos) {
		plot, err := r.partsPlot(parts)
		if err != nil {
			r.log.Warn("combine parts", "video", parts[0].video.Name(), "err", err)
			plot = ""
		}
		for _, p := range parts {
			delete(overviews, p.video.Path)
		}
		if plot != "" {
			overviews[parts[0].video.Path] = plot
		}
	}
}

func (r *runner) partsPlot(parts []videoPart) (string, error) {
	var lines []string
	found := false
	for _, p := range parts {
		text := "No summary for this part."
		md, err := os.ReadFile(p.video.SummaryPath())
		switch {
		case err == nil:
			text = r.openingParagraph(string(md))
			found = true
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
		if r.cfg.PlotFormat == "markdown" {
			lines = append(lines, fmt.Sprintf("**Part %d.** %s", p.number, text))
		} else {
			lines = append(lines, fmt.Sprintf("Part %d: %s", p.number, text))
		}
	}
	if !found {
		return "", nil
	}
	current, err := nfoPlot(parts[0].video.NFOPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return composePlot(strings.Join(lines, "\n\n"), originalPlot(current), r.cfg.PlotFormat), nil
}

func (r *runner) openingParagraph(md string) string {
	body := plotText(md)
	if r.cfg.PlotFormat == "markdown" {
		body = plotMarkdown(md)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(body), "\n\n")
	return strings.Join(strings.Fields(first), " ")
}
