// Package auth is the one admin's login: a PBKDF2 hash in the config, a signed session cookie.
package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	iterations = 600000
	cookieName = "switchyard"
	lifetime   = 30 * 24 * time.Hour
)

func Hash(password string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iterations, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

func Check(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err1 := strconv.Atoi(parts[1])
	salt, err2 := hex.DecodeString(parts[2])
	want, err3 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(key, want) == 1
}

func NewSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func sign(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// SetSession logs the user in. No Path: the browser takes the page's directory, so the cookie
// stays inside a reverse proxy's prefix.
func SetSession(w http.ResponseWriter, secret, user string) {
	msg := base64.RawURLEncoding.EncodeToString([]byte(user)) + "." + strconv.FormatInt(time.Now().Add(lifetime).Unix(), 10)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: msg + "." + sign(secret, msg),
		MaxAge: int(lifetime.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// Valid: the request carries an unexpired session of this user.
func Valid(r *http.Request, secret, user string) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(c.Value, '.')
	if i < 0 {
		return false
	}
	msg, sig := c.Value[:i], c.Value[i+1:]
	if !hmac.Equal([]byte(sig), []byte(sign(secret, msg))) {
		return false
	}
	u, exp, ok := strings.Cut(msg, ".")
	name, err := base64.RawURLEncoding.DecodeString(u)
	t, err2 := strconv.ParseInt(exp, 10, 64)
	return ok && err == nil && err2 == nil && string(name) == user && time.Now().Unix() < t
}
