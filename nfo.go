package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
)

// The line between the summary and ytdl-sub's original plot. Its presence is
// how a later run tells an injected plot from a freshly regenerated one, so
// changing it re-injects every .nfo once (harmless, just a library-wide
// refresh).
const plotMarker = "──────── YouTube description ────────"

// injectPlot puts summary at the top of the .nfo plot, keeping the original
// below the marker. It edits the <plot> element textually: re-marshalling the
// whole document would reorder or reformat what ytdl-sub wrote. Reports
// whether the file changed.
func injectPlot(nfoPath, summary string, dryRun bool) (bool, error) {
	raw, err := os.ReadFile(nfoPath)
	if err != nil {
		return false, err
	}
	doc := string(raw)
	newDoc, err := withPlot(doc, summary)
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

func withPlot(doc, summary string) (string, error) {
	start, end, inner, ok := findPlot(doc)
	original := ""
	if ok {
		var err error
		if original, err = xmlText(inner); err != nil {
			return "", err
		}
	}
	if _, after, found := strings.Cut(original, plotMarker); found {
		original = strings.TrimPrefix(after, "\n")
	}
	plot := summary + "\n\n" + plotMarker
	if original != "" {
		plot += "\n" + original
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
