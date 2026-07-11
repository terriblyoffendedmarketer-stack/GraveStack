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

// buildGraph processes articles through the AI to create thematic threads and
// relationships. Incremental: only analyzes articles missing from article_meta.
// Pass rebuild=true to force a full reanalysis.
func buildGraph(db *sql.DB, cfg Config) error {
	return buildGraphOpts(db, cfg, false)
}

func buildGraphOpts(db *sql.DB, cfg Config, rebuild bool) error {
	if cfg.AnthropicKey == "" {
		return fmt.Errorf("no Anthropic key available — set it in Settings or as ANTHROPIC_API_KEY")
	}

	allArticles, err := loadAllArticles(db)
	if err != nil {
		return fmt.Errorf("load articles: %w", err)
	}

	// Split into already-analyzed and unanalyzed.
	var toAnalyze []*Article
	var existingMeta []articleAnalysis
	for _, a := range allArticles {
		if rebuild {
			toAnalyze = append(toAnalyze, a)
			continue
		}
		var themes, ctx, diff sql.NullString
		err := db.QueryRow(`SELECT themes, context, difficulty FROM article_meta WHERE article_id = ?`, a.ID).
			Scan(&themes, &ctx, &diff)
		if err != nil {
			toAnalyze = append(toAnalyze, a)
		} else {
			var t []string
			_ = json.Unmarshal([]byte(themes.String), &t)
			existingMeta = append(existingMeta, articleAnalysis{
				ID: a.ID, Themes: t, Context: ctx.String, Difficulty: diff.String,
			})
		}
	}
	log.Printf("graph: %d total articles, %d already analyzed, %d to analyze",
		len(allArticles), len(existingMeta), len(toAnalyze))

	if len(toAnalyze) == 0 && !rebuild {
		log.Printf("graph: nothing new to analyze")
		return nil
	}

	// Phase 1: Analyze unanalyzed articles in batches of 8.
	var newMeta []articleAnalysis
	batch := 8
	for i := 0; i < len(toAnalyze); i += batch {
		end := i + batch
		if end > len(toAnalyze) {
			end = len(toAnalyze)
		}
		log.Printf("graph: analyzing batch %d-%d of %d new articles", i+1, end, len(toAnalyze))
		metas, err := analyzeArticleBatch(cfg, toAnalyze[i:end])
		if err != nil {
			log.Printf("graph: batch %d-%d failed: %v", i+1, end, err)
			continue
		}
		newMeta = append(newMeta, metas...)
		if end < len(toAnalyze) {
			time.Sleep(1 * time.Second)
		}
	}

	// Save new article metadata.
	for _, m := range newMeta {
		themesJSON, _ := json.Marshal(m.Themes)
		readTime := 0
		for _, a := range toAnalyze {
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
	log.Printf("graph: saved metadata for %d new articles", len(newMeta))

	// Combine all metadata for thread/relation generation.
	allMeta := append(existingMeta, newMeta...)

	// Phase 2: Threads — if no threads exist yet, create from scratch.
	// Otherwise, place new articles into existing threads + check emergence.
	var threadCount int
	db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&threadCount)

	if threadCount == 0 {
		log.Printf("graph: creating initial threads")
		if err := createThreads(db, cfg, allArticles, allMeta); err != nil {
			log.Printf("graph: thread creation failed: %v", err)
		}
	} else if len(newMeta) > 0 {
		log.Printf("graph: placing %d new articles into existing threads", len(newMeta))
		for _, m := range newMeta {
			var a *Article
			for _, art := range toAnalyze {
				if art.ID == m.ID {
					a = art
					break
				}
			}
			if a != nil {
				placeArticleInThreads(db, cfg, a, m)
			}
		}
		// Check thread emergence if enough unthreaded articles have accumulated.
		checkThreadEmergence(db, cfg, allArticles, allMeta)
	}

	// Phase 3: Relations — if none exist yet, create from scratch.
	// Otherwise, find relations for new articles only.
	var relCount int
	db.QueryRow(`SELECT COUNT(*) FROM article_relations`).Scan(&relCount)

	if relCount == 0 {
		log.Printf("graph: finding initial relationships")
		if err := findRelations(db, cfg, allArticles, allMeta); err != nil {
			log.Printf("graph: relation finding failed: %v", err)
		}
	} else if len(newMeta) > 0 {
		log.Printf("graph: finding relationships for %d new articles", len(newMeta))
		if err := findNewRelations(db, cfg, allArticles, allMeta, newMeta); err != nil {
			log.Printf("graph: new relation finding failed: %v", err)
		}
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

	// Clear existing threads and rebuild (only used for initial creation).
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
			db.Exec(`INSERT OR IGNORE INTO article_threads(article_id, thread_id, relevance, context) VALUES(?,?,?,?)`,
				aid, threadID, 1.0, ctx)
		}
	}
	log.Printf("graph: created %d threads", len(result.Threads))
	return nil
}

// placeArticleInThreads integrates a single newly-analyzed article into existing
// threads, storing a relevance score for each placement.
func placeArticleInThreads(db *sql.DB, cfg Config, a *Article, m articleAnalysis) {
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
	if len(threads) == 0 {
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "New article — ID: %d | %s (by %s) | themes: %s | context: %s\n\n",
		a.ID, a.Title, a.Author, strings.Join(m.Themes, ", "), m.Context)
	sb.WriteString("Existing threads:\n")
	for _, t := range threads {
		fmt.Fprintf(&sb, "- ID %d: %s — %s\n", t.ID, t.Title, t.Desc)
	}

	placement, err := callAnthropicRaw(cfg,
		`Given a new article and existing thematic threads, decide which threads (if any) this article belongs in. For each placement, rate the fit from 0.0 (weak) to 1.0 (perfect).
Return JSON: {"placements": [{"thread_id": <id>, "relevance": <0.0-1.0>, "context": "why it fits this thread"}]}. Only include threads where there's a genuine fit (relevance >= 0.3).`,
		sb.String(), 500)
	if err != nil {
		log.Printf("graph: place article %d: %v", a.ID, err)
		return
	}

	var result struct {
		Placements []struct {
			ThreadID  int64   `json:"thread_id"`
			Relevance float64 `json:"relevance"`
			Context   string  `json:"context"`
		} `json:"placements"`
	}
	if parseJSONFromResponse(placement, &result) == nil {
		for _, p := range result.Placements {
			db.Exec(`INSERT OR REPLACE INTO article_threads(article_id, thread_id, relevance, context) VALUES(?,?,?,?)`,
				a.ID, p.ThreadID, p.Relevance, p.Context)
		}
		log.Printf("graph: placed article %d into %d threads", a.ID, len(result.Placements))
	}
}

const emergenceThreshold = 8

// checkThreadEmergence looks for articles that aren't placed in any thread (or
// only weakly placed). When enough accumulate, asks the AI whether a new theme
// has emerged.
func checkThreadEmergence(db *sql.DB, cfg Config, articles []*Article, metas []articleAnalysis) {
	rows, err := db.Query(`
		SELECT a.id FROM articles a
		WHERE a.id NOT IN (
			SELECT article_id FROM article_threads WHERE relevance >= 0.5
		)
		AND a.id IN (SELECT article_id FROM article_meta)`)
	if err != nil {
		return
	}
	defer rows.Close()
	unthreaded := map[int64]bool{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		unthreaded[id] = true
	}

	if len(unthreaded) < emergenceThreshold {
		log.Printf("graph: %d unthreaded articles (threshold %d), skipping emergence check",
			len(unthreaded), emergenceThreshold)
		return
	}

	log.Printf("graph: %d unthreaded articles — checking for new themes", len(unthreaded))

	// Build context for the unthreaded articles.
	var sb strings.Builder
	sb.WriteString("These articles don't fit well into existing threads:\n")
	for _, a := range articles {
		if !unthreaded[a.ID] {
			continue
		}
		for _, m := range metas {
			if m.ID == a.ID {
				fmt.Fprintf(&sb, "ID: %d | %s (by %s) | themes: %s | context: %s\n",
					a.ID, a.Title, a.Author, strings.Join(m.Themes, ", "), m.Context)
				break
			}
		}
	}

	// Also list existing threads so the AI can suggest better placements.
	threadRows, err := db.Query(`SELECT id, title, description FROM threads`)
	if err != nil {
		return
	}
	defer threadRows.Close()
	sb.WriteString("\nExisting threads:\n")
	for threadRows.Next() {
		var id int64
		var title, desc string
		threadRows.Scan(&id, &title, &desc)
		fmt.Fprintf(&sb, "- ID %d: %s — %s\n", id, title, desc)
	}

	result, err := callAnthropicRaw(cfg,
		`You're reviewing articles that don't fit existing threads well. Three possible outcomes:

1. Some articles actually DO fit an existing thread — place them (with relevance score)
2. A group of articles forms a new theme — create ONE new thread (at most)
3. An existing thread should split because it's grown too broad

Return JSON:
{
  "placements": [{"article_id": <id>, "thread_id": <existing_id>, "relevance": <0.0-1.0>, "context": "why"}],
  "new_thread": null or {"slug": "...", "title": "...", "description": "...", "icon": "...", "article_ids": [...], "article_contexts": {"<id>": "..."}},
  "reasoning": "one sentence on what you decided and why"
}

Only create a new thread if at least 3 articles genuinely cluster around the same idea. Prefer better placements into existing threads over new threads.`,
		sb.String(), 2000)
	if err != nil {
		log.Printf("graph: emergence check failed: %v", err)
		return
	}

	var emergence struct {
		Placements []struct {
			ArticleID int64   `json:"article_id"`
			ThreadID  int64   `json:"thread_id"`
			Relevance float64 `json:"relevance"`
			Context   string  `json:"context"`
		} `json:"placements"`
		NewThread *struct {
			Slug            string            `json:"slug"`
			Title           string            `json:"title"`
			Description     string            `json:"description"`
			Icon            string            `json:"icon"`
			ArticleIDs      []int64           `json:"article_ids"`
			ArticleContexts map[string]string `json:"article_contexts"`
		} `json:"new_thread"`
		Reasoning string `json:"reasoning"`
	}
	if parseJSONFromResponse(result, &emergence) != nil {
		log.Printf("graph: couldn't parse emergence response")
		return
	}

	for _, p := range emergence.Placements {
		db.Exec(`INSERT OR REPLACE INTO article_threads(article_id, thread_id, relevance, context) VALUES(?,?,?,?)`,
			p.ArticleID, p.ThreadID, p.Relevance, p.Context)
	}
	log.Printf("graph: emergence placed %d articles into existing threads", len(emergence.Placements))

	if emergence.NewThread != nil {
		var maxOrder int
		db.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) FROM threads`).Scan(&maxOrder)
		res, err := db.Exec(`INSERT INTO threads(slug, title, description, icon, color, sort_order, created_at)
			VALUES(?,?,?,?,?,?,?)`,
			emergence.NewThread.Slug, emergence.NewThread.Title, emergence.NewThread.Description,
			emergence.NewThread.Icon, "", maxOrder+1, nowUTC())
		if err == nil {
			threadID, _ := res.LastInsertId()
			for _, aid := range emergence.NewThread.ArticleIDs {
				ctx := ""
				if emergence.NewThread.ArticleContexts != nil {
					ctx = emergence.NewThread.ArticleContexts[fmt.Sprintf("%d", aid)]
				}
				db.Exec(`INSERT OR REPLACE INTO article_threads(article_id, thread_id, relevance, context) VALUES(?,?,?,?)`,
					aid, threadID, 1.0, ctx)
			}
			log.Printf("graph: emergence created new thread %q with %d articles",
				emergence.NewThread.Title, len(emergence.NewThread.ArticleIDs))
		}
	}

	if emergence.Reasoning != "" {
		log.Printf("graph: emergence reasoning: %s", emergence.Reasoning)
	}
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

// findNewRelations discovers connections between newly analyzed articles and the
// entire collection, without rebuilding existing relationships.
func findNewRelations(db *sql.DB, cfg Config, allArticles []*Article, allMeta []articleAnalysis, newMeta []articleAnalysis) error {
	newIDs := map[int64]bool{}
	for _, m := range newMeta {
		newIDs[m.ID] = true
	}

	var sb strings.Builder
	sb.WriteString("NEW articles (find connections FROM these TO the rest):\n")
	for _, a := range allArticles {
		if !newIDs[a.ID] {
			continue
		}
		for _, m := range newMeta {
			if m.ID == a.ID {
				fmt.Fprintf(&sb, "ID: %d | %s (by %s) | themes: %s | context: %s\n",
					a.ID, a.Title, a.Author, strings.Join(m.Themes, ", "), m.Context)
				break
			}
		}
	}

	sb.WriteString("\nEXISTING articles (potential connection targets):\n")
	for _, a := range allArticles {
		if newIDs[a.ID] {
			continue
		}
		for _, m := range allMeta {
			if m.ID == a.ID {
				fmt.Fprintf(&sb, "ID: %d | %s (by %s) | themes: %s | context: %s\n",
					a.ID, a.Title, a.Author, strings.Join(m.Themes, ", "), m.Context)
				break
			}
		}
	}

	pr, err := callAnthropicRaw(cfg, relationsSystemPrompt+"\n\nFocus on connections involving the NEW articles. Each connection must have at least one NEW article.", sb.String(), 4000)
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

	for _, r := range relations {
		db.Exec(`INSERT OR IGNORE INTO article_relations(article_a, article_b, relation, strength, reason)
			VALUES(?,?,?,?,?)`, r.A, r.B, r.Relation, r.Strength, r.Reason)
	}
	log.Printf("graph: found %d new relations", len(relations))
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
	rows, err := db.Query(`SELECT ` + articleSelectCols + `, a.plain_text
		FROM articles a LEFT JOIN pitches p ON p.article_id = a.id
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var articles []*Article
	for rows.Next() {
		var a Article
		var pitchLine, pullQuote, plainText sql.NullString
		err := rows.Scan(
			&a.ID, &a.SubstackID, &a.URL, &a.Subdomain, &a.Slug, &a.Title, &a.Subtitle,
			&a.Author, &a.PublishedAt, &a.WordCount, &a.CoverImage, &a.BodyHTML,
			&a.Topic, &a.IsPaywalled, &a.SavedRank, &a.SyncedAt, &pitchLine, &pullQuote,
			&plainText,
		)
		if err != nil {
			continue
		}
		a.PitchLine = pitchLine.String
		a.PullQuote = pullQuote.String
		a.PlainText = plainText.String
		articles = append(articles, &a)
	}
	return articles, nil
}

// graphStatus returns a summary of the current graph state.
func graphStatus(db *sql.DB) map[string]any {
	var totalArticles, analyzed, threaded, relations, threads int
	db.QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&totalArticles)
	db.QueryRow(`SELECT COUNT(*) FROM article_meta`).Scan(&analyzed)
	db.QueryRow(`SELECT COUNT(DISTINCT article_id) FROM article_threads WHERE relevance >= 0.5`).Scan(&threaded)
	db.QueryRow(`SELECT COUNT(*) FROM article_relations`).Scan(&relations)
	db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&threads)
	return map[string]any{
		"total_articles": totalArticles,
		"analyzed":       analyzed,
		"unanalyzed":     totalArticles - analyzed,
		"threaded":       threaded,
		"unthreaded":     totalArticles - threaded,
		"relations":      relations,
		"threads":        threads,
	}
}
