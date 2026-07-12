package main

import (
	"database/sql"
	"net/http"
	"sort"
	"time"
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
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle"`
	Author     string `json:"author"`
	WordCount  int    `json:"word_count"`
	CoverImage string `json:"cover_image_url"`
	PitchLine  string `json:"pitch_line"`
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
			) comp ON comp.article_id = a.id`
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
			) comp ON comp.article_id = a.id`
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

	layoutMagazine(items)
	return items, nil
}

// layoutMagazine assigns tile sizes using a rhythm-based approach rather
// than pure score thresholds. This guarantees visual variety regardless
// of how clustered the scores are.
func layoutMagazine(items []magazineItem) {
	if len(items) == 0 {
		return
	}

	// Intelligent default scoring: even without event data, rank articles
	// using signals from the collection analysis and user preferences.
	for i := range items {
		// Visual richness — articles with covers look better in large tiles.
		if items[i].Article.CoverImage != "" {
			items[i].Score += 3
		}
		// Context richness — articles with AI-generated context have more
		// to show in premium positions.
		if items[i].Context != "" {
			items[i].Score += 2
		}
		// Thread membership — threaded articles have more context.
		if items[i].Thread != "" {
			items[i].Score += 1
		}
		// Read time sweet spot: 5-12 min reads are most likely to be started
		// and finished (ADHD-friendly). Very long reads get a small penalty,
		// very short ones get a small boost.
		rt := items[i].ReadTime
		if rt == 0 && items[i].Article.WordCount > 0 {
			rt = items[i].Article.WordCount / 220
		}
		switch {
		case rt >= 5 && rt <= 12:
			items[i].Score += 2
		case rt > 0 && rt < 5:
			items[i].Score += 1
		case rt > 20:
			items[i].Score -= 1
		}
		// Subtitle/pitch presence — more content to display.
		if items[i].Article.PitchLine != "" {
			items[i].Score += 1
		}
		// Completed articles get penalized so unread content surfaces.
		if items[i].Completed {
			items[i].Score -= 3
		}
	}

	// Daily rotation: use day-of-year as a seed to shift article ordering
	// so tiles don't become stale even without new reading data.
	dayOfYear := time.Now().UTC().YearDay()
	rotation := dayOfYear % max(1, len(items))

	// Sort by score descending — this determines which articles get
	// the premium (hero/large) layout positions.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Score > items[j].Score
	})

	// Rotate the tail so different articles surface on different days.
	// Keep the top 3 stable (they earned their spot via engagement).
	if len(items) > 5 {
		rest := items[3:]
		rot := rotation % max(1, len(rest))
		rotated := make([]magazineItem, len(rest))
		copy(rotated, rest[rot:])
		copy(rotated[len(rest)-rot:], rest[:rot])
		copy(items[3:], rotated)
	}

	// Assign sizes using a fixed rhythm pattern that repeats.
	// Pattern: hero, small, medium, small, large, small, small, medium,
	//          small, medium, large, small, small, medium, small, hero, ...
	// This creates visual punctuation: a big tile every ~8 items,
	// a large tile every ~5, mediums filling gaps, smalls as texture.
	rhythm := []string{
		"hero", "small", "medium", "small", "large", "small", "small", "medium",
		"small", "medium", "large", "small", "small", "medium", "small", "hero",
	}

	for i := range items {
		items[i].TileSize = rhythm[i%len(rhythm)]
	}

	// Completed articles get demoted one tier — they're still visible
	// but shouldn't dominate the visual space.
	for i := range items {
		if items[i].Completed {
			switch items[i].TileSize {
			case "hero":
				items[i].TileSize = "large"
			case "large":
				items[i].TileSize = "medium"
			}
		}
	}

	// Verify hero tiles have enough content to look good — if an article
	// has no cover AND no context, demote it from hero to large.
	for i := range items {
		if items[i].TileSize == "hero" {
			if items[i].Article.CoverImage == "" && items[i].Context == "" {
				items[i].TileSize = "large"
			}
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
