package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const anthropicAPI = "https://api.anthropic.com/v1/messages"

// pitchResult is what the LLM returns: one honest "what this actually is" line
// and one verbatim pull quote. Per the brief, the LLM's job is not to invent
// persuasion but to extend the title's promise with real specifics + a real
// sample of the author's voice.
type pitchResult struct {
	PitchLine string `json:"pitch_line"`
	PullQuote string `json:"pull_quote"`
}

const pitchSystem = `You write honest, high-signal pitches for saved articles the reader already chose to save. You do NOT invent hype. Your job:
1. pitch_line: ONE sentence stating what this piece actually is and why this specific reader would like it — concrete and informative (e.g. "a 12-minute argument that X, worth it for the section on Y"). Honest, not clickbait.
2. pull_quote: the single strongest sentence or two taken VERBATIM from the article body — a real sample of the author's voice. Copy it exactly, character for character. Do not paraphrase, do not add ellipses, do not fix punctuation.
Return ONLY minified JSON: {"pitch_line":"...","pull_quote":"..."}`

// generatePitch calls Anthropic and verifies the pull quote is really in the
// text. If the model paraphrased, we retry once asking for an exact quote; if it
// still fails, we fall back to a real sentence extracted locally so the promise
// ("a free sample of the voice") is never broken.
func generatePitch(cfg Config, a *Article) (pitchResult, error) {
	text := a.PlainText
	if text == "" {
		text = a.Subtitle
	}
	if len(text) > 16000 {
		text = text[:16000]
	}
	prompt := fmt.Sprintf("Title: %s\nAuthor: %s\nSubtitle: %s\n\nArticle body:\n%s",
		a.Title, a.Author, a.Subtitle, text)

	pr, err := callAnthropic(cfg, prompt)
	if err != nil {
		return pitchResult{}, err
	}
	if !quoteIsVerbatim(pr.PullQuote, text) && text != "" {
		// One retry, more emphatic.
		pr2, err2 := callAnthropic(cfg, prompt+"\n\nIMPORTANT: pull_quote MUST be copied exactly from the article body above.")
		if err2 == nil && quoteIsVerbatim(pr2.PullQuote, text) {
			pr = pr2
		} else {
			pr.PullQuote = firstStrongSentence(text)
		}
	}
	return pr, nil
}

// quoteIsVerbatim normalizes whitespace so a genuine copy that differs only in
// spacing still counts, but a paraphrase does not.
func quoteIsVerbatim(quote, text string) bool {
	q := strings.TrimSpace(quote)
	if len(q) < 15 {
		return false
	}
	return strings.Contains(normalizeWS(text), normalizeWS(q))
}

func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// firstStrongSentence returns a reasonably long early sentence as a guaranteed
// real sample when the model won't produce a verbatim quote.
func firstStrongSentence(text string) string {
	for _, sep := range []string{". ", "! ", "? "} {
		for _, s := range strings.Split(text, sep) {
			s = strings.TrimSpace(s)
			if len(s) >= 60 && len(s) <= 280 {
				return s + strings.TrimSpace(sep)
			}
		}
	}
	if len(text) > 200 {
		return strings.TrimSpace(text[:200])
	}
	return strings.TrimSpace(text)
}

func callAnthropic(cfg Config, prompt string) (pitchResult, error) {
	if cfg.AnthropicKey == "" {
		return pitchResult{}, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}
	reqBody := map[string]any{
		"model":      cfg.AnthropicModel,
		"max_tokens": 400,
		"system":     pitchSystem,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	buf, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", anthropicAPI, bytes.NewReader(buf))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", cfg.AnthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return pitchResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return pitchResult{}, fmt.Errorf("anthropic status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return pitchResult{}, err
	}
	if len(out.Content) == 0 {
		return pitchResult{}, fmt.Errorf("empty anthropic response")
	}
	return parsePitchJSON(out.Content[0].Text)
}

// parsePitchJSON tolerates the model wrapping JSON in prose or code fences.
func parsePitchJSON(s string) (pitchResult, error) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return pitchResult{}, fmt.Errorf("no JSON object in model output")
	}
	var pr pitchResult
	if err := json.Unmarshal([]byte(s[start:end+1]), &pr); err != nil {
		return pitchResult{}, err
	}
	return pr, nil
}

// savePitch stores/updates the pitch for an article.
func savePitch(db *sql.DB, articleID int64, pr pitchResult, model string) error {
	_, err := db.Exec(`INSERT INTO pitches(article_id, pitch_line, pull_quote, model, generated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(article_id) DO UPDATE SET
			pitch_line = excluded.pitch_line,
			pull_quote = excluded.pull_quote,
			model = excluded.model,
			generated_at = excluded.generated_at`,
		articleID, pr.PitchLine, pr.PullQuote, model, nowUTC())
	return err
}

// generatePitchesFor runs pitch generation for the given article IDs in the
// background, best-effort. Used after a sync so the reading UI has pitches ready.
func generatePitchesFor(db *sql.DB, cfg Config, ids []int64) {
	for _, id := range ids {
		a, err := getArticle(db, id)
		if err != nil {
			continue
		}
		pr, err := generatePitch(cfg, a)
		if err != nil {
			log.Printf("pitch %d (%s): %v", id, a.Title, err)
			continue
		}
		if err := savePitch(db, id, pr, cfg.AnthropicModel); err != nil {
			log.Printf("save pitch %d: %v", id, err)
		}
	}
}

// ensurePitch generates a pitch on demand if one is missing (e.g. sync happened
// before a key was set). Returns the article with pitch fields populated.
func ensurePitch(db *sql.DB, cfg Config, a *Article) {
	if a.PitchLine != "" {
		return
	}
	pr, err := generatePitch(cfg, a)
	if err != nil {
		// Graceful fallback: subtitle as the pitch line, a real sentence as quote.
		a.PitchLine = a.Subtitle
		a.PullQuote = firstStrongSentence(a.PlainText)
		return
	}
	_ = savePitch(db, a.ID, pr, cfg.AnthropicModel)
	a.PitchLine = pr.PitchLine
	a.PullQuote = pr.PullQuote
}
