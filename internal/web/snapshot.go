package web

import (
	"encoding/json"
	"sync"
	"time"
)

// snapshot holds the latest view as JSON. It is refreshed in the background every refreshEvery,
// but only while someone polls: idleAfter after the last poll, switchyard stops calling the
// cluster altogether. All viewers share the same snapshot, so ten open tabs cost what one does.
// After a quiet spell a poll gets the last view at once and the loop reads the new one; only the
// very first poll waits for it.
type snapshot struct {
	read         func() any
	refreshEvery time.Duration
	idleAfter    time.Duration

	mu       sync.Mutex
	v        []byte
	lastPoll time.Time
	wake     chan struct{}
}

func newSnapshot(read func() any, refreshEvery, idleAfter time.Duration) *snapshot {
	return &snapshot{read: read, refreshEvery: refreshEvery, idleAfter: idleAfter, wake: make(chan struct{}, 1)}
}

// get returns the latest view and wakes the background loop.
func (s *snapshot) get() []byte {
	s.mu.Lock()
	s.lastPoll = time.Now()
	v := s.v
	s.mu.Unlock()
	if v == nil {
		v = s.refresh()
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return v
}

func (s *snapshot) refresh() []byte {
	b, _ := json.Marshal(s.read())
	s.mu.Lock()
	s.v = b
	s.mu.Unlock()
	return b
}

func (s *snapshot) watched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastPoll) < s.idleAfter
}

func (s *snapshot) loop() {
	for {
		if !s.watched() {
			<-s.wake
			continue
		}
		s.refresh()
		time.Sleep(s.refreshEvery)
	}
}
