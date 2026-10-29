// Package risk implements the gateway's reaction to upstream risk-control
// restrictions (port of the "风控连坐保护" logic in legacy wb_proxy.py).
//
// Default policy: park the account, never the gateway. When the upstream
// answers 403 / code 11140 ("account restricted due to unusual activity") the
// offending account is tagged (RestrictedUntil, persisted, skipped by the
// picker) and the request continues on the remaining accounts. One bad account
// therefore costs that account, not the deployment.
//
// Optional policy (WB_RESTRICTION_HOLD_SECONDS > 0): if several distinct
// accounts of one realm get restricted within a window, pause dispatch to that
// realm altogether. Accounts of one deployment share a device/exit, so a sweep
// can take the whole pool; operators who prefer stopping early over losing the
// pool can switch the pause back on.
package risk

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RestrictedAccountCooldown is how long a restricted account stays parked.
const RestrictedAccountCooldown = 7200 * time.Second

var (
	restrictedCodeRe = regexp.MustCompile(`"code"\s*:\s*"?11140"?`)
	markers          = []string{"unusual activity", "账号存在异常行为", "已被限制"}
)

// IsAccountRestriction reports whether a 403 body says the ACCOUNT is
// restricted (as opposed to a content-review rejection of the request).
func IsAccountRestriction(detail string) bool {
	if detail == "" {
		return false
	}
	if restrictedCodeRe.MatchString(detail) {
		return true
	}
	low := strings.ToLower(detail)
	for _, m := range markers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func envFloat(name string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// Config holds the tunables (same environment variables as the legacy gateway).
type Config struct {
	// RotateMax is how many more accounts one request may try after a
	// restriction (WB_RESTRICTED_403_ROTATE_MAX, default 2).
	RotateMax int
	// WaveMin distinct restricted accounts within WaveWindow can pause the
	// realm (WB_RESTRICTION_WAVE_MIN, default 2). Only effective while Hold is
	// positive.
	WaveMin int
	// WaveWindow is the sliding window (WB_RESTRICTION_WAVE_WINDOW, 900 s).
	WaveWindow time.Duration
	// Hold is the pause length (WB_RESTRICTION_HOLD_SECONDS, default 0 = off:
	// a restricted account is tagged and skipped, dispatch never pauses).
	Hold time.Duration
}

// ConfigFromEnv reads the tunables. The defaults tag the restricted account and
// keep the rest of the pool serving; the realm-wide pause is opt-in.
func ConfigFromEnv() Config {
	min := int(envFloat("WB_RESTRICTION_WAVE_MIN", 2))
	if min < 1 {
		min = 1
	}
	rotate := int(envFloat("WB_RESTRICTED_403_ROTATE_MAX", 2))
	if rotate < 0 {
		rotate = 0
	}
	return Config{
		RotateMax:  rotate,
		WaveMin:    min,
		WaveWindow: time.Duration(envFloat("WB_RESTRICTION_WAVE_WINDOW", 900) * float64(time.Second)),
		Hold:       time.Duration(envFloat("WB_RESTRICTION_HOLD_SECONDS", 0) * float64(time.Second)),
	}
}

// Guard tracks restriction events per realm.
type Guard struct {
	Cfg Config
	Now func() time.Time

	mu     sync.Mutex
	events map[string]map[string]time.Time // realm -> uid -> when
	hold   map[string]time.Time            // realm -> paused until
}

// NewGuard creates a Guard with env configuration.
func NewGuard() *Guard { return &Guard{Cfg: ConfigFromEnv()} }

func (g *Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// HoldRemaining is how long dispatch to realm stays paused (0 = running).
func (g *Guard) HoldRemaining(realm string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := g.hold[realm].Sub(g.now()); d > 0 {
		return d
	}
	return 0
}

// Note records a restriction of uid and reports the time until which the realm
// is now paused (zero when no wave was detected). Events are de-duplicated by
// uid, so probing one account repeatedly cannot trigger a pause.
func (g *Guard) Note(realm, uid string) (pausedUntil time.Time) {
	now := g.now()
	if uid = strings.TrimSpace(uid); uid == "" {
		uid = "-"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.events == nil {
		g.events = map[string]map[string]time.Time{}
		g.hold = map[string]time.Time{}
	}
	ev := map[string]time.Time{}
	for k, at := range g.events[realm] {
		if now.Sub(at) <= g.Cfg.WaveWindow {
			ev[k] = at
		}
	}
	ev[uid] = now
	g.events[realm] = ev
	if len(ev) < g.Cfg.WaveMin || g.Cfg.Hold <= 0 {
		return time.Time{}
	}
	until := now.Add(g.Cfg.Hold)
	if cur := g.hold[realm]; cur.After(until) {
		until = cur
	}
	g.hold[realm] = until
	return until
}

// Reset clears all state (operator confirmed the restriction is resolved).
func (g *Guard) Reset() {
	g.mu.Lock()
	g.events, g.hold = nil, nil
	g.mu.Unlock()
}

// HoldMessage is the explanation given to clients during a pause.
func HoldMessage(remaining time.Duration) string {
	s := int(remaining.Seconds())
	return "上游风控已限制本区域账号（403 / code 11140，账号存在异常行为）。" +
		"网关已暂停派发 " + strconv.Itoa(s) + " 秒，避免继续换号重试导致同设备账号连坐被封。" +
		"请勿换号或高频重试；按 WorkBuddy 官方渠道申诉（通常 1-3 个工作日自动解除），解封后再恢复。 " +
		"Upstream restricted the account(s); dispatch paused " + strconv.Itoa(s) + "s to avoid further bans."
}
