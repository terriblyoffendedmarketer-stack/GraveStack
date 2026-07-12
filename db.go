package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// schema is applied on every boot; every statement is idempotent so it doubles
// as the migration path for a fresh SQLite file on a new volume.
const schema = `
CREATE TABLE IF NOT EXISTS articles (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	substack_id    TEXT UNIQUE NOT NULL,
	url            TEXT NOT NULL,
	subdomain      TEXT,
	slug           TEXT,
	title          TEXT NOT NULL,
	subtitle       TEXT,
	author         TEXT,
	published_at   TEXT,
	word_count     INTEGER DEFAULT 0,
	cover_image_url TEXT,
	body_html      TEXT,
	plain_text     TEXT,
	topic          TEXT,
	is_paywalled   INTEGER DEFAULT 0,
	saved_rank     INTEGER DEFAULT 0,
	synced_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pitches (
	article_id   INTEGER PRIMARY KEY REFERENCES articles(id) ON DELETE CASCADE,
	pitch_line   TEXT,
	pull_quote   TEXT,
	model        TEXT,
	generated_at TEXT
);

CREATE TABLE IF NOT EXISTS picks (
	date         TEXT PRIMARY KEY,
	article_id   INTEGER REFERENCES articles(id),
	rerolls_used INTEGER DEFAULT 0,
	dismissed    INTEGER DEFAULT 0,
	created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	article_id INTEGER REFERENCES articles(id),
	type       TEXT NOT NULL,
	scroll_pct INTEGER DEFAULT 0,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS buried (
	article_id INTEGER PRIMARY KEY REFERENCES articles(id) ON DELETE CASCADE,
	until      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS push_subscriptions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	endpoint   TEXT UNIQUE NOT NULL,
	p256dh     TEXT NOT NULL,
	auth       TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT
);

CREATE TABLE IF NOT EXISTS threads (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	slug        TEXT UNIQUE NOT NULL,
	title       TEXT NOT NULL,
	description TEXT,
	icon        TEXT,
	color       TEXT,
	sort_order  INTEGER DEFAULT 0,
	created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS article_threads (
	article_id INTEGER REFERENCES articles(id) ON DELETE CASCADE,
	thread_id  INTEGER REFERENCES threads(id) ON DELETE CASCADE,
	relevance  REAL DEFAULT 1.0,
	context    TEXT,
	PRIMARY KEY (article_id, thread_id)
);

CREATE TABLE IF NOT EXISTS article_relations (
	article_a  INTEGER REFERENCES articles(id) ON DELETE CASCADE,
	article_b  INTEGER REFERENCES articles(id) ON DELETE CASCADE,
	relation   TEXT NOT NULL,
	strength   REAL DEFAULT 1.0,
	reason     TEXT,
	PRIMARY KEY (article_a, article_b)
);

CREATE TABLE IF NOT EXISTS article_meta (
	article_id  INTEGER PRIMARY KEY REFERENCES articles(id) ON DELETE CASCADE,
	themes      TEXT,
	context     TEXT,
	read_time   INTEGER DEFAULT 0,
	difficulty  TEXT,
	analyzed_at TEXT
);

CREATE TABLE IF NOT EXISTS highlights (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	article_id INTEGER REFERENCES articles(id) ON DELETE CASCADE,
	text       TEXT NOT NULL,
	note       TEXT DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS issues (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	title          TEXT NOT NULL,
	query          TEXT NOT NULL,
	query_norm     TEXT NOT NULL,
	writeup        TEXT NOT NULL,
	main_pick      INTEGER REFERENCES articles(id),
	supporting     TEXT,
	article_count  INTEGER DEFAULT 0,
	created_at     TEXT NOT NULL
);
`

// Article is the stored representation of one saved Substack post.
type Article struct {
	ID          int64  `json:"id"`
	SubstackID  string `json:"substack_id"`
	URL         string `json:"url"`
	Subdomain   string `json:"subdomain"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Subtitle    string `json:"subtitle"`
	Author      string `json:"author"`
	PublishedAt string `json:"published_at"`
	WordCount   int    `json:"word_count"`
	CoverImage  string `json:"cover_image_url"`
	BodyHTML    string `json:"body_html"`
	PlainText   string `json:"-"`
	Topic       string `json:"topic"`
	IsPaywalled bool   `json:"is_paywalled"`
	SavedRank   int    `json:"saved_rank"`
	SyncedAt    string `json:"synced_at"`
	PitchLine   string `json:"pitch_line"`
	PullQuote   string `json:"pull_quote"`
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// modernc/sqlite is a single writer; one connection avoids "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return db, nil
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// getSetting returns "" when the key is absent.
func getSetting(db *sql.DB, key string) string {
	var v string
	_ = db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

func setSetting(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// scanArticle reads a joined articles+pitches row.
func scanArticle(s interface{ Scan(...any) error }) (*Article, error) {
	var a Article
	var pitchLine, pullQuote sql.NullString
	err := s.Scan(
		&a.ID, &a.SubstackID, &a.URL, &a.Subdomain, &a.Slug, &a.Title, &a.Subtitle,
		&a.Author, &a.PublishedAt, &a.WordCount, &a.CoverImage, &a.BodyHTML,
		&a.Topic, &a.IsPaywalled, &a.SavedRank, &a.SyncedAt, &pitchLine, &pullQuote,
	)
	if err != nil {
		return nil, err
	}
	a.PitchLine = pitchLine.String
	a.PullQuote = pullQuote.String
	return &a, nil
}

const articleSelectCols = `
	a.id, a.substack_id, a.url, a.subdomain, a.slug, a.title, a.subtitle,
	a.author, a.published_at, a.word_count, a.cover_image_url, a.body_html,
	a.topic, a.is_paywalled, a.saved_rank, a.synced_at,
	p.pitch_line, p.pull_quote`

func getArticle(db *sql.DB, id int64) (*Article, error) {
	row := db.QueryRow(`SELECT `+articleSelectCols+`
		FROM articles a LEFT JOIN pitches p ON p.article_id = a.id
		WHERE a.id = ?`, id)
	return scanArticle(row)
}

// buryArticle suppresses an article from daily picks until the given time.
// A zero/very-far until (permanent) is used for completed reads.
func buryArticle(db *sql.DB, articleID int64, until time.Time) error {
	_, err := db.Exec(
		`INSERT INTO buried(article_id, until) VALUES(?, ?)
		 ON CONFLICT(article_id) DO UPDATE SET until = excluded.until`,
		articleID, until.UTC().Format(time.RFC3339))
	return err
}

func logEvent(db *sql.DB, articleID int64, typ string, scrollPct int) error {
	_, err := db.Exec(
		`INSERT INTO events(article_id, type, scroll_pct, created_at) VALUES(?, ?, ?, ?)`,
		articleID, typ, scrollPct, nowUTC())
	return err
}
