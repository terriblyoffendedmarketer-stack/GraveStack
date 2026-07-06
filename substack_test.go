package main

import "testing"

// A realistic-ish saved-list blob: results wrapped under an unexpected key and
// mixed with unrelated objects, to prove walkPosts finds posts defensively.
const sampleSaved = `{
  "savedPosts": {
    "items": [
      {
        "id": 12345,
        "title": "The Quiet Part of Deep Work",
        "subtitle": "Why the hard hour is the whole game",
        "canonical_url": "https://calnewport.substack.com/p/the-quiet-part",
        "cover_image": "https://img/cover.jpg",
        "post_date": "2025-03-01T10:00:00Z",
        "wordcount": 1800,
        "audience": "everyone",
        "publishedBylines": [{"name": "Cal Newport"}]
      },
      {
        "not_a_post": true
      },
      {
        "id": 999,
        "title": "Paywalled Piece",
        "slug": "paywalled-piece",
        "canonical_url": "https://someone.substack.com/p/paywalled-piece",
        "audience": "only_paid",
        "publishedBylines": [{"name": "Someone"}]
      }
    ]
  },
  "meta": {"cursor": "abc"}
}`

func TestParseSavedJSONDefensive(t *testing.T) {
	posts, err := parseSavedJSON([]byte(sampleSaved))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(posts))
	}
	a := posts[0].normalize()
	if a.SubstackID != "12345" {
		t.Errorf("id = %q", a.SubstackID)
	}
	if a.Subdomain != "calnewport" || a.Slug != "the-quiet-part" {
		t.Errorf("canonical split wrong: sub=%q slug=%q", a.Subdomain, a.Slug)
	}
	if a.Author != "Cal Newport" {
		t.Errorf("author = %q", a.Author)
	}
	paid := posts[1].normalize()
	if !paid.IsPaywalled {
		t.Errorf("expected paywalled")
	}
}

func TestNormalizeCookie(t *testing.T) {
	cases := map[string]string{
		"abc123":          "connect.sid=abc123",
		"connect.sid=abc": "connect.sid=abc",
		"a=1; b=2":        "a=1; b=2",
	}
	for in, want := range cases {
		if got := normalizeCookie(in); got != want {
			t.Errorf("normalizeCookie(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitCanonical(t *testing.T) {
	sub, slug := splitCanonical("https://foo.substack.com/p/hello-world?utm=1")
	if sub != "foo" || slug != "hello-world" {
		t.Errorf("got sub=%q slug=%q", sub, slug)
	}
	sub, slug = splitCanonical("https://blog.custom.com/p/custom-slug")
	if slug != "custom-slug" {
		t.Errorf("custom-domain slug = %q", slug)
	}
}
