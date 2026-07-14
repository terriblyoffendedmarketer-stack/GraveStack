package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// vapidKeys loads the install's VAPID keypair, generating and persisting one on
// first run so push works out of the box on any host (keys live on the volume).
func vapidKeys(db *sql.DB) (pub, priv string, err error) {
	pub = getSetting(db, "vapid_public")
	priv = getSetting(db, "vapid_private")
	if pub != "" && priv != "" {
		return pub, priv, nil
	}
	priv, pub, err = webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", err
	}
	if err := setSetting(db, "vapid_public", pub); err != nil {
		return "", "", err
	}
	if err := setSetting(db, "vapid_private", priv); err != nil {
		return "", "", err
	}
	return pub, priv, nil
}

// pushPayload is the big-picture notification body the service worker renders.
type pushPayload struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	Image     string `json:"image,omitempty"` // big-picture cover
	Icon      string `json:"icon,omitempty"`
	Badge     string `json:"badge,omitempty"`
	ArticleID int64  `json:"article_id"`
	URL       string `json:"url"`
}

func saveSubscription(db *sql.DB, sub *webpush.Subscription) error {
	_, err := db.Exec(`INSERT INTO push_subscriptions(endpoint, p256dh, auth, created_at)
		VALUES(?,?,?,?)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth`,
		sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth, nowUTC())
	return err
}

func allSubscriptions(db *sql.DB) ([]*webpush.Subscription, error) {
	rows, err := db.Query(`SELECT endpoint, p256dh, auth FROM push_subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []*webpush.Subscription
	for rows.Next() {
		s := &webpush.Subscription{}
		if err := rows.Scan(&s.Endpoint, &s.Keys.P256dh, &s.Keys.Auth); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// sendPush delivers one payload to all subscriptions. Dead subscriptions
// (404/410) are pruned so the table doesn't rot.
func sendPush(db *sql.DB, cfg Config, payload pushPayload) error {
	pub, priv, err := vapidKeys(db)
	if err != nil {
		return err
	}
	subs, err := allSubscriptions(db)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(payload)
	for _, sub := range subs {
		resp, err := webpush.SendNotification(body, sub, &webpush.Options{
			Subscriber:      cfg.VAPIDSubject,
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             86400,
			Urgency:         webpush.UrgencyNormal,
		})
		if err != nil {
			log.Printf("push send error: %v", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			_, _ = db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, sub.Endpoint)
		}
	}
	return nil
}

// buildDailyPayload materializes today's pick and builds its notification.
func buildDailyPayload(db *sql.DB, cfg Config) (pushPayload, error) {
	p, err := todaysPick(db)
	if err != nil {
		return pushPayload{}, err
	}
	a, err := getArticle(db, p.ArticleID)
	if err != nil {
		return pushPayload{}, err
	}
	ensurePitch(db, cfg, a)
	body := a.PitchLine
	if a.PullQuote != "" {
		body = a.PitchLine + "\n\n" + "“" + trimQuote(a.PullQuote) + "”"
	}
	return pushPayload{
		Title:     a.Title, // the original hook that already worked
		Body:      body,
		Image:     a.CoverImage,
		Icon:      "/icons/icon-192.png",
		Badge:     "/icons/badge.png",
		ArticleID: a.ID,
		URL:       fmt.Sprintf("/?article=%d", a.ID),
	}, nil
}

func trimQuote(q string) string {
	const max = 180
	if len(q) <= max {
		return q
	}
	return q[:max] + "…"
}
