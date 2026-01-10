package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
)

type claudeStreamStarter func(ctx context.Context) (<-chan []byte, <-chan *interfaces.ErrorMessage)
type claudeStreamErrorEncoder func(errMsg *interfaces.ErrorMessage) []byte

const (
	claudeStreamReplayMaxBytes     = 8 << 20
	claudeStreamSubscriberBufSize  = 256
	claudeStreamOrphanCancelAfter  = 30 * time.Second
	claudeStreamCompletedCacheTTL  = 5 * time.Minute
	claudeStreamPruneIntervalFloor = 30 * time.Second
)

var globalClaudeStreamHub = newClaudeStreamHub()

func claudeStreamDedupeKey(authHeader, idempotencyKey string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(authHeader))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(idempotencyKey))
	return hex.EncodeToString(h.Sum(nil))
}

type claudeStreamHub struct {
	mu          sync.Mutex
	streams     map[string]*claudeStream
	lastPruneAt time.Time
}

func newClaudeStreamHub() *claudeStreamHub {
	return &claudeStreamHub{
		streams: make(map[string]*claudeStream),
	}
}

func (h *claudeStreamHub) getOrCreate(key string, starter claudeStreamStarter, encodeErr claudeStreamErrorEncoder) *claudeStream {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()

	h.pruneLocked(now)

	if s := h.streams[key]; s != nil {
		s.touch(now)
		return s
	}

	s := &claudeStream{
		key:         key,
		createdAt:   now,
		updatedAt:   now,
		subscribers: make(map[chan []byte]struct{}),
		doneCh:      make(chan struct{}),
	}
	h.streams[key] = s

	s.start(starter, encodeErr, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		// Best-effort prune on completion; keep cached streams until TTL.
		h.pruneLocked(time.Now())
	})

	return s
}

func (h *claudeStreamHub) pruneLocked(now time.Time) {
	if !h.lastPruneAt.IsZero() && now.Sub(h.lastPruneAt) < claudeStreamPruneIntervalFloor {
		return
	}
	h.lastPruneAt = now

	for key, s := range h.streams {
		if s == nil {
			delete(h.streams, key)
			continue
		}
		createdAt, doneAt, done := s.stateForPrune()
		if !done {
			// Cap runaway streams even if nobody retries with the same key.
			if now.Sub(createdAt) > claudeStreamCompletedCacheTTL*2 {
				s.cancelOrphaned()
			}
			continue
		}
		if !doneAt.IsZero() && now.Sub(doneAt) > claudeStreamCompletedCacheTTL {
			delete(h.streams, key)
		}
	}
}

type claudeStream struct {
	key string

	mu        sync.Mutex
	createdAt time.Time
	updatedAt time.Time
	doneAt    time.Time

	subscribers map[chan []byte]struct{}
	orphanTimer *time.Timer

	replayBytes int
	replay      [][]byte

	done   bool
	doneCh chan struct{}

	cancel context.CancelFunc
}

func (s *claudeStream) touch(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updatedAt = now
}

func (s *claudeStream) stateForPrune() (createdAt, doneAt time.Time, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createdAt, s.doneAt, s.done
}

func (s *claudeStream) cancelOrphaned() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *claudeStream) start(starter claudeStreamStarter, encodeErr claudeStreamErrorEncoder, onDone func()) {
	execCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	data, errs := starter(execCtx)

	go func() {
		defer func() {
			if onDone != nil {
				onDone()
			}
		}()

		for {
			select {
			case <-execCtx.Done():
				s.finish()
				return
			case chunk, ok := <-data:
				if !ok {
					// If an error is pending, prefer emitting it.
					select {
					case errMsg, ok := <-errs:
						if ok && errMsg != nil {
							if encodeErr != nil {
								s.broadcast(encodeErr(errMsg))
							}
						}
					default:
					}
					s.finish()
					return
				}
				s.broadcast(chunk)
			case errMsg, ok := <-errs:
				if !ok {
					continue
				}
				if errMsg != nil && encodeErr != nil {
					s.broadcast(encodeErr(errMsg))
				}
				s.finish()
				return
			}
		}
	}()
}

func (s *claudeStream) finish() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	s.doneAt = time.Now()
	close(s.doneCh)

	for ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, ch)
	}
	if s.orphanTimer != nil {
		s.orphanTimer.Stop()
		s.orphanTimer = nil
	}
	s.mu.Unlock()
}

func (s *claudeStream) subscribe() (replay [][]byte, sub <-chan []byte, unsubscribe func()) {
	ch := make(chan []byte, claudeStreamSubscriberBufSize)
	now := time.Now()

	s.mu.Lock()
	s.updatedAt = now

	if len(s.replay) > 0 {
		replay = append(replay, s.replay...)
	}

	if s.orphanTimer != nil {
		s.orphanTimer.Stop()
		s.orphanTimer = nil
	}

	if s.done {
		close(ch)
		sub = ch
		s.mu.Unlock()
		return replay, sub, func() {}
	}

	s.subscribers[ch] = struct{}{}
	sub = ch
	s.mu.Unlock()

	unsubscribe = func() {
		s.mu.Lock()
		if _, ok := s.subscribers[ch]; ok {
			delete(s.subscribers, ch)
			close(ch)
		}
		shouldCancel := !s.done && len(s.subscribers) == 0 && s.orphanTimer == nil
		if shouldCancel {
			s.orphanTimer = time.AfterFunc(claudeStreamOrphanCancelAfter, func() {
				s.cancelOrphaned()
			})
		}
		s.mu.Unlock()
	}

	return replay, sub, unsubscribe
}

func (s *claudeStream) broadcast(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	// Snapshot subscribers and decide on replay buffering under lock,
	// then broadcast outside to avoid holding the lock during writes.
	var subs []chan []byte

	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}

	if s.replayBytes < claudeStreamReplayMaxBytes {
		cloned := make([]byte, len(chunk))
		copy(cloned, chunk)
		if s.replayBytes+len(cloned) <= claudeStreamReplayMaxBytes {
			s.replay = append(s.replay, cloned)
			s.replayBytes += len(cloned)
		} else {
			// Stop buffering further once we hit the cap.
			s.replayBytes = claudeStreamReplayMaxBytes
		}
	}

	s.updatedAt = time.Now()
	for ch := range s.subscribers {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- chunk:
		default:
			// Subscriber can't keep up; drop it.
			s.mu.Lock()
			if _, ok := s.subscribers[ch]; ok {
				delete(s.subscribers, ch)
				close(ch)
			}
			s.mu.Unlock()
		}
	}
}
