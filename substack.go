package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
)

// defaultSavedListURL is a best-guess for Substack's undocumented saved-posts
// endpoint. It is very likely to drift — the user can override it in Settings
// ("saved_list_url"), or bypass it entirely via the paste-JSON importer. The
// response is parsed defensively (walkPosts) so schema changes rarely break us.
const defaultSavedListURL = "https://substack.com/api/v1/reader/posts/saved?limit=100"

// rawPost is the loose shape we try to pull out of any Substack JSON blob.
// Every field is optional; walkPosts collects whatever it can find.
type rawPost struct {
	ID               json.Number `json:"id"`
	PostID           json.Number `json:"post_id"`
	CanonicalURL     string      `json:"canonical_url"`
	SlugField        string      `json:"slug"`
	Title            string      `json:"title"`
	Subtitle         string      `json:"subtitle"`
	CoverImage       string      `json:"cover_image"`
	PostDate         string      `json:"post_date"`
	Wordcount        int         `json:"wordcount"`
	Audience         string      `json:"audience"`
	Type             string      `json:"type"`
	PublishedBylines []struct {
		Name string `json:"name"`
	} `json:"publishedBylines"`
}

// syncResult is returned to the UI after a sync run.
type syncResult struct {
	New     int      `json:"new"`
	Skipped int      `json:"skipped"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
}

var htmlPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("src", "srcset", "alt").OnElements("img")
	p.AllowAttrs("class").Globally()
	p.RequireNoReferrerOnLinks(true)
	return p
}()

var tagStripper = regexp.MustCompile(`<[^>]*>`)

func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// substackGet performs an authenticated GET with the session cookie. cookie may
// be a bare connect.sid value or a full "k=v; k2=v2" Cookie header — we forward
// it verbatim so the user can paste whatever DevTools shows.
func substackGet(cookie, rawurl string) ([]byte, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", normalizeCookie(cookie))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 GraveStack")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", rawurl, resp.StatusCode)
	}
	return body, nil
}

// normalizeCookie turns a bare "sid-value" into "connect.sid=sid-value"; a value
// that already contains "=" is assumed to be a complete Cookie header.
func normalizeCookie(c string) string {
	c = strings.TrimSpace(c)
	if c == "" || strings.Contains(c, "=") {
		return c
	}
	return "connect.sid=" + c
}

// walkPosts recursively descends arbitrary decoded JSON looking for objects that
// look like posts (have a title and some URL/slug). This tolerates the saved
// endpoint wrapping results under "posts", "items", "savedPosts", etc.
func walkPosts(v any, out *[]rawPost) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			walkPosts(e, out)
		}
	case map[string]any:
		if looksLikePost(t) {
			if rp, ok := decodePost(t); ok {
				*out = append(*out, rp)
				return
			}
		}
		for _, e := range t {
			walkPosts(e, out)
		}
	}
}

func looksLikePost(m map[string]any) bool {
	_, hasTitle := m["title"]
	if !hasTitle {
		return false
	}
	_, u1 := m["canonical_url"]
	_, u2 := m["slug"]
	return u1 || u2
}

func decodePost(m map[string]any) (rawPost, bool) {
	b, err := json.Marshal(m)
	if err != nil {
		return rawPost{}, false
	}
	var rp rawPost
	if err := json.Unmarshal(b, &rp); err != nil {
		return rawPost{}, false
	}
	if rp.Title == "" || (rp.CanonicalURL == "" && rp.SlugField == "") {
		return rawPost{}, false
	}
	return rp, true
}

// parseSavedJSON extracts posts from a raw saved-list JSON blob (from the live
// endpoint or pasted by the user).
func parseSavedJSON(blob []byte) ([]rawPost, error) {
	var v any
	if err := json.Unmarshal(blob, &v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var posts []rawPost
	walkPosts(v, &posts)
	return posts, nil
}

// normalize turns a rawPost into a partial Article (no body yet).
func (rp rawPost) normalize() Article {
	id := string(rp.ID)
	if id == "" {
		id = string(rp.PostID)
	}
	sub, slug := splitCanonical(rp.CanonicalURL)
	if slug == "" {
		slug = rp.SlugField
	}
	if id == "" { // last-resort stable id
		id = rp.CanonicalURL
	}
	author := ""
	if len(rp.PublishedBylines) > 0 {
		author = rp.PublishedBylines[0].Name
	}
	return Article{
		SubstackID:  id,
		URL:         rp.CanonicalURL,
		Subdomain:   sub,
		Slug:        slug,
		Title:       rp.Title,
		Subtitle:    rp.Subtitle,
		Author:      author,
		PublishedAt: rp.PostDate,
		WordCount:   rp.Wordcount,
		CoverImage:  rp.CoverImage,
		IsPaywalled: rp.Audience == "only_paid",
	}
}

var canonicalRE = regexp.MustCompile(`^https?://([^./]+)\.substack\.com/p/([^/?#]+)`)

func splitCanonical(u string) (subdomain, slug string) {
	if m := canonicalRE.FindStringSubmatch(u); m != nil {
		return m[1], m[2]
	}
	// Custom domain: derive host as subdomain-ish, slug from /p/<slug>.
	if pu, err := url.Parse(u); err == nil {
		if i := strings.Index(pu.Path, "/p/"); i >= 0 {
			return pu.Host, strings.Trim(pu.Path[i+3:], "/")
		}
	}
	return "", ""
}

// fetchBody pulls the full post JSON from the post's own publication and returns
// sanitized HTML + plain text. body_html is full when the cookie has access,
// otherwise a truncated preview (still useful for a pitch).
func fetchBody(cookie string, a *Article) error {
	if a.Slug == "" {
		return fmt.Errorf("no slug for %q", a.Title)
	}
	host := a.Subdomain
	if !strings.Contains(host, ".") {
		host = host + ".substack.com"
	}
	api := fmt.Sprintf("https://%s/api/v1/posts/%s", host, a.Slug)
	blob, err := substackGet(cookie, api)
	if err != nil {
		return err
	}
	var pj struct {
		BodyHTML          string `json:"body_html"`
		TruncatedBodyText string `json:"truncated_body_text"`
		Wordcount         int    `json:"wordcount"`
		CoverImage        string `json:"cover_image"`
		Audience          string `json:"audience"`
	}
	if err := json.Unmarshal(blob, &pj); err != nil {
		return err
	}
	if pj.CoverImage != "" && a.CoverImage == "" {
		a.CoverImage = pj.CoverImage
	}
	if pj.Wordcount > 0 {
		a.WordCount = pj.Wordcount
	}
	if pj.BodyHTML != "" {
		a.BodyHTML = htmlPolicy.Sanitize(pj.BodyHTML)
		a.PlainText = htmlToText(pj.BodyHTML)
		a.IsPaywalled = false
	} else {
		a.BodyHTML = "<p>" + html.EscapeString(pj.TruncatedBodyText) + "</p>"
		a.PlainText = pj.TruncatedBodyText
		a.IsPaywalled = true
	}
	return nil
}

func htmlToText(h string) string {
	t := tagStripper.ReplaceAllString(h, " ")
	t = strings.ReplaceAll(t, "&nbsp;", " ")
	return strings.Join(strings.Fields(t), " ")
}

// insertArticle stores a fully-populated article, skipping duplicates by
// substack_id. Returns (id, inserted).
func insertArticle(db *sql.DB, a *Article) (int64, bool, error) {
	var existing int64
	err := db.QueryRow(`SELECT id FROM articles WHERE substack_id = ?`, a.SubstackID).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	res, err := db.Exec(`INSERT INTO articles
		(substack_id, url, subdomain, slug, title, subtitle, author, published_at,
		 word_count, cover_image_url, body_html, plain_text, topic, is_paywalled, saved_rank, synced_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.SubstackID, a.URL, a.Subdomain, a.Slug, a.Title, a.Subtitle, a.Author,
		a.PublishedAt, a.WordCount, a.CoverImage, a.BodyHTML, a.PlainText, a.Topic,
		boolToInt(a.IsPaywalled), a.SavedRank, nowUTC())
	if err != nil {
		return 0, false, err
	}
	id, _ := res.LastInsertId()
	return id, true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// runSync is the core importer. Either savedJSON (pasted) or cookie+endpoint
// (live) provides the saved list; full bodies are fetched with the cookie when
// available. Pitch generation is kicked off by the caller for new articles.
func runSync(db *sql.DB, cfg Config, cookie, savedJSON, endpoint string) (syncResult, []int64, error) {
	var res syncResult
	var blob []byte
	var err error

	if strings.TrimSpace(savedJSON) != "" {
		blob = []byte(savedJSON)
	} else {
		if cookie == "" {
			return res, nil, fmt.Errorf("no cookie and no pasted JSON provided")
		}
		if endpoint == "" {
			endpoint = defaultSavedListURL
		}
		blob, err = substackGet(cookie, endpoint)
		if err != nil {
			return res, nil, fmt.Errorf("fetch saved list: %w", err)
		}
	}

	posts, err := parseSavedJSON(blob)
	if err != nil {
		return res, nil, err
	}
	if len(posts) == 0 {
		return res, nil, fmt.Errorf("no posts found in saved list (endpoint may have changed — try the paste-JSON option)")
	}

	var newIDs []int64
	for rank, rp := range posts {
		a := rp.normalize()
		a.SavedRank = rank // 0 = most recently saved (assumes API returns newest first)

		var exists int64
		if db.QueryRow(`SELECT id FROM articles WHERE substack_id = ?`, a.SubstackID).Scan(&exists) == nil {
			res.Skipped++
			continue
		}
		if cookie != "" {
			if err := fetchBody(cookie, &a); err != nil {
				res.Failed++
				if len(res.Errors) < 5 {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", a.Title, err))
				}
				// Still store metadata-only so it can surface (pitch from subtitle).
			}
		}
		id, inserted, err := insertArticle(db, &a)
		if err != nil {
			res.Failed++
			continue
		}
		if inserted {
			res.New++
			newIDs = append(newIDs, id)
		} else {
			res.Skipped++
		}
	}
	return res, newIDs, nil
}
