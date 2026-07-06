package main

import "testing"

func TestQuoteIsVerbatim(t *testing.T) {
	text := "The hard hour is the whole game.  You do not get   the payoff without it."
	if !quoteIsVerbatim("You do not get the payoff without it.", text) {
		t.Error("whitespace-normalized exact quote should match")
	}
	if quoteIsVerbatim("You always get the payoff regardless.", text) {
		t.Error("paraphrase should NOT match")
	}
	if quoteIsVerbatim("too short", text) {
		t.Error("short quote should be rejected")
	}
}

func TestFirstStrongSentence(t *testing.T) {
	text := "Short. This is a much longer sentence that easily clears the sixty character floor for a fallback quote. Tail."
	got := firstStrongSentence(text)
	if len(got) < 60 {
		t.Errorf("fallback too short: %q", got)
	}
}

func TestParsePitchJSON(t *testing.T) {
	// Model may wrap JSON in prose / fences.
	raw := "Here you go:\n```json\n{\"pitch_line\":\"a\",\"pull_quote\":\"b\"}\n```"
	pr, err := parsePitchJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pr.PitchLine != "a" || pr.PullQuote != "b" {
		t.Errorf("got %+v", pr)
	}
}
