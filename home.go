package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"
)

type homeResponse struct {
	Featured    *homeArticle   `json:"featured"`
	Suggestions []*homeArticle `json:"suggestions"`
	Writeup     string         `json:"writeup"`
	Threads     []Thread       `json:"threads"`
}

type homeArticle struct {
	Article  *Article `json:"article"`
	Context  string   `json:"context"`
	Reason   string   `json:"reason"`
	ReadTime int      `json:"read_time"`
	Thread   string   `json:"thread"`
}

type graphArticleInfo struct {
	ID         int64
	Themes     []string
	Context    string
	ReadTime   int
	Difficulty string
	ThreadIDs  []int64
}

func buildHome(db *sql.DB, cfg Config) (*homeResponse, error) {
	eligible, _, err := eligibleArticleIDs(db)
	if err != nil || len(eligible) == 0 {
		return nil, errNoArticles
	}
	eligibleSet := map[int64]bool{}
	for _, id := range eligible {
		eligibleSet[id] = true
	}
	infoMap := map[int64]*graphArticleInfo{}
	for _, id := range eligible {
		var themes, ctx, diff sql.NullString
		var readTime int
		err := db.QueryRow(`SELECT themes, context, read_time, difficulty FROM article_meta WHERE article_id = ?`, id).
			Scan(&themes, &ctx, &readTime, &diff)
		if err != nil {
			continue
		}
		info := &graphArticleInfo{ID: id, Context: ctx.String, ReadTime: readTime, Difficulty: diff.String}
		_ = json.Unmarshal([]byte(themes.String), &info.Themes)
		infoMap[id] = info
	}

	// Load thread memberships for eligible articles.
	threadNames := map[int64]string{}
	{
		rows, err := db.Query(`SELECT id, title FROM threads`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var title string
				rows.Scan(&id, &title)
				threadNames[id] = title
			}
		}
	}
	for _, id := range eligible {
		rows, err := db.Query(`SELECT thread_id FROM article_threads WHERE article_id = ? AND relevance >= 0.5 ORDER BY relevance DESC`, id)
		if err != nil {
			continue
		}
		for rows.Next() {
			var tid int64
			rows.Scan(&tid)
			if info, ok := infoMap[id]; ok {
				info.ThreadIDs = append(info.ThreadIDs, tid)
			}
		}
		rows.Close()
	}

	// Pick the featured article: weighted toward recent saves, preferring
	// articles with graph metadata.
	featuredID := pickFeatured(eligible, infoMap)
	featured, err := getArticle(db, featuredID)
	if err != nil {
		return nil, err
	}
	ensurePitch(db, cfg, featured)

	featuredInfo := infoMap[featuredID]
	featuredThread := ""
	if featuredInfo != nil && len(featuredInfo.ThreadIDs) > 0 {
		featuredThread = threadNames[featuredInfo.ThreadIDs[0]]
	}
	featuredContext := ""
	featuredReadTime := 0
	if featuredInfo != nil {
		featuredContext = featuredInfo.Context
		featuredReadTime = featuredInfo.ReadTime
	}

	home := &homeResponse{
		Featured: &homeArticle{
			Article:  featured,
			Context:  featuredContext,
			ReadTime: featuredReadTime,
			Thread:   featuredThread,
		},
	}

	// Pick 2-3 suggestions: diverse threads, different from featured.
	suggestions := pickSuggestions(db, eligible, infoMap, threadNames, featuredID, featuredInfo)
	for _, s := range suggestions {
		a, err := getArticle(db, s.ID)
		if err != nil {
			continue
		}
		ensurePitch(db, cfg, a)
		thread := ""
		if len(s.ThreadIDs) > 0 {
			thread = threadNames[s.ThreadIDs[0]]
		}
		home.Suggestions = append(home.Suggestions, &homeArticle{
			Article:  a,
			Context:  s.Context,
			Reason:   s.reason,
			ReadTime: s.ReadTime,
			Thread:   thread,
		})
	}

	// Generate daily write-up if we have an API key.
	if cfg.AnthropicKey != "" && len(home.Suggestions) > 0 {
		home.Writeup = generateWriteup(cfg, home.Featured, home.Suggestions)
	}

	// Include threads for nav.
	home.Threads = listThreads(db)

	return home, nil
}

func pickFeatured(eligible []int64, infoMap map[int64]*graphArticleInfo) int64 {
	// Prefer articles that have graph metadata (analyzed).
	var withMeta []int64
	for _, id := range eligible {
		if _, ok := infoMap[id]; ok {
			withMeta = append(withMeta, id)
		}
	}
	pool := withMeta
	if len(pool) == 0 {
		pool = eligible
	}

	n := len(pool)
	weights := make([]int, n)
	total := 0
	for i := range pool {
		w := n - i + 2
		weights[i] = w
		total += w
	}
	r := rand.Intn(total)
	for i, w := range weights {
		if r < w {
			return pool[i]
		}
		r -= w
	}
	return pool[n-1]
}

type suggestionCandidate struct {
	*graphArticleInfo
	reason string
}

func pickSuggestions(db *sql.DB, eligible []int64, infoMap map[int64]*graphArticleInfo, threadNames map[int64]string, featuredID int64, featuredInfo *graphArticleInfo) []suggestionCandidate {
	var candidates []suggestionCandidate
	usedThreads := map[int64]bool{}
	if featuredInfo != nil {
		for _, tid := range featuredInfo.ThreadIDs {
			usedThreads[tid] = true
		}
	}

	// Strategy 1: Find a related article (from article_relations).
	var relatedID int64
	var relReason string
	db.QueryRow(`SELECT CASE WHEN article_a = ? THEN article_b ELSE article_a END, reason
		FROM article_relations WHERE article_a = ? OR article_b = ?
		ORDER BY strength DESC LIMIT 1`, featuredID, featuredID, featuredID).
		Scan(&relatedID, &relReason)
	if relatedID > 0 {
		if info, ok := infoMap[relatedID]; ok {
			candidates = append(candidates, suggestionCandidate{info, relReason})
			for _, tid := range info.ThreadIDs {
				usedThreads[tid] = true
			}
		}
	}

	// Strategy 2: Pick from a different thread than the featured.
	for _, id := range eligible {
		if len(candidates) >= 3 {
			break
		}
		if id == featuredID || id == relatedID {
			continue
		}
		info, ok := infoMap[id]
		if !ok {
			continue
		}
		newThread := false
		for _, tid := range info.ThreadIDs {
			if !usedThreads[tid] {
				newThread = true
				break
			}
		}
		if !newThread && len(info.ThreadIDs) > 0 {
			continue
		}
		reason := ""
		if len(info.ThreadIDs) > 0 {
			reason = "From " + threadNames[info.ThreadIDs[0]]
		}
		candidates = append(candidates, suggestionCandidate{info, reason})
		for _, tid := range info.ThreadIDs {
			usedThreads[tid] = true
		}
	}

	// Fill remaining slots with random eligible articles.
	if len(candidates) < 2 {
		usedIDs := map[int64]bool{featuredID: true, relatedID: true}
		for _, c := range candidates {
			usedIDs[c.ID] = true
		}
		for _, id := range eligible {
			if len(candidates) >= 3 {
				break
			}
			if usedIDs[id] {
				continue
			}
			if info, ok := infoMap[id]; ok {
				candidates = append(candidates, suggestionCandidate{info, ""})
				usedIDs[id] = true
			}
		}
	}

	if len(candidates) > 3 {
		candidates = candidates[:3]
	}
	return candidates
}

func generateWriteup(cfg Config, featured *homeArticle, suggestions []*homeArticle) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Today's featured: %s (by %s) — %s\n",
		featured.Article.Title, featured.Article.Author, featured.Context)
	sb.WriteString("Also suggested:\n")
	for _, s := range suggestions {
		fmt.Fprintf(&sb, "- %s (by %s) — %s\n",
			s.Article.Title, s.Article.Author, s.Context)
	}

	result, err := callAnthropicRaw(cfg,
		`You are writing a brief daily note for someone's personal reading app. You have a featured article and 2-3 suggestions for today. Write 2-3 sentences that connect them — what's the thread between today's picks, what mood they create together, or what the reader might discover. Be warm but not saccharine, specific but not spoilery. Don't list the articles — weave them into a narrative. Write in second person ("you"). Return ONLY the text, no JSON.`,
		sb.String(), 300)
	if err != nil {
		log.Printf("home: writeup generation failed: %v", err)
		return ""
	}
	return strings.TrimSpace(result)
}

func listThreads(db *sql.DB) []Thread {
	rows, err := db.Query(`SELECT t.id, t.slug, t.title, t.description, t.icon, t.color, t.sort_order,
		(SELECT COUNT(*) FROM article_threads WHERE thread_id = t.id) as article_count
		FROM threads t ORDER BY t.sort_order`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var threads []Thread
	for rows.Next() {
		var t Thread
		if err := rows.Scan(&t.ID, &t.Slug, &t.Title, &t.Description, &t.Icon, &t.Color, &t.SortOrder, &t.ArticleCount); err != nil {
			continue
		}
		threads = append(threads, t)
	}
	return threads
}
