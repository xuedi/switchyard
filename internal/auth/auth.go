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
	Idle       = 12 * time.Hour     // a session ends this long after its last request
	Absolute   = 7 * 24 * time.Hour // and this long after the login at the latest
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

// Same compares two strings in constant time, whatever their lengths.
func Same(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
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

// Session is what the cookie carries, signed. Generation is the config's: bumping it there ends
// every session at once.
type Session struct {
	User       string
	Generation int
	Start      time.Time // the login
	Seen       time.Time // the last request, kept to the minute or so
}

// Live: the session is the user's, of the current generation, and has not run out.
func (s Session) Live(user string, generation int, now time.Time) bool {
	return Same(s.User, user) && s.Generation == generation &&
		now.Before(s.Start.Add(Absolute)) && now.Before(s.Seen.Add(Idle))
}

// SetSession writes the cookie. No Path: the browser takes the page's directory, so the cookie
// stays inside a reverse proxy's prefix. Only a request in switchyard's top directory may set
// it: one below (backup/state) would leave a second cookie for that directory, which the browser
// sends first there.
func SetSession(w http.ResponseWriter, secret string, s Session, secure bool) {
	msg := strings.Join([]string{base64.RawURLEncoding.EncodeToString([]byte(s.User)), strconv.Itoa(s.Generation),
		strconv.FormatInt(s.Start.Unix(), 10), strconv.FormatInt(s.Seen.Unix(), 10)}, ".")
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: msg + "." + sign(secret, msg),
		MaxAge: int(time.Until(s.Start.Add(Absolute)).Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

func ClearSession(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

// ReadSessions returns the request's sessions whose signature holds, and how many cookies of
// switchyard's it carried (more than one: one is left from a directory below, by 0.12.0 or older).
func ReadSessions(r *http.Request, secret string) ([]Session, int) {
	var list []Session
	cookies := r.CookiesNamed(cookieName)
	for _, c := range cookies {
		if s, ok := readSession(c.Value, secret); ok {
			list = append(list, s)
		}
	}
	return list, len(cookies)
}

func readSession(value, secret string) (Session, bool) {
	i := strings.LastIndexByte(value, '.')
	if i < 0 {
		return Session{}, false
	}
	msg, sig := value[:i], value[i+1:]
	if !hmac.Equal([]byte(sig), []byte(sign(secret, msg))) {
		return Session{}, false
	}
	f := strings.Split(msg, ".")
	if len(f) != 4 {
		return Session{}, false
	}
	user, err1 := base64.RawURLEncoding.DecodeString(f[0])
	gen, err2 := strconv.Atoi(f[1])
	start, err3 := strconv.ParseInt(f[2], 10, 64)
	seen, err4 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return Session{}, false
	}
	return Session{User: string(user), Generation: gen, Start: time.Unix(start, 0), Seen: time.Unix(seen, 0)}, true
}
