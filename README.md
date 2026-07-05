# GraveStack

> The saved folder is where good articles go to die. GraveStack digs one up for you every day.

A PWA that fights the Substack **save-but-never-read** habit. Instead of a graveyard list
(paradox of choice → paralysis → back to the fresh feed), GraveStack surfaces **one saved
article a day**, pitches it with a real pull quote, and renders the **full text inside the
app** so you never bounce to Substack's feed. Built for an ADHD brain: the fix is design,
not discipline.

## How it works

1. **Sync** your Substack saved list once (paste your session cookie, or paste the saved
   list JSON). Articles + full text are stored locally.
2. Each day the app picks **one** article and generates an honest pitch: the original
   title (the hook that already worked) + one sentence on what it actually is + one
   verbatim pull quote from the piece.
3. A **daily push notification** carries that pitch (big-picture style with the cover
   image on Android). Tapping it opens straight into the article.
4. The home screen **is** the article — cover + pitch, scroll down and you're already
   reading. No menu, no list.
5. Miss? One tap: **"Not today."** It's logged, buried for two weeks, and a fresh pick
   comes tomorrow. No same-day reroll — that's the slot-machine loop we're escaping.

Every interaction (opened / read / abandoned / completed / not-today) is logged for a v2
self-improving recommender.

## Quick start (local)

```bash
go build -o gravestack .
DATA_DIR=./data ANTHROPIC_API_KEY=sk-ant-... ./gravestack
# open http://localhost:8080
```

Then open **⚙ Settings** → paste your cookie → **Sync now** → set a notify time.

## Getting your Substack session cookie

1. Log into Substack in a desktop browser.
2. DevTools (F12) → **Application** → **Cookies** → `https://substack.com`.
3. Copy the value of **`connect.sid`** (or `substack.sid`). Paste it into Settings.
   You can paste the bare value or a full `name=value; name2=value2` string.

The cookie stays valid for months. It's stored server-side and only used for Substack
requests.

### If sync finds nothing (the endpoint drifted)

Substack's saved-list endpoint is undocumented and occasionally changes. Two fixes:

- **Override the URL:** open your Substack **Saved** page, DevTools → **Network**, find the
  request that returns your saved posts, copy its URL into Settings → *"override the
  saved-list URL."*
- **Paste the JSON directly:** copy that request's JSON **response** and paste it into
  Settings → *"Paste saved-list JSON."* This path never touches the live API, so it always
  works.

Full article text is fetched per-post from each publication's
`/api/v1/posts/{slug}` — full when your cookie has access, a preview otherwise.

## Configuration (env vars)

| Var | Default | Purpose |
|-----|---------|---------|
| `APP_PASSWORD` | *(none)* | Gate the app. **Set this in production** — your cookie lives server-side. |
| `ANTHROPIC_API_KEY` | *(none)* | Enables LLM pitches. Without it, pitches fall back to the subtitle. |
| `ANTHROPIC_MODEL` | `claude-sonnet-4-6` | Pitch model. |
| `DATA_DIR` | `./data` | SQLite location. **Point at a persistent volume in production.** |
| `PORT` / `ADDR` | `8080` / `:8080` | Listen port. |
| `CRON_TOKEN` | *(none)* | Protects `/internal/cron/daily` for the external-cron fallback. |
| `VAPID_SUBJECT` | `mailto:admin@…` | Contact for push services. |

VAPID keys are generated and persisted automatically on first run.

## Deploy (free-friendly)

**Recommended — Fly.io** (always-on machine + free persistent volume → reliable daily
push and a durable event log):

```bash
fly launch --no-deploy
fly volumes create gravedata --size 1
fly secrets set APP_PASSWORD=... ANTHROPIC_API_KEY=... CRON_TOKEN=...
fly deploy
```

See `fly.toml` (keep `min_machines_running = 1` so the scheduler runs).

**Any other host / a sleep-prone free tier:** deploy the Docker image anywhere, then let
GitHub Actions fire the daily push: set repo secrets `GRAVESTACK_URL` and
`GRAVESTACK_CRON_TOKEN`, and `.github/workflows/daily-push.yml` will POST the
token-protected cron endpoint hourly (the app enforces your real notify time and sends at
most once per day).

## Installing the PWA & notifications

On Android/Chrome: open the site → **Add to Home screen**. The installed app gets its own
entry in Android's notification settings (its own channel), so you can keep notifications
off everywhere else and on for just this. Enable them in Settings → *"Enable notifications
on this device."* (iOS 16.4+ works when installed to the home screen, without the
big-picture cover image.)

## Development

```bash
go test ./...   # unit tests: defensive JSON parse, verbatim-quote check, pick/reroll logic
go vet ./...
```

Architecture and design rules live in `CLAUDE.md`.

## Roadmap (v2, deferred)

- Behavioral re-ranking from the logged events (the self-improving "brain").
- Skip-decay / resurfacing tuning.
- Automatic weekly re-sync.
- Native wrap (Expo) only if PWA push disappoints.
