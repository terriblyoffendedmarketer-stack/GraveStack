# GraveStack — Development Rules

GraveStack is a PWA that fights the "save-but-never-read" habit: it surfaces **one**
saved Substack article per day, pitches it persuasively, and renders the full text
**in-app** so the Substack fresh feed never gets a chance to eat the reader. Design is
the fix, not discipline (the user has ADHD).

## Non-negotiable design principles

1. **Never show the full list.** One article at a time. The only browse view is a
   deliberately boring, category-sorted library buried in Settings — reaching it takes
   effort on purpose (`handleLibrary`). Do not make it delightful; a nice browse view is
   a beautifully designed relapse.
2. **The notification is the product.** It carries the pitch (hook + one honest line +
   one real pull quote), not a bland reminder. See `buildDailyPayload`.
3. **Read in-app, never deep-link to Substack.** Full sanitized `body_html` is rendered
   in the reader. Never add a "read on Substack" link to the main flow.
4. **One article, not a choice between articles.** The failure mode is choosing between
   articles, not choosing whether to read.

## The reroll switch (the "open problem")

`REROLLS_PER_DAY` in `config.go` is the single knob:
- **0 (default, current):** a miss gets one frictionless **"Not today"** → the day is
  dismissed and a fresh pick comes *tomorrow*. No same-day second pull — even one reroll
  trains the slot-machine loop, and the *anticipation* of the second pull is the trap.
- **1:** the brief's "one reroll/day, hard stop." Flip the constant; nothing else changes.

Do not add unlimited rerolls, a "generate more" button, or a shuffling tile grid. Known
scarcity is the off-switch for the variable-reward loop.

## Sync & the undocumented saved endpoint

- Substack has no official saved-list API. `defaultSavedListURL` in `substack.go` is a
  **best guess** — it may drift. The URL is overridable in Settings (`saved_list_url`),
  and there is a guaranteed **paste-JSON fallback**. The saved-list JSON is parsed
  **defensively** (`walkPosts` descends arbitrary structures looking for post-shaped
  objects) so a schema change rarely breaks sync. Keep it defensive; don't hard-code a
  rigid struct for the list response.
- Auth is the user's session cookie (`connect.sid`), stored server-side and forwarded
  verbatim. Full post text is fetched per-post from
  `https://{subdomain}.substack.com/api/v1/posts/{slug}` → `body_html`.
- Sync is **manual** (a Settings button). Re-syncs dedupe on `articles.substack_id`
  UNIQUE, so only genuinely new saves surface.

## Security / rendering

- **Always sanitize** Substack `body_html` before storing/rendering (`htmlPolicy`,
  bluemonday). Never inject raw remote HTML into the DOM.
- The whole app is gated by `APP_PASSWORD` because the Substack cookie lives server-side.

## Pitch layer

- The LLM's job is NOT to invent hype. `pitch.go` extends the title's promise with
  concrete specifics + **one verbatim pull quote** (`quoteIsVerbatim` enforces it; a
  local real-sentence fallback guarantees a genuine sample if the model paraphrases).
- Model: `claude-sonnet-4-6` (via `ANTHROPIC_MODEL`). Missing key degrades gracefully to
  the subtitle — pitches are never a hard dependency for reading.
- The Anthropic key is normally entered in the UI and kept in the browser's localStorage,
  sent per-request as `X-Anthropic-Key` and applied via `cfgForRequest`. **Never persist
  it server-side.** The `ANTHROPIC_API_KEY` env var is only an optional fallback. Pitches
  are generated (browser present) and stored in the DB, so the server-side daily push can
  read them without a key.

## App password

`APP_PASSWORD` is optional. Unset ⇒ the gate is open (`auth.enabled()` false). Recommended
on public deployments because the Substack cookie lives server-side. Keep the gate logic
optional — do not make the app hard-require a password.

## Events (v2 fuel)

Log `opened / read_started / scrolled / abandoned / completed / not_today / reroll` from
day one. Behavioral re-ranking is v2 — but never stop logging.

## Stack

- Go single binary, `//go:embed frontend`, stdlib `net/http` + 1.22 pattern mux.
- `modernc.org/sqlite` (pure Go, no CGO) at `$DATA_DIR/gravestack.db` — **must be on a
  persistent volume**, or the backlog/events reset on redeploy.
- Web push: `SherClockHolmes/webpush-go`; VAPID keys auto-generated and persisted.
- Host-agnostic (Dockerfile). Daily push fires from the internal scheduler on always-on
  hosts, or from `.github/workflows/daily-push.yml` hitting `/internal/cron/daily` on
  sleep-prone hosts.
