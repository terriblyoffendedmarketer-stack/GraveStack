package main

import (
	"database/sql"
	"embed"
	"encoding/json"
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

	// Home + reading loop (all gated)
	mux.HandleFunc("GET /api/home", s.auth.require(s.handleHome))
	mux.HandleFunc("POST /api/home/enrich", s.auth.require(s.handleHomeEnrich))
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
	mux.HandleFunc("GET /api/article/{id}/highlights", s.auth.require(s.handleListHighlights))
	mux.HandleFunc("POST /api/article/{id}/highlights", s.auth.require(s.handleSaveHighlight))
	mux.HandleFunc("DELETE /api/highlight/{id}", s.auth.require(s.handleDeleteHighlight))
	mux.HandleFunc("POST /api/ask", s.auth.require(s.handleAskWithIssues))
	mux.HandleFunc("GET /api/magazine", s.auth.require(s.handleMagazine))
	mux.HandleFunc("GET /api/issues", s.auth.require(s.handleListIssues))
	mux.HandleFunc("GET /api/issue/{id}", s.auth.require(s.handleGetIssue))
	mux.HandleFunc("DELETE /api/issue/{id}", s.auth.require(s.handleDeleteIssue))
	mux.HandleFunc("POST /api/issue/{id}/merge/{other}", s.auth.require(s.handleMergeIssues))
	mux.HandleFunc("GET /api/taste", s.auth.require(s.handleTaste))
	mux.HandleFunc("GET /api/article/{id}/notes", s.auth.require(s.handleListNotes))
	mux.HandleFunc("POST /api/article/{id}/notes", s.auth.require(s.handleSaveNote))
	mux.HandleFunc("GET /api/history", s.auth.require(s.handleHistory))

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

// ---- home ----

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgForRequest(r)
	home, err := buildHome(s.db, cfg)
	if err != nil {
		if err == errNoArticles {
			writeJSON(w, http.StatusOK, map[string]any{"empty": true})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, home)
}

func (s *server) handleHomeEnrich(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgForRequest(r)
	if cfg.AnthropicKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"pitches": map[string]any{}, "writeup": ""})
		return
	}

	var req struct {
		ArticleIDs []int64 `json:"article_ids"`
		Writeup    bool    `json:"writeup"`
		Featured   int64   `json:"featured"`
		Suggestion []int64 `json:"suggestions"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	pitches := map[int64]map[string]string{}
	for _, id := range req.ArticleIDs {
		a, err := getArticle(s.db, id)
		if err != nil {
			continue
		}
		ensurePitch(s.db, cfg, a)
		pitches[id] = map[string]string{
			"pitch_line": a.PitchLine,
			"pull_quote": a.PullQuote,
		}
	}

	var writeup string
	if req.Writeup && req.Featured > 0 {
		fa, _ := getArticle(s.db, req.Featured)
		if fa != nil {
			featured := &homeArticle{Article: fa}
			var suggestions []*homeArticle
			for _, sid := range req.Suggestion {
				sa, _ := getArticle(s.db, sid)
				if sa != nil {
					suggestions = append(suggestions, &homeArticle{Article: sa})
				}
			}
			if len(suggestions) > 0 {
				writeup = generateWriteup(cfg, featured, suggestions)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"pitches": pitches, "writeup": writeup})
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

	// Collect article IDs and contexts first, then close the cursor
	// before querying individual articles (single-connection SQLite).
	artRows, err := s.db.Query(`SELECT at.article_id, at.context
		FROM article_threads at WHERE at.thread_id = ? ORDER BY at.relevance DESC`, t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type artRef struct {
		id  int64
		ctx string
	}
	var refs []artRef
	for artRows.Next() {
		var aid int64
		var ctx sql.NullString
		if err := artRows.Scan(&aid, &ctx); err != nil {
			continue
		}
		refs = append(refs, artRef{aid, ctx.String})
	}
	artRows.Close()

	type threadArticle struct {
		Article       *Article `json:"article"`
		ThreadContext string   `json:"thread_context"`
	}
	var articles []threadArticle
	for _, ref := range refs {
		a, err := getArticle(s.db, ref.id)
		if err != nil {
			continue
		}
		articles = append(articles, threadArticle{Article: a, ThreadContext: ref.ctx})
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": t, "articles": articles})
}

func (s *server) handleRelated(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	relRows, err := s.db.Query(`
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
	type relRef struct {
		rel, reason string
		strength    float64
		relID       int64
	}
	var relRefs []relRef
	for relRows.Next() {
		var r relRef
		if err := relRows.Scan(&r.rel, &r.strength, &r.reason, &r.relID); err != nil {
			continue
		}
		relRefs = append(relRefs, r)
	}
	relRows.Close()

	type related struct {
		Article  *Article `json:"article"`
		Relation string   `json:"relation"`
		Strength float64  `json:"strength"`
		Reason   string   `json:"reason"`
	}
	var results []related
	for _, r := range relRefs {
		a, err := getArticle(s.db, r.relID)
		if err != nil {
			continue
		}
		results = append(results, related{Article: a, Relation: r.rel, Strength: r.strength, Reason: r.reason})
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *server) handleListHighlights(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows, err := s.db.Query(`SELECT id, text, note, created_at FROM highlights WHERE article_id = ? ORDER BY id`, id)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	defer rows.Close()
	type highlight struct {
		ID        int64  `json:"id"`
		Text      string `json:"text"`
		Note      string `json:"note"`
		CreatedAt string `json:"created_at"`
	}
	var hl []highlight
	for rows.Next() {
		var h highlight
		rows.Scan(&h.ID, &h.Text, &h.Note, &h.CreatedAt)
		hl = append(hl, h)
	}
	if hl == nil {
		hl = []highlight{}
	}
	writeJSON(w, http.StatusOK, hl)
}

func (s *server) handleSaveHighlight(w http.ResponseWriter, r *http.Request) {
	articleID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Text string `json:"text"`
		Note string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil || body.Text == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	res, err := s.db.Exec(`INSERT INTO highlights(article_id, text, note, created_at) VALUES(?,?,?,?)`,
		articleID, body.Text, body.Note, nowUTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (s *server) handleDeleteHighlight(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.db.Exec(`DELETE FROM highlights WHERE id = ?`, id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleListNotes(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows, err := s.db.Query(`SELECT id, text, created_at FROM notes WHERE article_id = ? ORDER BY id DESC`, id)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	defer rows.Close()
	type note struct {
		ID        int64  `json:"id"`
		Text      string `json:"text"`
		CreatedAt string `json:"created_at"`
	}
	var notes []note
	for rows.Next() {
		var n note
		rows.Scan(&n.ID, &n.Text, &n.CreatedAt)
		notes = append(notes, n)
	}
	if notes == nil {
		notes = []note{}
	}
	writeJSON(w, http.StatusOK, notes)
}

func (s *server) handleSaveNote(w http.ResponseWriter, r *http.Request) {
	articleID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &body); err != nil || body.Text == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	res, err := s.db.Exec(`INSERT INTO notes(article_id, text, created_at) VALUES(?,?,?)`,
		articleID, body.Text, nowUTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`
		SELECT DISTINCT a.id, a.title, a.author, a.cover_image_url, e.created_at
		FROM events e
		JOIN articles a ON a.id = e.article_id
		WHERE e.type IN ('read_started', 'completed')
		GROUP BY a.id
		ORDER BY MAX(e.created_at) DESC
		LIMIT 50`)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	defer rows.Close()
	type historyItem struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		Author   string `json:"author"`
		CoverURL string `json:"cover_image_url"`
		LastRead string `json:"last_read"`
	}
	var items []historyItem
	for rows.Next() {
		var h historyItem
		rows.Scan(&h.ID, &h.Title, &h.Author, &h.CoverURL, &h.LastRead)
		items = append(items, h)
	}
	if items == nil {
		items = []historyItem{}
	}
	writeJSON(w, http.StatusOK, items)
}

// handleAsk is now replaced by handleAskWithIssues in issues.go.
