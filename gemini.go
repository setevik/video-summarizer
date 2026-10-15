package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/genai"
)

// Summarizer is the model call, behind an interface so the run loop can be
// tested without the network.
type Summarizer interface {
	FromVideo(ctx context.Context, m Meta) (Summary, Usage, error)
	FromTranscript(ctx context.Context, m Meta, transcript string) (Summary, Usage, error)
}

type Usage struct {
	PromptTokens, OutputTokens, ThoughtTokens int
	Latency                                   time.Duration
}

type gemini struct {
	client   *genai.Client
	model    string
	language string
	thinking genai.ThinkingLevel
	timeout  time.Duration
}

// newGemini authenticates from the environment: GEMINI_API_KEY for the
// Gemini API, or GOOGLE_GENAI_USE_VERTEXAI + GOOGLE_CLOUD_PROJECT +
// GOOGLE_CLOUD_LOCATION + Application Default Credentials for Vertex AI. Which
// one is a deployment decision, so the code does not pick.
func newGemini(ctx context.Context, c Config) (*gemini, error) {
	client, err := genai.NewClient(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &gemini{client: client, model: c.Model, language: c.SummaryLanguage, thinking: genai.ThinkingLevel(strings.ToUpper(c.ThinkingLevel)), timeout: c.CallTimeout}, nil
}

var summarySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"overview": map[string]any{
			"type":        "string",
			"description": "2-3 sentences: what the video is about and what it concludes or shows.",
		},
		"key_points": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"timestamp": map[string]any{"type": "string", "description": "Where the point starts: MM:SS, or H:MM:SS past the first hour."},
					"point":     map[string]any{"type": "string", "description": "One sentence."},
				},
				"required": []string{"timestamp", "point"},
			},
		},
	},
	"required": []string{"overview", "key_points"},
}

func (g *gemini) config() *genai.GenerateContentConfig {
	c := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(
			"You summarize videos for a personal media library. Write in "+g.language+", whatever language the video is in. "+
				"Be concrete: name the actual ideas, steps, results and examples rather than describing the video's structure. "+
				"Use only what the video itself contains; the description is context and may be promotional.", genai.RoleUser),
		ResponseMIMEType:   "application/json",
		ResponseJsonSchema: summarySchema,
		// Low resolution is ~100 tokens per second of video instead of ~300.
		// Summaries depend on speech and on-screen text, not fine detail.
		MediaResolution: genai.MediaResolutionLow,
	}
	if g.thinking != "" {
		c.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: g.thinking}
	}
	return c
}

func (g *gemini) FromVideo(ctx context.Context, m Meta) (Summary, Usage, error) {
	parts := []*genai.Part{
		genai.NewPartFromURI(m.URL, "video/mp4"),
		genai.NewPartFromText(prompt(m, "")),
	}
	return g.generate(ctx, parts, m.Duration)
}

func (g *gemini) FromTranscript(ctx context.Context, m Meta, transcript string) (Summary, Usage, error) {
	return g.generate(ctx, []*genai.Part{genai.NewPartFromText(prompt(m, transcript))}, m.Duration)
}

func (g *gemini) generate(ctx context.Context, parts []*genai.Part, duration int) (Summary, Usage, error) {
	if g.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.timeout)
		defer cancel()
	}
	start := time.Now()
	resp, err := g.client.Models.GenerateContent(ctx, g.model, []*genai.Content{genai.NewContentFromParts(parts, genai.RoleUser)}, g.config())
	u := Usage{Latency: time.Since(start)}
	if err != nil {
		return Summary{}, u, err
	}
	if md := resp.UsageMetadata; md != nil {
		u.PromptTokens, u.OutputTokens, u.ThoughtTokens = int(md.PromptTokenCount), int(md.CandidatesTokenCount), int(md.ThoughtsTokenCount)
	}
	text := resp.Text()
	if text == "" {
		reason := "empty response"
		if len(resp.Candidates) > 0 {
			reason = "finish reason " + string(resp.Candidates[0].FinishReason)
		} else if resp.PromptFeedback != nil {
			reason = "blocked: " + string(resp.PromptFeedback.BlockReason)
		}
		return Summary{}, u, errors.New(reason)
	}
	var s Summary
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		return Summary{}, u, fmt.Errorf("decoding model output: %w", err)
	}
	s.tidy(duration)
	if s.Overview == "" {
		return Summary{}, u, errors.New("model returned no overview")
	}
	return s, u, nil
}

func prompt(m Meta, transcript string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Title: %s\n", m.Title)
	if m.Channel != "" {
		fmt.Fprintf(&b, "Channel: %s\n", m.Channel)
	}
	if m.Duration > 0 {
		fmt.Fprintf(&b, "Length: %s\n", formatTS(m.Duration))
	}
	if d := strings.TrimSpace(m.Description); d != "" {
		fmt.Fprintf(&b, "\nYouTube description (viewers see it right below your summary):\n<<<\n%s\n>>>\n", truncate(d, 4000))
	}
	if len(m.Chapters) > 0 {
		b.WriteString("\nThe creator's chapters, each with the time it starts:\n")
		for _, c := range m.Chapters {
			fmt.Fprintf(&b, "%s %s\n", formatTS(c.Start), c.Title)
		}
	}
	if transcript != "" {
		fmt.Fprintf(&b, "\nTranscript, each line prefixed with the time it starts:\n<<<\n%s>>>\n", transcript)
	}
	lo, hi := pointRange(m.Duration)
	fmt.Fprintf(&b, "\nSummarize the video: an overview of 2-3 sentences, then %d-%d key points in the order they appear, each with the timestamp where it begins.", lo, hi)
	if len(m.Chapters) > 0 {
		// Creator chapters are exact; the model's own timestamps are not.
		b.WriteString(" Follow the creator's chapters: a point about a chapter starts at that chapter's time. Skip intro, sponsor and outro chapters, and merge short ones to stay within the range.")
	}
	if strings.TrimSpace(m.Description) != "" {
		b.WriteString(" Do not restate the description: say what the video covers that it does not.")
	}
	return b.String()
}

// pointRange scales the number of key points with length: a one-minute clip
// has two things to say, a lecture has a dozen.
func pointRange(sec int) (int, int) {
	switch {
	case sec == 0:
		return 4, 8
	case sec < 180:
		return 2, 4
	case sec < 900:
		return 4, 6
	case sec < 2700:
		return 5, 9
	default:
		return 8, 14
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// Errors from the API, by what the run loop should do about them.
func isQuota(err error) bool {
	var ae genai.APIError
	return errors.As(err, &ae) && ae.Code == 429
}

// isTransient covers server errors, timeouts and network failures: none of
// them says anything about the video.
func isTransient(err error) bool {
	var ae genai.APIError
	if errors.As(err, &ae) {
		return ae.Code >= 500
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// authReasons are google.rpc.ErrorInfo reasons that mean the credentials or
// the project, not the video, are at fault. A bare 403 is not enough: Gemini
// also answers 403 for a private video, which falls back to its transcript.
var authReasons = map[string]bool{
	"API_KEY_INVALID": true, "API_KEY_EXPIRED": true, "API_KEY_SERVICE_BLOCKED": true,
	"API_KEY_HTTP_REFERRER_BLOCKED": true, "API_KEY_IP_ADDRESS_BLOCKED": true,
	"ACCESS_TOKEN_EXPIRED": true, "ACCESS_TOKEN_SCOPE_INSUFFICIENT": true,
	"SERVICE_DISABLED": true, "BILLING_DISABLED": true, "CONSUMER_INVALID": true,
	"CONSUMER_SUSPENDED": true, "IAM_PERMISSION_DENIED": true,
}

// isAuth reports a credentials or project problem, which would fail every
// video alike.
func isAuth(err error) bool {
	var ae genai.APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Code == 401 {
		return true
	}
	for _, d := range ae.Details {
		if r, _ := d["reason"].(string); authReasons[r] {
			return true
		}
	}
	return false
}
