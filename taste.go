package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

type tasteResponse struct {
	Numbers    tasteNumbers     `json:"numbers"`
	TopThreads []threadStat     `json:"top_threads"`
	TopAuthors []authorStat     `json:"top_authors"`
	TopArticles []articleStat   `json:"top_articles"`
	Queries    []queryStat      `json:"queries"`
	ReadLengths readLengthDist  `json:"read_lengths"`
}

type tasteNumbers struct {
	TotalArticles  int     `json:"total_articles"`
	Completed      int     `json:"completed"`
	Highlights     int     `json:"highlights"`
	Notes          int     `json:"notes"`
	QueriesMade    int     `json:"queries_made"`
	AvgScrollDepth int     `json:"avg_scroll_depth"`
	ThisWeek       int     `json:"this_week"`
	ThisMonth      int     `json:"this_month"`
	Loved          int     `json:"loved"`
}

type threadStat struct {
	Title       string  `json:"title"`
	Slug        string  `json:"slug"`
	Icon        string  `json:"icon"`
	ReadCount   int     `json:"read_count"`
	AvgScore    float64 `json:"avg_score"`
	ArticleCount int    `json:"article_count"`
}

type authorStat struct {
	Name      string  `json:"name"`
	ReadCount int     `json:"read_count"`
	AvgScore  float64 `json:"avg_score"`
	Articles  int     `json:"articles"`
}

type articleStat struct {
	ID    int64   `json:"id"`
	Title string  `json:"title"`
	Author string `json:"author"`
	Score float64 `json:"score"`
}

type queryStat struct {
	Title     string `json:"title"`
	Query     string `json:"query"`
	CreatedAt string `json:"created_at"`
}

type readLengthDist struct {
	Short  int `json:"short"`
	Medium int `json:"medium"`
	Long   int `json:"long"`
}

func buildTaste(db *sql.DB) (*tasteResponse, error) {
	t := &tasteResponse{}

	// --- Numbers ---
	db.QueryRow(`SELECT COUNT(*) FROM articles`).Scan(&t.Numbers.TotalArticles)
	db.QueryRow(`SELECT COUNT(DISTINCT article_id) FROM events WHERE type = 'completed'`).Scan(&t.Numbers.Completed)
	db.QueryRow(`SELECT COUNT(*) FROM highlights`).Scan(&t.Numbers.Highlights)
	db.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&t.Numbers.QueriesMade)
	db.QueryRow(`SELECT COUNT(DISTINCT article_id) FROM events WHERE type = 'loved'`).Scan(&t.Numbers.Loved)

	db.QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&t.Numbers.Notes)

	db.QueryRow(`SELECT COALESCE(AVG(scroll_pct), 0) FROM events
		WHERE type IN ('completed', 'abandoned', 'scrolled')`).Scan(&t.Numbers.AvgScrollDepth)

	db.QueryRow(`SELECT COUNT(DISTINCT article_id) FROM events
		WHERE type = 'completed' AND created_at > datetime('now', '-7 days')`).Scan(&t.Numbers.ThisWeek)
	db.QueryRow(`SELECT COUNT(DISTINCT article_id) FROM events
		WHERE type = 'completed' AND created_at > datetime('now', '-30 days')`).Scan(&t.Numbers.ThisMonth)

	// --- Top threads by engagement ---
	{
		rows, err := db.Query(`
			SELECT t.title, t.slug, t.icon,
				COUNT(DISTINCT e.article_id) as read_count,
				COALESCE(AVG(eng.score), 0) as avg_score,
				(SELECT COUNT(*) FROM article_threads WHERE thread_id = t.id) as total
			FROM threads t
			JOIN article_threads at2 ON at2.thread_id = t.id
			JOIN events e ON e.article_id = at2.article_id AND e.type IN ('completed', 'read_started', 'loved')
			LEFT JOIN (
				SELECT article_id,
					SUM(CASE type WHEN 'opened' THEN 1.0 WHEN 'read_started' THEN 2.0
						WHEN 'completed' THEN 5.0 WHEN 'loved' THEN 8.0 WHEN 'meh' THEN -3.0
						WHEN 'abandoned' THEN scroll_pct / 50.0
						ELSE scroll_pct / 100.0 END) as score
				FROM events GROUP BY article_id
			) eng ON eng.article_id = at2.article_id
			GROUP BY t.id
			ORDER BY avg_score DESC
			LIMIT 8`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var s threadStat
				rows.Scan(&s.Title, &s.Slug, &s.Icon, &s.ReadCount, &s.AvgScore, &s.ArticleCount)
				t.TopThreads = append(t.TopThreads, s)
			}
		}
	}

	// --- Top authors ---
	{
		rows, err := db.Query(`
			SELECT a.author,
				COUNT(DISTINCT e.article_id) as read_count,
				COALESCE(AVG(eng.score), 0) as avg_score,
				COUNT(DISTINCT a.id) as total_articles
			FROM articles a
			JOIN events e ON e.article_id = a.id AND e.type IN ('completed', 'read_started', 'loved')
			LEFT JOIN (
				SELECT article_id,
					SUM(CASE type WHEN 'opened' THEN 1.0 WHEN 'read_started' THEN 2.0
						WHEN 'completed' THEN 5.0 WHEN 'loved' THEN 8.0 WHEN 'meh' THEN -3.0
						WHEN 'abandoned' THEN scroll_pct / 50.0
						ELSE scroll_pct / 100.0 END) as score
				FROM events GROUP BY article_id
			) eng ON eng.article_id = a.id
			WHERE a.author != ''
			GROUP BY a.author
			ORDER BY avg_score DESC
			LIMIT 8`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var s authorStat
				rows.Scan(&s.Name, &s.ReadCount, &s.AvgScore, &s.Articles)
				t.TopAuthors = append(t.TopAuthors, s)
			}
		}
	}

	// --- Most-engaged articles ---
	{
		rows, err := db.Query(`
			SELECT a.id, a.title, a.author,
				SUM(CASE e.type WHEN 'opened' THEN 1.0 WHEN 'read_started' THEN 2.0
					WHEN 'completed' THEN 5.0 WHEN 'loved' THEN 8.0 WHEN 'meh' THEN -3.0
					WHEN 'abandoned' THEN e.scroll_pct / 50.0
					ELSE e.scroll_pct / 100.0 END) as score
			FROM articles a
			JOIN events e ON e.article_id = a.id
			GROUP BY a.id
			HAVING score > 0
			ORDER BY score DESC
			LIMIT 10`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var s articleStat
				rows.Scan(&s.ID, &s.Title, &s.Author, &s.Score)
				t.TopArticles = append(t.TopArticles, s)
			}
		}
	}

	// --- Queries (from issues) ---
	{
		rows, err := db.Query(`SELECT title, query, created_at FROM issues ORDER BY created_at DESC LIMIT 20`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var q queryStat
				rows.Scan(&q.Title, &q.Query, &q.CreatedAt)
				t.Queries = append(t.Queries, q)
			}
		}
	}

	// --- Read length distribution ---
	{
		db.QueryRow(`SELECT COUNT(*) FROM articles a
			JOIN events e ON e.article_id = a.id AND e.type = 'completed'
			WHERE a.word_count < 1500`).Scan(&t.ReadLengths.Short)
		db.QueryRow(`SELECT COUNT(*) FROM articles a
			JOIN events e ON e.article_id = a.id AND e.type = 'completed'
			WHERE a.word_count BETWEEN 1500 AND 4000`).Scan(&t.ReadLengths.Medium)
		db.QueryRow(`SELECT COUNT(*) FROM articles a
			JOIN events e ON e.article_id = a.id AND e.type = 'completed'
			WHERE a.word_count > 4000`).Scan(&t.ReadLengths.Long)
	}

	// Ensure non-nil slices for JSON.
	if t.TopThreads == nil { t.TopThreads = []threadStat{} }
	if t.TopAuthors == nil { t.TopAuthors = []authorStat{} }
	if t.TopArticles == nil { t.TopArticles = []articleStat{} }
	if t.Queries == nil { t.Queries = []queryStat{} }

	return t, nil
}

func (s *server) handleTaste(w http.ResponseWriter, r *http.Request) {
	taste, err := buildTaste(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Inject top themes from article_meta for the heatmap.
	type themeCount struct {
		Theme string `json:"theme"`
		Count int    `json:"count"`
	}
	themeCounts := map[string]int{}
	rows, err := s.db.Query(`
		SELECT m.themes FROM article_meta m
		JOIN events e ON e.article_id = m.article_id AND e.type IN ('completed', 'read_started', 'loved')`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var raw string
			rows.Scan(&raw)
			var themes []string
			json.Unmarshal([]byte(raw), &themes)
			for _, th := range themes {
				themeCounts[th]++
			}
		}
	}
	var themes []themeCount
	for th, c := range themeCounts {
		themes = append(themes, themeCount{th, c})
	}
	// Sort by count desc.
	for i := 0; i < len(themes); i++ {
		for j := i + 1; j < len(themes); j++ {
			if themes[j].Count > themes[i].Count {
				themes[i], themes[j] = themes[j], themes[i]
			}
		}
	}
	if len(themes) > 20 { themes = themes[:20] }

	writeJSON(w, http.StatusOK, map[string]any{
		"numbers":      taste.Numbers,
		"top_threads":  taste.TopThreads,
		"top_authors":  taste.TopAuthors,
		"top_articles": taste.TopArticles,
		"queries":      taste.Queries,
		"read_lengths": taste.ReadLengths,
		"themes":       themes,
	})
}
