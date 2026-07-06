package main

import (
	"os"
	"path/filepath"
	"strconv"
)

// REROLLS_PER_DAY is the design switch for the "open problem" (see CLAUDE.md).
// 0 (default): a miss = one frictionless "Not today", fresh pick tomorrow, no
// same-day second pull. 1: the brief's "one reroll/day, hard stop" behavior.
// Kept as a constant on purpose — flipping it needs no other change.
const REROLLS_PER_DAY = 0

// buryNotTodayDays is how long a "not today" article stays out of rotation.
const buryNotTodayDays = 14

type Config struct {
	Addr           string // listen address, e.g. ":8080"
	DataDir        string // where gravestack.db lives
	AppPassword    string // gate for the whole app (single user)
	AnthropicKey   string
	AnthropicModel string
	CronToken      string // protects /internal/cron/daily for external schedulers
	VAPIDSubject   string // "mailto:" contact for push services
}

func loadConfig() Config {
	c := Config{
		Addr:           envOr("ADDR", ":"+envOr("PORT", "8080")),
		DataDir:        envOr("DATA_DIR", "./data"),
		AppPassword:    os.Getenv("APP_PASSWORD"),
		AnthropicKey:   os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel: envOr("ANTHROPIC_MODEL", "claude-sonnet-4-6"),
		CronToken:      os.Getenv("CRON_TOKEN"),
		VAPIDSubject:   envOr("VAPID_SUBJECT", "mailto:admin@gravestack.local"),
	}
	return c
}

func (c Config) dbPath() string { return filepath.Join(c.DataDir, "gravestack.db") }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
