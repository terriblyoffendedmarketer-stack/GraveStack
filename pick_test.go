package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedArticle(t *testing.T, db *sql.DB, id, title string, rank int) int64 {
	t.Helper()
	a := &Article{
		SubstackID: id, URL: "https://x.substack.com/p/" + id, Subdomain: "x", Slug: id,
		Title: title, PlainText: "Some body text.", SavedRank: rank,
	}
	newID, _, err := insertArticle(db, a)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return newID
}

func TestTodaysPickIsStable(t *testing.T) {
	db := testDB(t)
	seedArticle(t, db, "a1", "One", 0)
	seedArticle(t, db, "a2", "Two", 1)

	p1, err := todaysPick(db)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	p2, err := todaysPick(db)
	if err != nil {
		t.Fatalf("pick2: %v", err)
	}
	if p1.ArticleID != p2.ArticleID {
		t.Errorf("today's pick should be stable: %d vs %d", p1.ArticleID, p2.ArticleID)
	}
}

func TestNotTodayDismissesDay(t *testing.T) {
	db := testDB(t)
	seedArticle(t, db, "a1", "One", 0)
	seedArticle(t, db, "a2", "Two", 1)

	p, _ := todaysPick(db)
	if err := notToday(db); err != nil {
		t.Fatalf("notToday: %v", err)
	}
	// The article is buried...
	var buried int
	db.QueryRow(`SELECT COUNT(*) FROM buried WHERE article_id = ?`, p.ArticleID).Scan(&buried)
	if buried != 1 {
		t.Errorf("expected article buried after not-today")
	}
	// ...and the day is dismissed, not replaced.
	pr, _ := getPick(db, localDate(db))
	if !pr.Dismissed {
		t.Errorf("day should be dismissed")
	}
	// A not_today event was logged.
	var ev int
	db.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'not_today'`).Scan(&ev)
	if ev != 1 {
		t.Errorf("expected not_today event")
	}
}

func TestRerollDisabledByDefault(t *testing.T) {
	// REROLLS_PER_DAY defaults to 0, so reroll must be refused.
	if REROLLS_PER_DAY != 0 {
		t.Skip("test assumes default reroll allowance of 0")
	}
	db := testDB(t)
	seedArticle(t, db, "a1", "One", 0)
	seedArticle(t, db, "a2", "Two", 1)
	todaysPick(db)
	if _, err := reroll(db); err != errRerollNotAllowed {
		t.Errorf("expected errRerollNotAllowed, got %v", err)
	}
}

func TestCompletedBuriesPermanently(t *testing.T) {
	db := testDB(t)
	id := seedArticle(t, db, "a1", "One", 0)
	seedArticle(t, db, "a2", "Two", 1)
	if err := markCompleted(db, id, 95); err != nil {
		t.Fatalf("markCompleted: %v", err)
	}
	ids, _, _ := eligibleArticleIDs(db)
	for _, e := range ids {
		if e == id {
			t.Errorf("completed article should not be eligible")
		}
	}
}

func TestEmptyBacklog(t *testing.T) {
	db := testDB(t)
	if _, err := todaysPick(db); err != errNoArticles {
		t.Errorf("expected errNoArticles, got %v", err)
	}
}
