package main

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"sort"
)

type magazineItem struct {
	Article    *Article `json:"article"`
	Thread     string   `json:"thread"`
	ThreadSlug string   `json:"thread_slug"`
	Context    string   `json:"context"`
	ReadTime   int      `json:"read_time"`
	Difficulty string   `json:"difficulty"`
	Score      float64  `json:"score"`
	TileSize   string   `json:"tile_size"` // "large", "medium", "small"
	Completed  bool     `json:"completed"`
}

func (s *server) handleMagazine(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("thread")
	items, err := buildMagazine(s.db, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func buildMagazine(db *sql.DB, threadFilter string) ([]magazineItem, error) {
	scores := engagementScores(db)
	completedSet := completedArticles(db)

	var query string
	var args []any

	if threadFilter != "" {
		query = `SELECT ` + articleSelectCols + `
			FROM articles a
			LEFT JOIN pitches p ON p.article_id = a.id
			INNER JOIN article_threads at2 ON at2.article_id = a.id
			INNER JOIN threads t ON t.id = at2.thread_id AND t.slug = ?
			ORDER BY a.published_at DESC`
		args = []any{threadFilter}
	} else {
		query = `SELECT ` + articleSelectCols + `
			FROM articles a
			LEFT JOIN pitches p ON p.article_id = a.id
			ORDER BY a.published_at DESC`
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	threadMap := articleThreadMap(db)

	var items []magazineItem
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			continue
		}
		a.BodyHTML = ""
		a.PlainText = ""

		var ctx string
		var readTime int
		var diff string
		db.QueryRow(`SELECT context, read_time, difficulty FROM article_meta WHERE article_id = ?`, a.ID).
			Scan(&ctx, &readTime, &diff)

		score := scores[a.ID]

		items = append(items, magazineItem{
			Article:    a,
			Thread:     threadMap[a.ID].title,
			ThreadSlug: threadMap[a.ID].slug,
			Context:    ctx,
			ReadTime:   readTime,
			Difficulty: diff,
			Score:      score,
			Completed:  completedSet[a.ID],
		})
	}

	assignTileSizes(items)

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Score > items[j].Score
	})

	return items, nil
}

type threadInfo struct {
	title string
	slug  string
}

func articleThreadMap(db *sql.DB) map[int64]threadInfo {
	m := map[int64]threadInfo{}
	rows, err := db.Query(`
		SELECT at2.article_id, t.title, t.slug
		FROM article_threads at2
		JOIN threads t ON t.id = at2.thread_id
		ORDER BY at2.relevance DESC`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var aid int64
		var title, slug string
		rows.Scan(&aid, &title, &slug)
		if _, ok := m[aid]; !ok {
			m[aid] = threadInfo{title, slug}
		}
	}
	return m
}

func engagementScores(db *sql.DB) map[int64]float64 {
	scores := map[int64]float64{}

	rows, err := db.Query(`
		SELECT article_id, type, scroll_pct, created_at
		FROM events ORDER BY created_at`)
	if err != nil {
		return scores
	}
	defer rows.Close()

	for rows.Next() {
		var aid int64
		var typ string
		var pct int
		var ts string
		rows.Scan(&aid, &typ, &pct, &ts)

		switch typ {
		case "opened":
			scores[aid] += 1
		case "read_started":
			scores[aid] += 2
		case "completed":
			scores[aid] += 5
		case "abandoned":
			scores[aid] += float64(pct) / 50.0
		case "scrolled":
			scores[aid] += float64(pct) / 100.0
		}
	}
	return scores
}

func completedArticles(db *sql.DB) map[int64]bool {
	m := map[int64]bool{}
	rows, err := db.Query(`SELECT DISTINCT article_id FROM events WHERE type = 'completed'`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		m[id] = true
	}
	return m
}

func assignTileSizes(items []magazineItem) {
	if len(items) == 0 {
		return
	}

	var maxScore float64
	for _, it := range items {
		if it.Score > maxScore {
			maxScore = it.Score
		}
	}

	// Also consider metadata richness: articles with cover images, context,
	// and longer read times deserve more visual space.
	for i := range items {
		richness := 0.0
		if items[i].Article.CoverImage != "" {
			richness += 2
		}
		if items[i].Context != "" {
			richness += 1
		}
		if items[i].ReadTime > 8 {
			richness += 1
		}
		items[i].Score += richness
	}

	// Recompute max after richness boost.
	maxScore = 0
	for _, it := range items {
		if it.Score > maxScore {
			maxScore = it.Score
		}
	}

	if maxScore == 0 {
		maxScore = 1
	}

	for i := range items {
		norm := items[i].Score / maxScore
		switch {
		case norm >= 0.7:
			items[i].TileSize = "large"
		case norm >= 0.3:
			items[i].TileSize = "medium"
		default:
			items[i].TileSize = "small"
		}
	}

	// Ensure variety: cap large tiles at ~20% of total.
	largeCount := 0
	maxLarge := int(math.Max(2, math.Ceil(float64(len(items))*0.2)))
	for i := range items {
		if items[i].TileSize == "large" {
			largeCount++
			if largeCount > maxLarge {
				items[i].TileSize = "medium"
			}
		}
	}

	// Avoid metadata-only tiles: if themes/context exist, use
	// the JSON themes for tag display.
	for i := range items {
		if items[i].Context == "" && items[i].Article.PitchLine != "" {
			items[i].Context = items[i].Article.PitchLine
		}
	}

	// Parse themes from article_meta for richer display.
	// (themes are stored as JSON array in article_meta.themes)
}

func loadArticleThemes(db *sql.DB, articleID int64) []string {
	var raw sql.NullString
	db.QueryRow(`SELECT themes FROM article_meta WHERE article_id = ?`, articleID).Scan(&raw)
	if !raw.Valid {
		return nil
	}
	var themes []string
	json.Unmarshal([]byte(raw.String), &themes)
	return themes
}
