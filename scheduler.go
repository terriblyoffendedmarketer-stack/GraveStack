package main

import (
	"database/sql"
	"log"
	"time"
)

// startScheduler runs a per-minute ticker that fires the daily push once, when
// the user's local clock reaches their configured notify time. It is safe on
// always-on hosts; sleep-prone hosts should instead drive /internal/cron/daily
// from an external scheduler (see .github/workflows/daily-push.yml).
func startScheduler(db *sql.DB, cfg Config) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			maybeFireDaily(db, cfg)
			maybeAutoSync(db, cfg)
		}
	}()
}

// maybeFireDaily sends the push if (a) a notify time is set, (b) local time is
// at or just past it, and (c) we haven't already sent today. Idempotent within a
// day via the "last_push_date" setting, so both the ticker and the external cron
// can call it without double-sending.
func maybeFireDaily(db *sql.DB, cfg Config) {
	notify := getSetting(db, "notify_time") // "HH:MM"
	if notify == "" {
		return
	}
	loc := userLocation(db)
	now := time.Now().In(loc)
	today := now.Format("2006-01-02")
	if getSetting(db, "last_push_date") == today {
		return
	}
	target, err := time.ParseInLocation("2006-01-02 15:04", today+" "+notify, loc)
	if err != nil {
		return
	}
	// Fire when we're within a 10-minute window after the target, so a missed
	// tick (or a late external cron) still delivers rather than skipping the day.
	if now.Before(target) || now.After(target.Add(10*time.Minute)) {
		return
	}
	fireDaily(db, cfg, today)
}

const autoSyncInterval = 8 * time.Hour

func maybeAutoSync(db *sql.DB, cfg Config) {
	cookie := getSetting(db, "cookie")
	if cookie == "" {
		return
	}
	lastSync := getSetting(db, "last_auto_sync")
	if lastSync != "" {
		t, err := time.Parse(time.RFC3339, lastSync)
		if err == nil && time.Since(t) < autoSyncInterval {
			return
		}
	}
	endpoint := getSetting(db, "saved_list_url")
	res, _, err := runSync(db, cfg, cookie, "", endpoint)
	if err != nil {
		log.Printf("auto-sync: %v", err)
		return
	}
	_ = setSetting(db, "last_auto_sync", time.Now().UTC().Format(time.RFC3339))
	log.Printf("auto-sync: %d new, %d skipped", res.New, res.Skipped)

	// If new articles arrived and we have an API key, run incremental graph build
	// to analyze them, place in threads, and check for thread emergence.
	if res.New > 0 && cfg.AnthropicKey != "" {
		log.Printf("auto-sync: running incremental graph build for %d new articles", res.New)
		if err := buildGraph(db, cfg); err != nil {
			log.Printf("auto-sync: graph build failed: %v", err)
		}
	}
}

func fireDaily(db *sql.DB, cfg Config, today string) {
	payload, err := buildDailyPayload(db, cfg)
	if err != nil {
		log.Printf("daily push: build payload: %v", err)
		return
	}
	if err := sendPush(db, cfg, payload); err != nil {
		log.Printf("daily push: send: %v", err)
		return
	}
	_ = setSetting(db, "last_push_date", today)
	log.Printf("daily push sent for %s: %q", today, payload.Title)
}
