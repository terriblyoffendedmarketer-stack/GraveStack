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

// Thread represents a thematic grouping of articles.
type Thread struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Color       string `json:"color"`
	SortOrder   int    `json:"sort_order"`
	ArticleCount int   `json:"article_count,omitempty"`
}

// ArticleMeta holds AI-generated context for a single article.
type ArticleMeta struct {
	ArticleID  int64    `json:"article_id"`
	Themes     []string `json:"themes"`
	Context    string   `json:"context"`
	ReadTime   int      `json:"read_time"`
	Difficulty string   `json:"difficulty"`
}

// ArticleRelation is a directed connection between two articles.
type ArticleRelation struct {
	ArticleA int64   `json:"article_a"`
	ArticleB int64   `json:"article_b"`
	Relation string  `json:"relation"`
	Strength float64 `json:"strength"`
	Reason   string  `json:"reason"`
}

const graphSystemPrompt = `You are an intelligent librarian analyzing a personal collection of saved Substack articles. Your job is to understand each article deeply and identify thematic connections.

You will receive a batch of articles with their titles, authors, subtitles, and text excerpts. For each article, provide:

1. themes: 2-4 thematic tags from this controlled vocabulary (use EXACTLY these strings):
   - "self-understanding" (identity, authenticity, self-knowledge, introspection)
   - "connection" (friendship, intimacy, conversations, relationships, loneliness)
   - "philosophy" (existentialism, ethics, meaning, big thinkers, metaphysics)
   - "writing" (craft, essays, articulation, language, storytelling)
   - "internet-culture" (attention, algorithms, doomscrolling, online life, phones)
   - "art-and-taste" (visual art, cinema, music, aesthetics, cultural criticism)
   - "productivity" (habits, focus, ADHD, procrastination, self-improvement)
   - "economics" (capitalism, markets, inequality, political economy)
   - "history" (ancient civilizations, historical analysis, social structures)
   - "marketing" (SEO, content strategy, brand, professional skills)

2. context: ONE sentence explaining what makes this article interesting in this collection — not a summary, but why someone who saved all these articles would want to read THIS one. Be specific and honest.

3. difficulty: "light" (casual read, under 5 min), "medium" (needs focus, 5-15 min), or "deep" (demanding, 15+ min)

Return JSON array: [{"id": <article_id>, "themes": [...], "context": "...", "difficulty": "..."}]`

const threadSystemPrompt = `You are organizing a personal article collection into thematic threads — not categories, but named IDEAS that connect multiple articles. Each thread should feel like a mini-revelation: "oh, these pieces are all about the same thing."

You will receive a list of articles with their themes and context. Create 6-10 threads. Each thread needs:
- slug: kebab-case identifier
- title: a compelling name (not generic — "the articulation project" not "writing articles")
- description: 2-3 sentences explaining the thread's core idea and why these articles belong together
- icon: a Tabler icon name (without "ti-" prefix) that fits the theme
- article_ids: which articles belong (an article can appear in multiple threads)
- article_contexts: for each article, a short phrase explaining why it belongs in THIS thread specifically

Return JSON: {"threads": [{"slug": "...", "title": "...", "description": "...", "icon": "...", "article_ids": [...], "article_contexts": {"<id>": "..."}}]}`

const relationsSystemPrompt = `You are finding meaningful connections between articles in a personal collection. Not just "same topic" — interesting intellectual relationships:

- "deepens": article B takes an idea from A further
- "challenges": B argues against or complicates A's thesis
- "complements": A and B approach the same question from different angles
- "applies": B is a practical application of A's abstract idea
- "echoes": B's argument resonates with A despite being about something different

You will receive articles with their themes and context. Find the 20-30 strongest connections. Only include genuinely interesting ones — if two articles are both about "writing," that alone isn't a connection. The connection should make someone say "oh, I should read these together."

Return JSON array: [{"a": <id>, "b": <id>, "relation": "deepens|challenges|complements|applies|echoes", "strength": 0.0-1.0, "reason": "one sentence explaining the connection"}]`

// buildGraph processes all articles through the AI to create thematic threads
// and article relationships. This is the one-time bulk job that powers the
// entire intelligent UI.
func buildGraph(db *sql.DB, cfg Config) error {
	if cfg.AnthropicKey == "" {
		return fmt.Errorf("no Anthropic key available — set it in Settings or as ANTHROPIC_API_KEY")
	}

	articles, err := loadAllArticles(db)
	if err != nil {
		return fmt.Errorf("load articles: %w", err)
	}
	log.Printf("graph: processing %d articles", len(articles))

	// Phase 1: Analyze each article (in batches of 8)
	var allMeta []articleAnalysis
	batch := 8
	for i := 0; i < len(articles); i += batch {
		end := i + batch
		if end > len(articles) {
			end = len(articles)
		}
		log.Printf("graph: analyzing batch %d-%d of %d", i+1, end, len(articles))
		metas, err := analyzeArticleBatch(cfg, articles[i:end])
		if err != nil {
			log.Printf("graph: batch %d-%d failed: %v", i+1, end, err)
			continue
		}
		allMeta = append(allMeta, metas...)
		if end < len(articles) {
			time.Sleep(1 * time.Second)
		}
	}

	// Save article metadata
	for _, m := range allMeta {
		themesJSON, _ := json.Marshal(m.Themes)
		readTime := 0
		for _, a := range articles {
			if a.ID == m.ID {
				readTime = (a.WordCount + 249) / 250
				break
			}
		}
		_, err := db.Exec(`INSERT INTO article_meta(article_id, themes, context, read_time, difficulty, analyzed_at)
			VALUES(?,?,?,?,?,?) ON CONFLICT(article_id) DO UPDATE SET
			themes=excluded.themes, context=excluded.context, read_time=excluded.read_time,
			difficulty=excluded.difficulty, analyzed_at=excluded.analyzed_at`,
			m.ID, string(themesJSON), m.Context, readTime, m.Difficulty, nowUTC())
		if err != nil {
			log.Printf("graph: save meta %d: %v", m.ID, err)
		}
	}
	log.Printf("graph: saved metadata for %d articles", len(allMeta))

	// Phase 2: Create threads from the analyzed articles
	log.Printf("graph: creating threads")
	if err := createThreads(db, cfg, articles, allMeta); err != nil {
		log.Printf("graph: thread creation failed: %v", err)
	}

	// Phase 3: Find cross-article relationships
	log.Printf("graph: finding relationships")
	if err := findRelations(db, cfg, articles, allMeta); err != nil {
		log.Printf("graph: relation finding failed: %v", err)
	}

	log.Printf("graph: complete")
	return nil
}

type articleAnalysis struct {
	ID         int64    `json:"id"`
	Themes     []string `json:"themes"`
	Context    string   `json:"context"`
	Difficulty string   `json:"difficulty"`
}

func analyzeArticleBatch(cfg Config, articles []*Article) ([]articleAnalysis, error) {
	var sb strings.Builder
	for _, a := range articles {
		text := a.PlainText
		if len(text) > 3000 {
			text = text[:3000]
		}
		fmt.Fprintf(&sb, "\n---\nID: %d\nTitle: %s\nAuthor: %s\nSubtitle: %s\nWord count: %d\n\nExcerpt:\n%s\n",
			a.ID, a.Title, a.Author, a.Subtitle, a.WordCount, text)
	}

	pr, err := callAnthropicRaw(cfg, graphSystemPrompt, sb.String(), 2000)
	if err != nil {
		return nil, err
	}

	var results []articleAnalysis
	if err := parseJSONFromResponse(pr, &results); err != nil {
		return nil, fmt.Errorf("parse analysis: %w", err)
	}
	return results, nil
}

func createThreads(db *sql.DB, cfg Config, articles []*Article, metas []articleAnalysis) error {
	var sb strings.Builder
	for _, a := range articles {
		for _, m := range metas {
			if m.ID == a.ID {
				fmt.Fprintf(&sb, "ID: %d | %s (by %s) | themes: %s | context: %s\n",
					a.ID, a.Title, a.Author, strings.Join(m.Themes, ", "), m.Context)
				break
			}
		}
	}

	pr, err := callAnthropicRaw(cfg, threadSystemPrompt, sb.String(), 4000)
	if err != nil {
		return err
	}

	var result struct {
		Threads []struct {
			Slug            string            `json:"slug"`
			Title           string            `json:"title"`
			Description     string            `json:"description"`
			Icon            string            `json:"icon"`
			ArticleIDs      []int64           `json:"article_ids"`
			ArticleContexts map[string]string `json:"article_contexts"`
		} `json:"threads"`
	}
	if err := parseJSONFromResponse(pr, &result); err != nil {
		return fmt.Errorf("parse threads: %w", err)
	}

	// Clear existing threads and rebuild
	db.Exec(`DELETE FROM article_threads`)
	db.Exec(`DELETE FROM threads`)

	for i, t := range result.Threads {
		res, err := db.Exec(`INSERT INTO threads(slug, title, description, icon, color, sort_order, created_at)
			VALUES(?,?,?,?,?,?,?)`,
			t.Slug, t.Title, t.Description, t.Icon, "", i, nowUTC())
		if err != nil {
			log.Printf("graph: insert thread %s: %v", t.Slug, err)
			continue
		}
		threadID, _ := res.LastInsertId()

		for _, aid := range t.ArticleIDs {
			ctx := ""
			if t.ArticleContexts != nil {
				ctx = t.ArticleContexts[fmt.Sprintf("%d", aid)]
			}
			db.Exec(`INSERT OR IGNORE INTO article_threads(article_id, thread_id, context) VALUES(?,?,?)`,
				aid, threadID, ctx)
		}
	}
	log.Printf("graph: created %d threads", len(result.Threads))
	return nil
}

func findRelations(db *sql.DB, cfg Config, articles []*Article, metas []articleAnalysis) error {
	var sb strings.Builder
	for _, a := range articles {
		for _, m := range metas {
			if m.ID == a.ID {
				fmt.Fprintf(&sb, "ID: %d | %s (by %s) | themes: %s | context: %s\n",
					a.ID, a.Title, a.Author, strings.Join(m.Themes, ", "), m.Context)
				break
			}
		}
	}

	pr, err := callAnthropicRaw(cfg, relationsSystemPrompt, sb.String(), 4000)
	if err != nil {
		return err
	}

	var relations []struct {
		A        int64   `json:"a"`
		B        int64   `json:"b"`
		Relation string  `json:"relation"`
		Strength float64 `json:"strength"`
		Reason   string  `json:"reason"`
	}
	if err := parseJSONFromResponse(pr, &relations); err != nil {
		return fmt.Errorf("parse relations: %w", err)
	}

	db.Exec(`DELETE FROM article_relations`)
	for _, r := range relations {
		db.Exec(`INSERT OR IGNORE INTO article_relations(article_a, article_b, relation, strength, reason)
			VALUES(?,?,?,?,?)`, r.A, r.B, r.Relation, r.Strength, r.Reason)
	}
	log.Printf("graph: created %d relations", len(relations))
	return nil
}

// callAnthropicRaw is a general-purpose Anthropic call with custom system prompt
// and higher token limits for graph processing.
func callAnthropicRaw(cfg Config, system, prompt string, maxTokens int) (string, error) {
	return callAnthropicWith(cfg, system, prompt, maxTokens)
}

func callAnthropicWith(cfg Config, system, prompt string, maxTokens int) (string, error) {
	if cfg.AnthropicKey == "" {
		return "", fmt.Errorf("no Anthropic key")
	}
	reqBody := map[string]any{
		"model":      cfg.AnthropicModel,
		"max_tokens": maxTokens,
		"system":     system,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	buf, _ := json.Marshal(reqBody)

	req, err := http.NewRequest("POST", anthropicAPI, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", cfg.AnthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("anthropic status %d: %s", resp.StatusCode, string(body))
	}

	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if len(out.Content) == 0 {
		return "", fmt.Errorf("empty response")
	}
	return out.Content[0].Text, nil
}

// parseJSONFromResponse extracts JSON from a model response that may contain
// prose or code fences around the JSON.
func parseJSONFromResponse(s string, v any) error {
	// Try direct parse first
	if err := json.Unmarshal([]byte(s), v); err == nil {
		return nil
	}
	// Try extracting from code fence
	if i := strings.Index(s, "```json"); i >= 0 {
		s = s[i+7:]
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
		return json.Unmarshal([]byte(strings.TrimSpace(s)), v)
	}
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
		return json.Unmarshal([]byte(strings.TrimSpace(s)), v)
	}
	// Try finding array or object
	start := strings.IndexAny(s, "[{")
	if start < 0 {
		return fmt.Errorf("no JSON found in response")
	}
	opener := s[start]
	closer := byte(']')
	if opener == '{' {
		closer = '}'
	}
	end := strings.LastIndexByte(s, closer)
	if end <= start {
		return fmt.Errorf("no complete JSON found")
	}
	return json.Unmarshal([]byte(s[start:end+1]), v)
}

func loadAllArticles(db *sql.DB) ([]*Article, error) {
	rows, err := db.Query(`SELECT ` + articleSelectCols + `
		FROM articles a LEFT JOIN pitches p ON p.article_id = a.id
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var articles []*Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			continue
		}
		// We need plain_text for analysis
		var pt sql.NullString
		db.QueryRow(`SELECT plain_text FROM articles WHERE id = ?`, a.ID).Scan(&pt)
		a.PlainText = pt.String
		articles = append(articles, a)
	}
	return articles, nil
}

// addArticleToGraph processes a single new article and integrates it into
// the existing graph. Called after sync for newly added articles.
func addArticleToGraph(db *sql.DB, cfg Config, articleID int64) {
	if cfg.AnthropicKey == "" {
		return
	}
	a, err := getArticle(db, articleID)
	if err != nil {
		return
	}
	var pt sql.NullString
	db.QueryRow(`SELECT plain_text FROM articles WHERE id = ?`, a.ID).Scan(&pt)
	a.PlainText = pt.String

	metas, err := analyzeArticleBatch(cfg, []*Article{a})
	if err != nil || len(metas) == 0 {
		return
	}
	m := metas[0]
	themesJSON, _ := json.Marshal(m.Themes)
	readTime := (a.WordCount + 249) / 250
	db.Exec(`INSERT INTO article_meta(article_id, themes, context, read_time, difficulty, analyzed_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(article_id) DO UPDATE SET
		themes=excluded.themes, context=excluded.context, read_time=excluded.read_time,
		difficulty=excluded.difficulty, analyzed_at=excluded.analyzed_at`,
		m.ID, string(themesJSON), m.Context, readTime, m.Difficulty, nowUTC())

	// Find which existing threads this article fits into
	rows, err := db.Query(`SELECT id, slug, title, description FROM threads`)
	if err != nil {
		return
	}
	defer rows.Close()
	type threadInfo struct {
		ID    int64
		Slug  string
		Title string
		Desc  string
	}
	var threads []threadInfo
	for rows.Next() {
		var t threadInfo
		rows.Scan(&t.ID, &t.Slug, &t.Title, &t.Desc)
		threads = append(threads, t)
	}

	if len(threads) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "New article — ID: %d | %s (by %s) | themes: %s | context: %s\n\n",
			a.ID, a.Title, a.Author, strings.Join(m.Themes, ", "), m.Context)
		sb.WriteString("Existing threads:\n")
		for _, t := range threads {
			fmt.Fprintf(&sb, "- ID %d: %s — %s\n", t.ID, t.Title, t.Desc)
		}

		placement, err := callAnthropicRaw(cfg,
			`Given a new article and existing thematic threads, decide which threads (if any) this article belongs in. Return JSON: {"placements": [{"thread_id": <id>, "context": "why it fits"}]}. Only include threads where there's a genuine fit.`,
			sb.String(), 500)
		if err == nil {
			var result struct {
				Placements []struct {
					ThreadID int64  `json:"thread_id"`
					Context  string `json:"context"`
				} `json:"placements"`
			}
			if parseJSONFromResponse(placement, &result) == nil {
				for _, p := range result.Placements {
					db.Exec(`INSERT OR IGNORE INTO article_threads(article_id, thread_id, context) VALUES(?,?,?)`,
						a.ID, p.ThreadID, p.Context)
				}
			}
		}
	}
}
