package main

import (
	"database/sql"
	"math"
	"net/http"
)

type magazineItem struct {
	Article    *magazineArticle `json:"article"`
	Thread     string           `json:"thread"`
	ThreadSlug string           `json:"thread_slug"`
	Context    string           `json:"context"`
	ReadTime   int              `json:"read_time"`
	Difficulty string           `json:"difficulty"`
	Score      float64          `json:"score"`
	TileSize   string           `json:"tile_size"`
	Completed  bool             `json:"completed"`
}

type magazineArticle struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Subtitle  string `json:"subtitle"`
	Author    string `json:"author"`
	WordCount int    `json:"word_count"`
	CoverImage string `json:"cover_image_url"`
	PitchLine string `json:"pitch_line"`
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
	var query string
	var args []any

	if threadFilter != "" {
		query = `SELECT a.id, a.title, a.subtitle, a.author, a.word_count, a.cover_image_url,
				COALESCE(p.pitch_line, ''),
				COALESCE(t.title, ''), COALESCE(t.slug, ''),
				COALESCE(m.context, ''), COALESCE(m.read_time, 0), COALESCE(m.difficulty, ''),
				COALESCE(eng.score, 0),
				CASE WHEN comp.article_id IS NOT NULL THEN 1 ELSE 0 END
			FROM articles a
			LEFT JOIN pitches p ON p.article_id = a.id
			INNER JOIN article_threads at2 ON at2.article_id = a.id
			INNER JOIN threads tf ON tf.id = at2.thread_id AND tf.slug = ?
			LEFT JOIN (
				SELECT at3.article_id, t2.title, t2.slug,
					ROW_NUMBER() OVER (PARTITION BY at3.article_id ORDER BY at3.relevance DESC) as rn
				FROM article_threads at3
				JOIN threads t2 ON t2.id = at3.thread_id
			) t ON t.article_id = a.id AND t.rn = 1
			LEFT JOIN article_meta m ON m.article_id = a.id
			LEFT JOIN (
				SELECT article_id,
					SUM(CASE type WHEN 'opened' THEN 1.0 WHEN 'read_started' THEN 2.0
						WHEN 'completed' THEN 5.0
						WHEN 'abandoned' THEN scroll_pct / 50.0
						ELSE scroll_pct / 100.0 END) as score
				FROM events GROUP BY article_id
			) eng ON eng.article_id = a.id
			LEFT JOIN (
				SELECT DISTINCT article_id FROM events WHERE type = 'completed'
			) comp ON comp.article_id = a.id
			ORDER BY COALESCE(eng.score, 0) DESC, a.published_at DESC`
		args = []any{threadFilter}
	} else {
		query = `SELECT a.id, a.title, a.subtitle, a.author, a.word_count, a.cover_image_url,
				COALESCE(p.pitch_line, ''),
				COALESCE(t.title, ''), COALESCE(t.slug, ''),
				COALESCE(m.context, ''), COALESCE(m.read_time, 0), COALESCE(m.difficulty, ''),
				COALESCE(eng.score, 0),
				CASE WHEN comp.article_id IS NOT NULL THEN 1 ELSE 0 END
			FROM articles a
			LEFT JOIN pitches p ON p.article_id = a.id
			LEFT JOIN (
				SELECT at3.article_id, t2.title, t2.slug,
					ROW_NUMBER() OVER (PARTITION BY at3.article_id ORDER BY at3.relevance DESC) as rn
				FROM article_threads at3
				JOIN threads t2 ON t2.id = at3.thread_id
			) t ON t.article_id = a.id AND t.rn = 1
			LEFT JOIN article_meta m ON m.article_id = a.id
			LEFT JOIN (
				SELECT article_id,
					SUM(CASE type WHEN 'opened' THEN 1.0 WHEN 'read_started' THEN 2.0
						WHEN 'completed' THEN 5.0
						WHEN 'abandoned' THEN scroll_pct / 50.0
						ELSE scroll_pct / 100.0 END) as score
				FROM events GROUP BY article_id
			) eng ON eng.article_id = a.id
			LEFT JOIN (
				SELECT DISTINCT article_id FROM events WHERE type = 'completed'
			) comp ON comp.article_id = a.id
			ORDER BY COALESCE(eng.score, 0) DESC, a.published_at DESC`
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []magazineItem
	for rows.Next() {
		var art magazineArticle
		var it magazineItem
		var completed int
		err := rows.Scan(
			&art.ID, &art.Title, &art.Subtitle, &art.Author, &art.WordCount, &art.CoverImage,
			&art.PitchLine,
			&it.Thread, &it.ThreadSlug,
			&it.Context, &it.ReadTime, &it.Difficulty,
			&it.Score, &completed,
		)
		if err != nil {
			continue
		}
		it.Article = &art
		it.Completed = completed == 1
		if it.Context == "" && art.PitchLine != "" {
			it.Context = art.PitchLine
		}
		items = append(items, it)
	}

	assignTileSizes(items)
	return items, nil
}

func assignTileSizes(items []magazineItem) {
	if len(items) == 0 {
		return
	}

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

	var maxScore float64
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
}
