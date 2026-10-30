package redisx

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// modelWindowPrefix namespaces the shared per-model cooldown windows. The
// member is the account UID, the score is the unix-millisecond end of the
// window, so a stale member is simply below "now" and can be pruned.
const modelWindowPrefix = "wb:mcool:"

// ModelWindows shares per-model cooldowns between replicas.
//
// A per-model 429 window (or a lost entitlement, or an exhausted 6004 quota) is
// learned by whichever replica dispatched the request. Without sharing, the
// other replica keeps sending the same model to the same account until its own
// window expires, which is exactly how a single bad model can look like a
// fleet-wide failure.
type ModelWindows struct {
	c *Client
}

// NewModelWindows wraps c. A nil client yields a store that always errors, so
// the caller's local state stays authoritative when Redis is not configured.
func NewModelWindows(c *Client) *ModelWindows { return &ModelWindows{c: c} }

func modelWindowKey(realmName, model string) string {
	return modelWindowPrefix + strings.ToLower(realmName) + ":" + strings.ToLower(model)
}

func nowMs() int64 { return time.Now().UnixMilli() }

var errNoClient = errors.New("redis not configured")

// Mark records that account uid cannot use model until then.
func (m *ModelWindows) Mark(ctx context.Context, realmName, model, uid string, until time.Time) error {
	if m == nil || m.c == nil || m.c.Client == nil {
		return errNoClient
	}
	if uid == "" || model == "" || !until.After(time.Now()) {
		return nil
	}
	key := modelWindowKey(realmName, model)
	now := nowMs()
	end := until.UnixMilli()
	pipe := m.c.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(now, 10))
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(end), Member: uid})
	// The whole set can be dropped once the last window it holds has run out.
	pipe.PExpireAt(ctx, key, until.Add(time.Minute))
	_, err := pipe.Exec(ctx)
	return err
}

// Clear drops the window of one account for one model (a successful call is
// proof the model works again).
func (m *ModelWindows) Clear(ctx context.Context, realmName, model, uid string) error {
	if m == nil || m.c == nil || m.c.Client == nil {
		return errNoClient
	}
	return m.c.ZRem(ctx, modelWindowKey(realmName, model), uid).Err()
}

// Blocked returns the accounts of uids whose window for model has not ended
// yet, with the end time. It is one pipeline per call, bounded by the caller's
// candidate set.
func (m *ModelWindows) Blocked(ctx context.Context, realmName, model string, uids []string) (map[string]time.Time, error) {
	if m == nil || m.c == nil || m.c.Client == nil {
		return nil, errNoClient
	}
	if model == "" || len(uids) == 0 {
		return nil, nil
	}
	key := modelWindowKey(realmName, model)
	pipe := m.c.Pipeline()
	cmds := make([]*redis.FloatCmd, len(uids))
	for i, uid := range uids {
		cmds[i] = pipe.ZScore(ctx, key, uid)
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	now := nowMs()
	out := map[string]time.Time{}
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			continue
		}
		end := int64(cmd.Val())
		if end <= now {
			continue
		}
		out[uids[i]] = time.UnixMilli(end)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
