# GraveStack — Development Rules

## Workflow rules (read first)

- **When told to merge or push to main, do it immediately.** Do not defer to the
  user, do not say "that's up to you", do not ask for confirmation. An explicit
  instruction to merge IS the confirmation. Create the PR, mark it ready, merge
  it — all in one go.
- **Don't create draft PRs when the intent is to merge.** If the user says to
  push to main, the PR is a means to an end, not a review gate. Create it as
  ready and merge it right away.

GraveStack is a PWA that fights the "save-but-never-read" habit. Instead of a
graveyard list (paradox of choice → paralysis → back to the fresh feed), it
surfaces saved Substack articles through an **intelligent, curated surface** and
renders the **full text in-app** so you never bounce to Substack's feed. Built
for an ADHD brain: the fix is design, not discipline.

## Design principles (evolved from v1)

1. **The constraint is no endless scroll, not "one article."** v1 tried "one
   article, take it or leave it" — it killed the pull to open the app. The v2
   direction: a curated home surface with a featured article (80-90% of screen,
   showing first lines so you feel like you're already reading), 2-3 smaller
   suggestions with context, a daily write-up, and an AI query field. Not a
   feed, not a restriction — a curated magazine of your own collection.
2. **The notification is the product.** It carries the pitch (hook + one honest
   line + one real pull quote), not a bland reminder. See `buildDailyPayload`.
3. **Read in-app, never deep-link to Substack.** Full sanitized `body_html` is
   rendered in the reader. Never add a "read on Substack" link to the main flow.
4. **Context everywhere.** Every article, thread, and recommendation should have
   a reason — why this article matters in the collection, why it's in this
   thread, why it's recommended today. The context layer is the product.
5. **No arbitrary choice.** Choices are curated (a featured pick + 2-3
   alternatives with reasons), not open-ended (never a browseable grid or
   infinite scroll).

## Current state (as of 2026-07-12)

### Deployed and working
- **App live at** `https://gravestack.fly.dev` (Fly.io, Singapore, shared-cpu-1x,
  1GB persistent volume `gravedata` at `/data`)
- **APP_PASSWORD:** set as Fly secret
- **Fly secrets:** `APP_PASSWORD`, `CRON_TOKEN`, `ANTHROPIC_API_KEY` all set
- **138 articles synced** from Substack saved list. 134 have full text, 4 missing
  (3 empty subdomain from first sync, 1 paywalled 403)
- **Substack saved-list URL:** `https://substack.com/api/v1/reader/saved`
  (cursor-based pagination, 12 items/page). `defaultSavedListURL` in
  `substack.go` is correct as of July 2026.
- **Sync handles:** pagination (all pages), throttling (500ms between fetches),
  retry with exponential backoff on 429, body backfill for metadata-only
  articles, custom domain resolution via `publication.subdomain`

### Built but not yet deployed
- **Graph system** (`graph.go`): DB tables for threads, article_threads,
  article_relations, article_meta. Processing pipeline that sends articles to
  Claude in batches for thematic analysis, thread creation, and relationship
  finding. Endpoints: `POST /api/build-graph`, `GET /api/threads`,
  `GET /api/thread/{slug}`, `GET /api/article/{id}/related`, `POST /api/ask`
- **Needs refactoring before first run:** `buildGraph` currently rebuilds from
  scratch — must be made incremental (see "Next steps" below)

### Not yet built
- New home page UI (featured tile + suggestions + write-up + AI query)
- Threads view (thematic clusters with context)
- Magazine view (variable tiles, Pinterest-style, interest-driven sizing)
- Post-read experience (related articles, annotations)
- Auto-sync on timer
- Thread emergence logic (smart detection of new themes)

## Next steps (in order)

1. **Refactor graph build to be incremental** — only process unanalyzed
   articles. Add `thread_fit` score on `article_threads`. Add emergence logic:
   when unthreaded articles reach threshold (~8-10), run a single API call to
   check if a new theme has formed. Never rebuild from scratch unless explicitly
   asked.
2. **Make sync lazy** — no AI calls on sync, only on first view. Pitches and
   graph placement happen when the user opens the app, not on sync.
3. **Run first full graph build** — one-time ~$0.50-1.00 to analyze all 134
   articles, create threads, find relationships.
4. **Build new home page UI** — see "Home page design" below.
5. **Build threads and magazine views.**
6. **Wire up feedback loop** — events → tile sizing → sort order.

## Home page design (single scrollable view)

### Level 1: Featured article (80-90% of screen)
- Cover image (present but not dominant)
- Title + pitch/context
- First 1-2 lines of actual article text visible — feels like already reading
- Tapping opens full reader

### Level 2: 2-3 alternative suggestions
- Smaller tiles with context for each
- Mix: one comfort author, one stretch, one thematic connection

### Level 3: Daily write-up
- Curated narrative connecting the day's recommendations
- Catches users who skipped levels 1 and 2

### Level 4: AI query field
- "What do you want to explore?"
- Returns article-backed write-up — the response itself teaches, articles are
  further exploration
- e.g. "Aristotle's ethics" → write-up about values in the collection +
  recommended articles that cover similar ideas

## Graph / intelligence layer

### DB tables
- `threads` — named thematic groupings with title, description, icon
- `article_threads` — many-to-many with `relevance` score and `context` per link
- `article_relations` — pairwise connections (deepens/challenges/complements/
  applies/echoes) with `strength` and `reason`
- `article_meta` — per-article themes, context blurb, read time, difficulty

### Cost model
- Full rebuild: ~$0.50-1.00 (one-time)
- 15 new articles incremental: ~$0.25
- Thread emergence scan: ~$0.03 (fires every 2-3 months)
- Each "ask" query: ~$0.01-0.03
- Daily use: ~$0.30-0.50/month total

### Thread emergence (not yet built)
When unthreaded/weakly-placed articles accumulate past a threshold, run ONE API
call with those articles + existing thread titles. Three outcomes: better
placement into existing threads, one new thread created, or an existing thread
splits. Never rebuilds the full graph.

## Collection analysis (138 articles, July 2026)

### Thematic clusters
1. **Self-understanding (~35)** — identity, authenticity, introspection
2. **Connection and intimacy (~20)** — friendship, conversations, loneliness
3. **Philosophy and big ideas (~20)** — Nietzsche, Frankl, existentialism
4. **Writing and thinking (~18)** — essay craft, articulation, language
5. **Internet and attention (~15)** — doomscrolling, algorithms, phones
6. **Art, culture and taste (~12)** — visual art, cinema, aesthetics
7. **Productivity and action (~10)** — ADHD, habits, self-improvement
8. **Marketing and work (~8)** — SEO, content strategy, brand

### Top authors
Big Think (10), Sam Kriss (8), Philosopheasy (6), maja (4), Erifili Gounari (4)

### Key insight
The throughline is self-knowledge — not self-help. Philosophy, psychology,
relationships, and writing all orbit "who are you really?" Tension between
short comfort reads (maja, Erifili) and long aspirational reads (Sam Kriss).

## Sync & the undocumented saved endpoint

- `defaultSavedListURL` = `https://substack.com/api/v1/reader/saved` (correct
  as of July 2026, cursor-based pagination with `nextCursor`).
- `fetchAllPages` follows cursors with a safety cap of 50 pages.
- `walkPosts` extracts posts from the `{items: [{post: {...}, publication:
  {...}}]}` wrapper and carries `publication.subdomain` into `rawPost.PubSubdomain`.
- Auth is the user's session cookie (`connect.sid`), stored server-side.
- `substackGet` retries up to 3 times on 429 with exponential backoff (2s→4s→8s).
- Sync throttles 500ms between body fetches to avoid rate limiting.
- Re-sync backfills bodies for metadata-only articles (checks `body_html` is
  non-empty before skipping).

## Security / rendering

- **Always sanitize** Substack `body_html` before storing/rendering (`htmlPolicy`,
  bluemonday). Never inject raw remote HTML into the DOM.
- The whole app is gated by `APP_PASSWORD` because the Substack cookie lives
  server-side.

## Pitch layer

- The LLM's job is NOT to invent hype. `pitch.go` extends the title's promise
  with concrete specifics + **one verbatim pull quote** (`quoteIsVerbatim`
  enforces it; local real-sentence fallback guarantees a genuine sample).
- Model: `claude-sonnet-4-6` (via `ANTHROPIC_MODEL`).
- The Anthropic key is entered in the UI (browser localStorage) or set as
  `ANTHROPIC_API_KEY` env var (currently set as Fly secret).

## App password

`APP_PASSWORD` is optional. Unset ⇒ the gate is open. Currently set as a Fly secret.

## Events (v2 fuel)

Log `opened / read_started / scrolled / abandoned / completed / not_today /
reroll` from day one. Behavioral re-ranking feeds the magazine view tile sizing
and sort order.

## Stack

- Go single binary, `//go:embed frontend`, stdlib `net/http` + 1.22 pattern mux.
- `modernc.org/sqlite` (pure Go, no CGO) at `$DATA_DIR/gravestack.db` — **must
  be on a persistent volume**.
- Web push: `SherClockHolmes/webpush-go`; VAPID keys auto-generated and persisted.
- Host-agnostic (Dockerfile). Daily push fires from internal scheduler or
  `.github/workflows/daily-push.yml`.
