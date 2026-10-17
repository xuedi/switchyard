package auth

import (
	"sync"
	"time"
)

const (
	maxFails = 10
	window   = 15 * time.Minute // for the failures, and the lock that follows them
)

// Limiter slows password guessing: 10 failures of one client in 15 minutes lock it out for 15
// minutes, and one check runs at a time with a second's pause after a failure, so guesses spread
// over many clients stay at about one a second. In memory: a restart forgets it.
type Limiter struct {
	check  sync.Mutex
	mu     sync.Mutex
	fails  map[string][]time.Time
	locked map[string]time.Time
}

// Try runs the password check for the client; locked is true when it was not even tried.
func (l *Limiter) Try(client string, check func() bool) (ok, locked bool) {
	now := time.Now()
	l.mu.Lock()
	if l.fails == nil {
		l.fails, l.locked = map[string][]time.Time{}, map[string]time.Time{}
	}
	for c, t := range l.locked {
		if now.Sub(t) > window {
			delete(l.locked, c)
		}
	}
	_, locked = l.locked[client]
	l.mu.Unlock()
	if locked {
		return false, true
	}

	l.check.Lock()
	ok = check()
	if !ok {
		time.Sleep(time.Second)
	}
	l.check.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	if ok {
		delete(l.fails, client)
		return true, false
	}
	recent := []time.Time{now}
	for _, t := range l.fails[client] {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	l.fails[client] = recent
	if len(recent) >= maxFails {
		l.locked[client] = now
		delete(l.fails, client)
	}
	for c, ts := range l.fails {
		if now.Sub(ts[0]) > window {
			delete(l.fails, c)
		}
	}
	return false, false
}
