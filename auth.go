package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Auth is a minimal single-user gate. The app stores a Substack session cookie
// server-side, so the whole surface must be protected even though there's only
// one user. Sessions are stateless HMAC tokens signed with a per-install secret.
type Auth struct {
	password string
	secret   []byte
}

const sessionCookie = "gs_session"
const sessionTTL = 90 * 24 * time.Hour

func newAuth(db *sql.DB, password string) *Auth {
	secret := getSetting(db, "session_secret")
	if secret == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		secret = hex.EncodeToString(b)
		_ = setSetting(db, "session_secret", secret)
	}
	raw, _ := hex.DecodeString(secret)
	return &Auth{password: password, secret: raw}
}

// enabled reports whether a password gate is configured. With no APP_PASSWORD
// (e.g. local dev) the gate is open.
func (a *Auth) enabled() bool { return a.password != "" }

func (a *Auth) sign(expiry int64) string {
	msg := strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(msg))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return msg + "." + sig
}

func (a *Auth) valid(token string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	want := a.sign(expiry)
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

func (a *Auth) issue(w http.ResponseWriter) {
	expiry := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.sign(expiry),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(expiry, 0),
	})
}

func (a *Auth) authed(r *http.Request) bool {
	if !a.enabled() {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return a.valid(c.Value)
}

// checkPassword is constant-time.
func (a *Auth) checkPassword(pw string) bool {
	return subtle.ConstantTimeCompare([]byte(pw), []byte(a.password)) == 1
}

// require wraps a handler so unauthenticated API calls get 401.
func (a *Auth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
