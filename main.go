package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
)

//go:embed all:frontend
var frontendFS embed.FS

type server struct {
	db   *sql.DB
	cfg  Config
	auth *Auth
}

func main() {
	cfg := loadConfig()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	db, err := openDB(cfg.dbPath())
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Ensure a VAPID keypair exists so the /api/vapid-public-key endpoint works.
	if _, _, err := vapidKeys(db); err != nil {
		log.Printf("warning: could not init VAPID keys: %v", err)
	}

	s := &server{db: db, cfg: cfg, auth: newAuth(db, cfg.AppPassword)}
	startScheduler(db, cfg)

	mux := http.NewServeMux()
	s.routes(mux)

	log.Printf("GraveStack listening on %s (data=%s, model=%s, rerolls/day=%d)",
		cfg.Addr, cfg.DataDir, cfg.AnthropicModel, REROLLS_PER_DAY)
	if err := http.ListenAndServe(cfg.Addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) routes(mux *http.ServeMux) {
	// Auth
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/session", s.handleSession)

	// Core reading loop (all gated)
	mux.HandleFunc("GET /api/today", s.auth.require(s.handleToday))
	mux.HandleFunc("POST /api/reroll", s.auth.require(s.handleReroll))
	mux.HandleFunc("POST /api/not-today", s.auth.require(s.handleNotToday))
	mux.HandleFunc("POST /api/events", s.auth.require(s.handleEvents))

	// Sync + settings + library
	mux.HandleFunc("POST /api/sync", s.auth.require(s.handleSync))
	mux.HandleFunc("GET /api/settings", s.auth.require(s.handleGetSettings))
	mux.HandleFunc("POST /api/settings", s.auth.require(s.handleSaveSettings))
	mux.HandleFunc("GET /api/library", s.auth.require(s.handleLibrary))
	mux.HandleFunc("GET /api/article/{id}", s.auth.require(s.handleArticle))

	// Push
	mux.HandleFunc("GET /api/vapid-public-key", s.auth.require(s.handleVAPIDKey))
	mux.HandleFunc("POST /api/subscribe", s.auth.require(s.handleSubscribe))

	// Graph / intelligence
	mux.HandleFunc("GET /api/graph-test", s.auth.require(s.handleGraphTest))
	mux.HandleFunc("GET /api/graph-status", s.auth.require(s.handleGraphStatus))
	mux.HandleFunc("POST /api/build-graph", s.auth.require(s.handleBuildGraph))
	mux.HandleFunc("GET /api/threads", s.auth.require(s.handleThreads))
	mux.HandleFunc("GET /api/thread/{slug}", s.auth.require(s.handleThread))
	mux.HandleFunc("GET /api/article/{id}/related", s.auth.require(s.handleRelated))
	mux.HandleFunc("POST /api/ask", s.auth.require(s.handleAsk))

	// External cron trigger (token-protected, no session needed)
	mux.HandleFunc("POST /internal/cron/daily", s.handleCron)

	// Static PWA
	sub, _ := fs.Sub(frontendFS, "frontend")
	mux.Handle("/", http.FileServer(http.FS(sub)))
}

// ---- helpers ----

// cfgForRequest returns a per-request copy of the config with the Anthropic key
// overridden by the X-Anthropic-Key header when the browser supplies one (it is
// kept in the browser's localStorage, sent per-request, and never persisted
// server-side). Falls back to the ANTHROPIC_API_KEY env var when absent.
func (s *server) cfgForRequest(r *http.Request) Config {
	c := s.cfg
	if k := strings.TrimSpace(r.Header.Get("X-Anthropic-Key")); k != "" {
		c.AnthropicKey = k
	}
	return c
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---- auth handlers ----

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.auth.enabled() || s.auth.checkPassword(body.Password) {
		s.auth.issue(w)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	http.Error(w, "invalid password", http.StatusUnauthorized)
}

func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"authed":     s.auth.authed(r),
		"needsLogin": s.auth.enabled(),
	})
}

// ---- reading loop ----

func (s *server) handleToday(w http.ResponseWriter, r *http.Request) {
	p, err := todaysPick(s.db)
	if err != nil {
		if err == errNoArticles {
			writeJSON(w, http.StatusOK, map[string]any{"empty": true})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pr, _ := getPick(s.db, p.Date)
	if pr != nil && pr.Dismissed {
		writeJSON(w, http.StatusOK, map[string]any{"dismissed": true})
		return
	}
	a, err := getArticle(s.db, p.ArticleID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ensurePitch(s.db, s.cfgForRequest(r), a)
	writeJSON(w, http.StatusOK, map[string]any{
		"article":       a,
		"canReroll":     !p.Dismissed && p.RerollsUsed < REROLLS_PER_DAY,
		"rerollsPerDay": REROLLS_PER_DAY,
	})
}

func (s *server) handleReroll(w http.ResponseWriter, r *http.Request) {
	p, err := reroll(s.db)
	if err != nil {
		if err == errRerollNotAllowed {
			http.Error(w, "no reroll available", http.StatusForbidden)
			return
		}
		if err == errNoArticles {
			writeJSON(w, http.StatusOK, map[string]any{"empty": true})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a, err := getArticle(s.db, p.ArticleID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ensurePitch(s.db, s.cfgForRequest(r), a)
	writeJSON(w, http.StatusOK, map[string]any{
		"article":   a,
		"canReroll": p.RerollsUsed < REROLLS_PER_DAY,
	})
}

func (s *server) handleNotToday(w http.ResponseWriter, r *http.Request) {
	if err := notToday(s.db); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dismissed": true})
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ArticleID int64  `json:"article_id"`
		Type      string `json:"type"`
		ScrollPct int    `json:"scroll_pct"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Type == "completed" {
		_ = markCompleted(s.db, body.ArticleID, body.ScrollPct)
	} else {
		_ = logEvent(s.db, body.ArticleID, body.Type, body.ScrollPct)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- sync + settings ----

func (s *server) handleSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cookie    string `json:"cookie"`
		SavedJSON string `json:"savedJson"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cookie := body.Cookie
	if cookie == "" {
		cookie = getSetting(s.db, "cookie")
	} else {
		_ = setSetting(s.db, "cookie", cookie) // remember for future syncs
	}
	endpoint := getSetting(s.db, "saved_list_url")
	cfg := s.cfgForRequest(r)
	res, _, err := runSync(s.db, cfg, cookie, body.SavedJSON, endpoint)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "result": res})
		return
	}
	// Pitches and graph placement are lazy — generated on first view via
	// ensurePitch, not on sync. Keeps sync fast and avoids wasted AI calls
	// on articles the user may never open.
	writeJSON(w, http.StatusOK, res)
}

// settingsKeys are the user-editable settings surfaced to the UI. The Substack
// cookie is intentionally excluded from GET responses (write-only).
var settingsKeys = []string{"notify_time", "timezone", "saved_list_url"}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	for _, k := range settingsKeys {
		out[k] = getSetting(s.db, k)
	}
	out["hasCookie"] = getSetting(s.db, "cookie") != ""
	out["hasAnthropicKey"] = s.cfg.AnthropicKey != ""
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := readJSON(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, k := range settingsKeys {
		if v, ok := body[k]; ok {
			_ = setSetting(s.db, k, v)
		}
	}
	if v, ok := body["cookie"]; ok && v != "" {
		_ = setSetting(s.db, "cookie", v)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLibrary is the intentionally-boring escape hatch: a plain,
// category-sorted list of everything not yet read. Requires effort to reach (UI
// buries it in Settings) per design principle #5.
func (s *server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`
		SELECT a.title, a.author, a.topic, a.word_count, a.id
		FROM articles a
		WHERE a.id NOT IN (SELECT article_id FROM buried WHERE until > ?)
		ORDER BY COALESCE(NULLIF(a.topic,''), 'Uncategorized') ASC, a.title ASC`, nowUTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type item struct {
		Title     string `json:"title"`
		Author    string `json:"author"`
		Topic     string `json:"topic"`
		WordCount int    `json:"word_count"`
		ID        int64  `json:"id"`
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.Title, &it.Author, &it.Topic, &it.WordCount, &it.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, items)
}

// handleArticle serves a specific article (used by the library escape hatch).
// Opening it here still logs an "opened" event so v2 has the signal.
func (s *server) handleArticle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	a, err := getArticle(s.db, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ensurePitch(s.db, s.cfgForRequest(r), a)
	_ = logEvent(s.db, id, "opened", 0)
	writeJSON(w, http.StatusOK, map[string]any{"article": a})
}

// ---- push ----

func (s *server) handleVAPIDKey(w http.ResponseWriter, r *http.Request) {
	pub, _, err := vapidKeys(s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": pub})
}

func (s *server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var sub webpush.Subscription
	if err := readJSON(r, &sub); err != nil || sub.Endpoint == "" {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}
	if err := saveSubscription(s.db, &sub); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleCron(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if s.cfg.CronToken == "" || token != s.cfg.CronToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// force=1 ignores the notify-time window (manual/test trigger).
	if r.URL.Query().Get("force") == "1" {
		date := localDate(s.db)
		fireDaily(s.db, s.cfg, date)
	} else {
		maybeFireDaily(s.db, s.cfg)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// isTruthy is a tiny helper for form-ish string flags.
func isTruthy(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ---- graph / intelligence ----

func (s *server) handleGraphTest(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgForRequest(r)
	if cfg.AnthropicKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"error": "no API key"})
		return
	}
	result, err := callAnthropicRaw(cfg, "Reply with exactly: ok", "Test", 10)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error(), "model": cfg.AnthropicModel})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "response": result, "model": cfg.AnthropicModel})
}

func (s *server) handleGraphStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, graphStatus(s.db))
}

func (s *server) handleBuildGraph(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgForRequest(r)
	rebuild := isTruthy(r.URL.Query().Get("rebuild"))
	async := !isTruthy(r.URL.Query().Get("sync"))
	if async {
		go func() {
			if err := buildGraphOpts(s.db, cfg, rebuild); err != nil {
				log.Printf("build-graph: %v", err)
			}
		}()
		status := graphStatus(s.db)
		status["ok"] = true
		status["started"] = true
		if rebuild {
			status["mode"] = "full rebuild"
		} else {
			status["mode"] = "incremental"
		}
		writeJSON(w, http.StatusOK, status)
		return
	}
	if err := buildGraphOpts(s.db, cfg, rebuild); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	status := graphStatus(s.db)
	status["ok"] = true
	status["complete"] = true
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleThreads(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT t.id, t.slug, t.title, t.description, t.icon, t.color, t.sort_order,
		(SELECT COUNT(*) FROM article_threads WHERE thread_id = t.id) as article_count
		FROM threads t ORDER BY t.sort_order`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
	writeJSON(w, http.StatusOK, threads)
}

func (s *server) handleThread(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var t Thread
	err := s.db.QueryRow(`SELECT id, slug, title, description, icon, color, sort_order FROM threads WHERE slug = ?`, slug).
		Scan(&t.ID, &t.Slug, &t.Title, &t.Description, &t.Icon, &t.Color, &t.SortOrder)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	artRows, err := s.db.Query(`SELECT at.article_id, at.context
		FROM article_threads at WHERE at.thread_id = ? ORDER BY at.relevance DESC`, t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer artRows.Close()

	type threadArticle struct {
		Article       *Article `json:"article"`
		ThreadContext string   `json:"thread_context"`
	}
	var articles []threadArticle
	for artRows.Next() {
		var aid int64
		var ctx sql.NullString
		if err := artRows.Scan(&aid, &ctx); err != nil {
			continue
		}
		a, err := getArticle(s.db, aid)
		if err != nil {
			continue
		}
		articles = append(articles, threadArticle{Article: a, ThreadContext: ctx.String})
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": t, "articles": articles})
}

func (s *server) handleRelated(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	rows, err := s.db.Query(`
		SELECT ar.relation, ar.strength, ar.reason,
			CASE WHEN ar.article_a = ? THEN ar.article_b ELSE ar.article_a END as related_id
		FROM article_relations ar
		WHERE ar.article_a = ? OR ar.article_b = ?
		ORDER BY ar.strength DESC
		LIMIT 10`, id, id, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type related struct {
		Article  *Article `json:"article"`
		Relation string   `json:"relation"`
		Strength float64  `json:"strength"`
		Reason   string   `json:"reason"`
	}
	var results []related
	for rows.Next() {
		var rel, reason string
		var strength float64
		var relID int64
		if err := rows.Scan(&rel, &strength, &reason, &relID); err != nil {
			continue
		}
		a, err := getArticle(s.db, relID)
		if err != nil {
			continue
		}
		results = append(results, related{Article: a, Relation: rel, Strength: strength, Reason: reason})
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	if err := readJSON(r, &body); err != nil || strings.TrimSpace(body.Query) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg := s.cfgForRequest(r)

	// Load all articles with their metadata for context
	rows, err := s.db.Query(`SELECT a.id, a.title, a.author, a.subtitle, am.themes, am.context
		FROM articles a LEFT JOIN article_meta am ON am.article_id = a.id
		ORDER BY a.id`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var catalog strings.Builder
	for rows.Next() {
		var id int64
		var title, author, subtitle string
		var themes, ctx sql.NullString
		rows.Scan(&id, &title, &author, &subtitle, &themes, &ctx)
		context := ctx.String
		if context == "" {
			context = subtitle
		}
		fmt.Fprintf(&catalog, "ID:%d | %s (by %s) | themes: %s | %s\n",
			id, title, author, themes.String, context)
	}

	prompt := fmt.Sprintf("User's collection:\n%s\n\nUser asks: %s", catalog.String(), body.Query)

	result, err := callAnthropicRaw(cfg,
		`You are a thoughtful librarian for someone's personal article collection. They're asking about a topic — find the most relevant articles and write a short, engaging response.

Your response MUST be valid JSON with this structure:
{"writeup": "2-3 paragraphs exploring the user's question, weaving in references to specific articles by title. Teach them something from the articles, don't just list them. Make the writeup itself valuable to read.", "main_pick": <article_id>, "supporting": [<article_id>, ...], "reasoning": "one sentence on why you chose these"}

The writeup should feel like a knowledgeable friend saying "oh, you're interested in that? Here's what your own collection has to say about it." Reference articles naturally, by title and author, as part of the narrative.`,
		prompt, 2000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var response map[string]any
	if err := parseJSONFromResponse(result, &response); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"writeup": result, "main_pick": nil, "supporting": nil})
		return
	}
	writeJSON(w, http.StatusOK, response)
}
