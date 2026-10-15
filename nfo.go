package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
)

// How the summary and ytdl-sub's original plot share the <plot> element.
// Every layout leaves a marker, which is how a later run tells an injected
// plot from a freshly regenerated one and recovers the original under it.
// A layout change re-injects every .nfo once (one library-wide refresh).
const (
	// PLOT_FORMAT=text: plain text for any client. The original follows
	// this line.
	plotMarker = "──────── YouTube description ────────"

	// PLOT_FORMAT=markdown: jellyfin-web renders the overview as Markdown
	// (markdown-it with HTML, then DOMPurify), as do the apps built on it.
	// The original sits folded in <details>, on one line with <br> breaks, so
	// Markdown cannot reflow it or read a "#hashtag" line as a heading. The
	// comment marks the plot as ours even when there is no original; HTML
	// comments never display.
	mdMarker    = "<!-- video-summarizer -->"
	detailsOpen = "<details><summary>YouTube description</summary>"
	detailsEnd  = "</details>"
)

// injectPlot puts summary at the top of the .nfo plot, keeping the original
// below it in the given format. It edits the <plot> element textually:
// re-marshalling the whole document would reorder or reformat what ytdl-sub
// wrote. Reports whether the file changed.
func injectPlot(nfoPath, summary, format string, dryRun bool) (bool, error) {
	raw, err := os.ReadFile(nfoPath)
	if err != nil {
		return false, err
	}
	doc := string(raw)
	newDoc, err := withPlot(doc, summary, format)
	if err != nil {
		return false, fmt.Errorf("%s: %w", nfoPath, err)
	}
	if newDoc == doc {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	return true, writeAtomic(nfoPath, []byte(newDoc))
}

func withPlot(doc, summary, format string) (string, error) {
	start, end, inner, ok := findPlot(doc)
	current := ""
	if ok {
		var err error
		if current, err = xmlText(inner); err != nil {
			return "", err
		}
	}
	original := originalPlot(current)
	var plot string
	if format == "markdown" {
		plot = summary
		if original != "" {
			plot += "\n\n" + detailsOpen + htmlLines(original) + detailsEnd
		}
		plot += "\n\n" + mdMarker
	} else {
		plot = summary + "\n\n" + plotMarker
		if original != "" {
			plot += "\n" + original
		}
	}
	elem := "<plot>" + escapeText(plot) + "</plot>"
	if ok {
		return doc[:start] + elem + doc[end:], nil
	}
	// No plot at all: add one as the root's last child.
	i := strings.LastIndex(doc, "</")
	if i < 0 {
		return "", fmt.Errorf("no root element")
	}
	return doc[:i] + "  " + elem + "\n" + doc[i:], nil
}

// originalPlot recovers ytdl-sub's plot from a plot in either layout, or
// returns it unchanged if it carries no marker.
func originalPlot(plot string) string {
	if strings.Contains(plot, mdMarker) {
		_, after, found := strings.Cut(plot, detailsOpen)
		if !found {
			return ""
		}
		body, _, _ := strings.Cut(after, detailsEnd)
		return unhtmlLines(body)
	}
	if _, after, found := strings.Cut(plot, plotMarker); found {
		return strings.TrimPrefix(after, "\n")
	}
	return plot
}

// htmlLines escapes text for HTML and joins its lines with <br>, so it stays
// a single HTML block. unhtmlLines reverses it exactly: a literal "<br>" in
// the text was escaped, so every remaining <br> is ours.
func htmlLines(s string) string {
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "").Replace(strings.TrimRight(s, "\n"))
	return strings.ReplaceAll(s, "\n", "<br>")
}

func unhtmlLines(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// nfoPlot returns the plot text an .nfo holds, as Jellyfin would read it.
func nfoPlot(nfoPath string) (string, error) {
	raw, err := os.ReadFile(nfoPath)
	if err != nil {
		return "", err
	}
	_, _, inner, ok := findPlot(string(raw))
	if !ok {
		return "", nil
	}
	return xmlText(inner)
}

// findPlot locates <plot>...</plot> or <plot/>, returning the byte range of
// the whole element and its raw inner content.
func findPlot(doc string) (start, end int, inner string, ok bool) {
	if i := strings.Index(doc, "<plot/>"); i >= 0 {
		return i, i + len("<plot/>"), "", true
	}
	i := strings.Index(doc, "<plot>")
	if i < 0 {
		return 0, 0, "", false
	}
	j := strings.Index(doc[i:], "</plot>")
	if j < 0 {
		return 0, 0, "", false
	}
	return i, i + j + len("</plot>"), doc[i+len("<plot>") : i+j], true
}

// xmlText decodes entities and CDATA in an element's raw content.
func xmlText(inner string) (string, error) {
	var v struct {
		Text string `xml:",chardata"`
	}
	if err := xml.Unmarshal([]byte("<p>"+inner+"</p>"), &v); err != nil {
		return "", err
	}
	return v.Text, nil
}

// escapeText escapes only what XML requires in text content. xml.EscapeText
// also turns every newline into "&#xA;", which is valid but unreadable for
// anyone opening the .nfo.
func escapeText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
