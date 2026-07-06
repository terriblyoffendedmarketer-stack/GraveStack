package main

import (
	"database/sql"
	"errors"
	"math/rand"
	"time"
)

// errNoArticles means the backlog is empty (or everything is read/buried).
var errNoArticles = errors.New("no eligible articles")

// localDate returns today's date in the user's configured timezone (settings
// "timezone", default UTC). The pick and the notification must agree on "today".
func localDate(db *sql.DB) string {
	return time.Now().In(userLocation(db)).Format("2006-01-02")
}

func userLocation(db *sql.DB) *time.Location {
	tz := getSetting(db, "timezone")
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// eligibleArticleIDs returns unread, unburied articles, ordered so that newer
// saves rank first (revealed preference). Burial expiry is respected.
func eligibleArticleIDs(db *sql.DB) ([]int64, []int, error) {
	rows, err := db.Query(`
		SELECT a.id, a.saved_rank
		FROM articles a
		WHERE a.id NOT IN (
			SELECT article_id FROM buried WHERE until > ?
		)
		ORDER BY a.saved_rank ASC`, nowUTC())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	var ranks []int
	for rows.Next() {
		var id int64
		var rank int
		if err := rows.Scan(&id, &rank); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		ranks = append(ranks, rank)
	}
	return ids, ranks, rows.Err()
}

// choosePick selects one article with a weighted random that favors more
// recently saved pieces, while still giving older backlog a real chance (so the
// graveyard actually gets consumed). exclude is the current pick on a reroll.
func choosePick(db *sql.DB, exclude int64) (int64, error) {
	ids, _, err := eligibleArticleIDs(db)
	if err != nil {
		return 0, err
	}
	filtered := ids[:0:0]
	for _, id := range ids {
		if id != exclude {
			filtered = append(filtered, id)
		}
	}
	if len(filtered) == 0 {
		return 0, errNoArticles
	}
	// Weight: earliest in the list (newest save) gets the highest weight, decaying
	// linearly but never to zero, so the whole backlog stays reachable.
	n := len(filtered)
	weights := make([]int, n)
	total := 0
	for i := range filtered {
		w := n - i + 2 // +2 keeps the tail non-trivial
		weights[i] = w
		total += w
	}
	r := rand.Intn(total)
	for i, w := range weights {
		if r < w {
			return filtered[i], nil
		}
		r -= w
	}
	return filtered[n-1], nil
}

// pickRow is today's pick record.
type pickRow struct {
	Date        string
	ArticleID   int64
	RerollsUsed int
	Dismissed   bool
}

func getPick(db *sql.DB, date string) (*pickRow, error) {
	var p pickRow
	var dismissed int
	err := db.QueryRow(`SELECT date, article_id, rerolls_used, dismissed FROM picks WHERE date = ?`, date).
		Scan(&p.Date, &p.ArticleID, &p.RerollsUsed, &dismissed)
	if err != nil {
		return nil, err
	}
	p.Dismissed = dismissed == 1
	return &p, nil
}

// todaysPick returns (creating if needed) the pick row for today. It does NOT
// resurrect a dismissed day — a dismissed pick stays dismissed until tomorrow.
func todaysPick(db *sql.DB) (*pickRow, error) {
	date := localDate(db)
	if p, err := getPick(db, date); err == nil {
		return p, nil
	}
	id, err := choosePick(db, 0)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`INSERT INTO picks(date, article_id, rerolls_used, dismissed, created_at)
		VALUES(?, ?, 0, 0, ?)`, date, id, nowUTC())
	if err != nil {
		return nil, err
	}
	_ = logEvent(db, id, "opened", 0)
	return &pickRow{Date: date, ArticleID: id, RerollsUsed: 0}, nil
}

// reroll replaces today's article, if the daily allowance permits. With
// REROLLS_PER_DAY == 0 this always returns errRerollNotAllowed.
var errRerollNotAllowed = errors.New("reroll not allowed today")

func reroll(db *sql.DB) (*pickRow, error) {
	date := localDate(db)
	p, err := getPick(db, date)
	if err != nil {
		return nil, err
	}
	if p.Dismissed || p.RerollsUsed >= REROLLS_PER_DAY {
		return nil, errRerollNotAllowed
	}
	// Bury the rejected article and log the signal.
	_ = buryArticle(db, p.ArticleID, time.Now().Add(buryNotTodayDays*24*time.Hour))
	_ = logEvent(db, p.ArticleID, "reroll", 0)
	next, err := choosePick(db, p.ArticleID)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`UPDATE picks SET article_id = ?, rerolls_used = rerolls_used + 1 WHERE date = ?`,
		next, date)
	if err != nil {
		return nil, err
	}
	_ = logEvent(db, next, "opened", 0)
	return &pickRow{Date: date, ArticleID: next, RerollsUsed: p.RerollsUsed + 1}, nil
}

// notToday dismisses today's slot: logs the signal, buries the article, and
// marks the day done. No same-day replacement — a fresh pick comes tomorrow.
func notToday(db *sql.DB) error {
	date := localDate(db)
	p, err := getPick(db, date)
	if err != nil {
		return err
	}
	_ = buryArticle(db, p.ArticleID, time.Now().Add(buryNotTodayDays*24*time.Hour))
	_ = logEvent(db, p.ArticleID, "not_today", 0)
	_, err = db.Exec(`UPDATE picks SET dismissed = 1 WHERE date = ?`, date)
	return err
}

// markCompleted permanently buries a fully-read article so it never resurfaces.
func markCompleted(db *sql.DB, articleID int64, scrollPct int) error {
	_ = logEvent(db, articleID, "completed", scrollPct)
	return buryArticle(db, articleID, time.Now().AddDate(100, 0, 0))
}
