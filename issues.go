package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type Issue struct {
	ID           int64          `json:"id"`
	Title        string         `json:"title"`
	Query        string         `json:"query"`
	Writeup      string         `json:"writeup"`
	MainPick     int64          `json:"main_pick"`
	Supporting   []int64        `json:"supporting"`
	Articles     map[string]any `json:"articles,omitempty"`
	ArticleCount int            `json:"article_count"`
	CreatedAt    string         `json:"created_at"`
}

func normalizeQuery(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	q = regexp.MustCompile(`[^\w\s]+`).ReplaceAllString(q, "")
	q = regexp.MustCompile(`\s+`).ReplaceAllString(q, " ")
	// Remove common stop words for better matching.
	stops := map[string]bool{
		"a": true, "an": true, "the": true, "is": true, "are": true,
		"was": true, "were": true, "in": true, "on": true, "at": true,
		"to": true, "of": true, "for": true, "and": true, "or": true,
		"what": true, "about": true, "do": true, "i": true, "my": true,
		"me": true, "can": true, "you": true, "tell": true, "show": true,
	}
	words := strings.Fields(q)
	var kept []string
	for _, w := range words {
		if !stops[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return q
	}
	return strings.Join(kept, " ")
}

func findSimilarIssue(db *sql.DB, normQuery string, currentArticleCount int) *Issue {
	row := db.QueryRow(`SELECT id, title, query, writeup, main_pick, supporting, article_count, created_at
		FROM issues WHERE query_norm = ? ORDER BY created_at DESC LIMIT 1`, normQuery)
	issue, err := scanIssue(row)
	if err != nil {
		return nil
	}
	// If article count has grown significantly, don't reuse.
	if currentArticleCount > issue.ArticleCount+5 {
		return nil
	}
	return issue
}

func saveIssue(db *sql.DB, title, query, writeup string, mainPick int64, supporting []int64, articleCount int) (*Issue, error) {
	supJSON, _ := json.Marshal(supporting)
	norm := normalizeQuery(query)
	res, err := db.Exec(`INSERT INTO issues(title, query, query_norm, writeup, main_pick, supporting, article_count, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		title, query, norm, writeup, mainPick, string(supJSON), articleCount, nowUTC())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Issue{
		ID: id, Title: title, Query: query, Writeup: writeup,
		MainPick: mainPick, Supporting: supporting,
		ArticleCount: articleCount, CreatedAt: nowUTC(),
	}, nil
}

func scanIssue(s interface{ Scan(...any) error }) (*Issue, error) {
	var issue Issue
	var supJSON sql.NullString
	err := s.Scan(&issue.ID, &issue.Title, &issue.Query, &issue.Writeup,
		&issue.MainPick, &supJSON, &issue.ArticleCount, &issue.CreatedAt)
	if err != nil {
		return nil, err
	}
	if supJSON.Valid {
		json.Unmarshal([]byte(supJSON.String), &issue.Supporting)
	}
	return &issue, nil
}

func enrichIssueArticles(db *sql.DB, issue *Issue) {
	articles := map[string]any{}
	allIDs := append([]int64{issue.MainPick}, issue.Supporting...)
	for _, id := range allIDs {
		if id <= 0 {
			continue
		}
		a, err := getArticle(db, id)
		if err != nil {
			continue
		}
		articles[fmt.Sprintf("%d", id)] = map[string]any{
			"id": a.ID, "title": a.Title, "author": a.Author,
			"cover_image_url": a.CoverImage, "word_count": a.WordCount,
			"pitch_line": a.PitchLine, "subtitle": a.Subtitle,
		}
	}
	issue.Articles = articles
}

// --- handlers ---

func (s *server) handleAskWithIssues(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	if err := readJSON(r, &body); err != nil || strings.TrimSpace(body.Query) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg := s.cfgForRequest(r)

	// Count current articles.
	var articleCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&articleCount)

	// Check for existing similar issue.
	norm := normalizeQuery(body.Query)
	existing := findSimilarIssue(s.db, norm, articleCount)
	if existing != nil {
		enrichIssueArticles(s.db, existing)
		writeJSON(w, http.StatusOK, map[string]any{
			"issue":      existing,
			"writeup":    existing.Writeup,
			"main_pick":  existing.MainPick,
			"supporting": existing.Supporting,
			"articles":   existing.Articles,
			"cached":     true,
		})
		return
	}

	// Generate fresh — same logic as the original handleAsk.
	rows, err := s.db.Query(`SELECT a.id, a.title, a.author, a.subtitle, am.themes, am.context
		FROM articles a LEFT JOIN article_meta am ON am.article_id = a.id
		ORDER BY a.id`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var catalog strings.Builder
	for rows.Next() {
		var id int64
		var title, author, subtitle string
		var themes, ctx sql.NullString
		rows.Scan(&id, &title, &author, &subtitle, &themes, &ctx)
		context := ctx.String
		if context == "" {
			context = subtitle
		}
		fmt.Fprintf(&catalog, "ID:%d | %s (by %s) | themes: %s | %s\n",
			id, title, author, themes.String, context)
	}

	prompt := fmt.Sprintf("User's collection (%d articles):\n%s\n\nUser asks: %s", articleCount, catalog.String(), body.Query)

	result, err := callAnthropicRaw(cfg,
		`You are a thoughtful librarian for someone's personal article collection. They're asking about a topic — find the most relevant articles and write a short, engaging response.

Your response MUST be valid JSON with this structure:
{"title": "A short, evocative title for this mini-issue (3-8 words, like a magazine section header)", "writeup": "2-3 paragraphs exploring the user's question, weaving in references to specific articles by title. Teach them something from the articles, don't just list them. Make the writeup itself valuable to read.", "main_pick": <article_id>, "supporting": [<article_id>, ...], "reasoning": "one sentence on why you chose these"}

The writeup should feel like a knowledgeable friend saying "oh, you're interested in that? Here's what your own collection has to say about it." Reference articles naturally, by title and author, as part of the narrative.`,
		prompt, 2000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var response map[string]any
	if err := parseJSONFromResponse(result, &response); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"writeup": result, "main_pick": nil, "supporting": nil})
		return
	}

	// Extract fields for saving.
	title, _ := response["title"].(string)
	if title == "" {
		title = body.Query
	}
	writeup, _ := response["writeup"].(string)
	var mainPick int64
	if mp, ok := response["main_pick"].(float64); ok {
		mainPick = int64(mp)
	}
	var supporting []int64
	if sup, ok := response["supporting"].([]any); ok {
		for _, v := range sup {
			if id, ok := v.(float64); ok && id > 0 {
				supporting = append(supporting, int64(id))
			}
		}
	}

	// Save as issue.
	issue, err := saveIssue(s.db, title, body.Query, writeup, mainPick, supporting, articleCount)
	if err != nil {
		// Non-fatal — return the result even if save fails.
		_ = err
	}

	// Enrich with article details.
	articles := map[string]any{}
	allPicks := append([]int64{mainPick}, supporting...)
	for _, id := range allPicks {
		if id <= 0 {
			continue
		}
		a, err := getArticle(s.db, id)
		if err != nil {
			continue
		}
		articles[fmt.Sprintf("%d", id)] = map[string]any{
			"id": a.ID, "title": a.Title, "author": a.Author,
			"cover_image_url": a.CoverImage, "word_count": a.WordCount,
			"pitch_line": a.PitchLine, "subtitle": a.Subtitle,
		}
	}
	response["articles"] = articles
	if issue != nil {
		response["issue"] = issue
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, title, query, writeup, main_pick, supporting, article_count, created_at
		FROM issues ORDER BY created_at DESC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var issues []Issue
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			continue
		}
		issues = append(issues, *issue)
	}
	if issues == nil {
		issues = []Issue{}
	}
	writeJSON(w, http.StatusOK, issues)
}

func (s *server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	row := s.db.QueryRow(`SELECT id, title, query, writeup, main_pick, supporting, article_count, created_at
		FROM issues WHERE id = ?`, id)
	issue, err := scanIssue(row)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	enrichIssueArticles(s.db, issue)
	writeJSON(w, http.StatusOK, issue)
}

func (s *server) handleDeleteIssue(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	s.db.Exec(`DELETE FROM issues WHERE id = ?`, id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleMergeIssues(w http.ResponseWriter, r *http.Request) {
	keepID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	mergeID, err := strconv.ParseInt(r.PathValue("other"), 10, 64)
	if err != nil {
		http.Error(w, "bad other id", http.StatusBadRequest)
		return
	}

	keepRow := s.db.QueryRow(`SELECT id, title, query, writeup, main_pick, supporting, article_count, created_at
		FROM issues WHERE id = ?`, keepID)
	keep, err := scanIssue(keepRow)
	if err != nil {
		http.Error(w, "issue not found", http.StatusNotFound)
		return
	}

	mergeRow := s.db.QueryRow(`SELECT id, title, query, writeup, main_pick, supporting, article_count, created_at
		FROM issues WHERE id = ?`, mergeID)
	merge, err := scanIssue(mergeRow)
	if err != nil {
		http.Error(w, "merge issue not found", http.StatusNotFound)
		return
	}

	// Combine writeups with a separator.
	combined := keep.Writeup + "\n\n---\n\n" + merge.Writeup

	// Merge supporting articles, dedup.
	seen := map[int64]bool{keep.MainPick: true}
	for _, id := range keep.Supporting {
		seen[id] = true
	}
	for _, id := range merge.Supporting {
		if !seen[id] && id > 0 {
			keep.Supporting = append(keep.Supporting, id)
			seen[id] = true
		}
	}
	if !seen[merge.MainPick] && merge.MainPick > 0 {
		keep.Supporting = append(keep.Supporting, merge.MainPick)
	}

	supJSON, _ := json.Marshal(keep.Supporting)
	s.db.Exec(`UPDATE issues SET writeup = ?, supporting = ? WHERE id = ?`,
		combined, string(supJSON), keepID)
	s.db.Exec(`DELETE FROM issues WHERE id = ?`, mergeID)

	keep.Writeup = combined
	enrichIssueArticles(s.db, keep)
	writeJSON(w, http.StatusOK, keep)
}
