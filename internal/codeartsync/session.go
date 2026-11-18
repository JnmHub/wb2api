package codeartsync

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/codearts"
)

// ErrSessionsFull means the account already holds as many upstream chat
// sessions as it may: the caller should pick another account instead of
// sending a request the upstream would reject with TM.00001041.
var ErrSessionsFull = errors.New("codearts: account session slots exhausted")

// Sessions manages the upstream chat-session leases of the Huawei channel.
//
// The upstream allows three concurrent conversations per account; slots are
// busy/idle leases rather than request lifetimes, so every chat request must
// register busy, renew every 30 s and send idle when it ends. The gate here is
// local and keyed by account uid (never by access key: that changes on every
// renewal and would reset the count).
type Sessions struct {
	Pool *pool.Pool
	// Base overrides the snap-access root (tests).
	Base string
	// Interval is the busy renewal period (0 = codearts.HeartbeatInterval).
	Interval time.Duration
	// Limit overrides the per-account slot count (0 = the channel ceiling).
	Limit func(*accounts.Record) int
	Log   *slog.Logger

	mu     sync.Mutex
	active map[string]int
}

func (s *Sessions) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Sessions) base(rec *accounts.Record) string {
	if s.Base != "" {
		return s.Base
	}
	return realm.Get(rec.Realm).ChatUpstream
}

func (s *Sessions) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return codearts.HeartbeatInterval
}

func (s *Sessions) limit(rec *accounts.Record) int {
	if s.Limit != nil {
		if n := s.Limit(rec); n > 0 {
			return n
		}
	}
	if n := realm.CodeArtsInflight(); n > 0 {
		return n
	}
	return 3
}

// Session is one held upstream conversation slot.
type Session struct {
	manager *Sessions
	acc     *pool.Account
	id      string
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// ID is the user-session-id the chat request must carry.
func (s *Session) ID() string { return s.id }

// Begin reserves a slot and registers it busy. ErrSessionsFull is returned
// without touching the upstream when the account is already at its limit.
func (s *Sessions) Begin(ctx context.Context, acc *pool.Account) (*Session, error) {
	if acc == nil {
		return nil, errors.New("codearts session: account required")
	}
	rec := acc.Record()
	limit := s.limit(rec)
	s.mu.Lock()
	if s.active == nil {
		s.active = map[string]int{}
	}
	used := s.active[acc.UID]
	if used >= limit {
		s.mu.Unlock()
		return nil, ErrSessionsFull
	}
	s.active[acc.UID] = used + 1
	s.mu.Unlock()

	release := func() {
		s.mu.Lock()
		if n := s.active[acc.UID]; n <= 1 {
			delete(s.active, acc.UID)
		} else {
			s.active[acc.UID] = n - 1
		}
		s.mu.Unlock()
	}

	id, err := codearts.NewUserSessionID()
	if err != nil {
		release()
		return nil, err
	}
	cur := acc.Record()
	if err := codearts.Heartbeat(ctx, nil, s.base(cur), codearts.CredentialOf(cur), id, codearts.SessionBusy); err != nil {
		// The upstream may have taken the slot anyway: release our own id.
		idleCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = codearts.Heartbeat(idleCtx, nil, s.base(cur), codearts.CredentialOf(cur), id, codearts.SessionIdle)
		cancel()
		release()
		return nil, err
	}
	sess := &Session{manager: s, acc: acc, id: id, stop: make(chan struct{}), done: make(chan struct{})}
	go sess.renew()
	return sess, nil
}

// renew keeps the lease busy. A failed renewal must not release the slot: one
// missed heartbeat is not the end of the conversation.
func (s *Session) renew() {
	defer close(s.done)
	t := time.NewTicker(s.manager.interval())
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			rec := s.acc.Record()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := codearts.Heartbeat(ctx, nil, s.manager.base(rec), codearts.CredentialOf(rec), s.id, codearts.SessionBusy)
			cancel()
			if err != nil {
				s.manager.logger().Warn("codearts busy renewal failed", "account", short(s.acc.UID), "err", err)
			}
		}
	}
}

// Close sends idle exactly once and frees the slot.
func (s *Session) Close() {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		rec := s.acc.Record()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := codearts.Heartbeat(ctx, nil, s.manager.base(rec), codearts.CredentialOf(rec), s.id, codearts.SessionIdle)
		cancel()
		if err != nil {
			s.manager.logger().Warn("codearts idle release failed", "account", short(s.acc.UID), "err", err)
		}
		s.manager.mu.Lock()
		if n := s.manager.active[s.acc.UID]; n <= 1 {
			delete(s.manager.active, s.acc.UID)
		} else {
			s.manager.active[s.acc.UID] = n - 1
		}
		s.manager.mu.Unlock()
	})
}

// Active reports how many slots an account currently holds (panel/debug).
func (s *Sessions) Active(uid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[uid]
}
