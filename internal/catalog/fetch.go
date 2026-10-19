package catalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

// representative is the realm's account used for catalogue calls: the first
// one holding a token, else the first one.
func (s *Service) representative(r string) *pool.Account {
	if s.Pool == nil {
		return nil
	}
	var first *pool.Account
	for _, a := range s.Pool.All() {
		rec := a.Record()
		if r != "" && rec.Realm != r {
			continue
		}
		if rec.AccessToken != "" {
			return a
		}
		if first == nil {
			first = a
		}
	}
	return first
}

func (s *Service) configURL(r string) string {
	base := realm.Get(r).ChatUpstream
	if s.UpstreamOverride != "" {
		base = s.UpstreamOverride
	}
	return strings.TrimRight(base, "/") + ConfigPath
}

// configHeaders: the user agent decides which catalogue comes back, and only
// the desktop UA gets the full list (an unknown one is a hard 400), so this
// uses the realm's fixed desktop UA rather than the account's identity.
func configHeaders(r, ua, uid, token string) identity.Headers {
	cfg := realm.Get(r)
	return identity.Headers{
		identity.P("Accept", "application/json, text/plain, */*"),
		identity.P("User-Agent", ua),
		identity.P("Origin", cfg.Origin),
		identity.P("Referer", cfg.Origin+"/"),
		identity.P("Authorization", "Bearer "+token),
		identity.P("X-User-Id", uid),
	}
}

// fetchRemote asks the realm's own product-config endpoint for the catalogue.
// At most two 10s attempts (one per desktop UA): the endpoint sits behind a WAF
// where a dropped connection is normal, and every caller has a fallback.
func (s *Service) fetchRemote(ctx context.Context, r string) *Parsed {
	if r != realm.Intl && r != realm.CN {
		return nil
	}
	acc := s.representative(r)
	if acc == nil || s.Up == nil {
		return nil
	}
	rec := acc.Record()
	if rec.AccessToken == "" {
		s.logger().Info("remote catalog: no usable account, skipping", "realm", r)
		return nil
	}
	ver, chatUA := identity.DesktopVersionIntl, identity.DesktopUAIntl
	if r == realm.CN {
		ver, chatUA = identity.DesktopVersionCN, identity.DesktopUACN
	}
	uas := []string{chatUA, "WorkBuddy/" + ver}
	if uas[1] == uas[0] {
		uas = uas[:1]
	}
	var last any
	for _, ua := range uas {
		body, err := s.getConfig(ctx, acc, rec.ProxyLegacy, s.configURL(r), configHeaders(r, ua, rec.UID, rec.AccessToken))
		if err != nil {
			last = err
			continue
		}
		doc, err := jsonx.Unmarshal(body)
		if err != nil {
			last = err
			continue
		}
		if p := ParseRemote(doc); p != nil {
			s.persist(r, body)
			return p
		}
		last = "empty catalogue"
	}
	s.logger().Warn("remote catalog fetch failed", "realm", r, "err", last)
	return nil
}

func (s *Service) getConfig(ctx context.Context, acc *pool.Account, proxy, url string, h identity.Headers) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := s.Up.Wire(acc.UID, proxy).Do(ctx, upstream.Request{Method: http.MethodGet, URL: url, Headers: h})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// persist archives the last successful /v3/config body (nothing reads it back;
// a write failure never affects the result).
func (s *Service) persist(r string, body []byte) {
	if s.CacheDir == "" {
		return
	}
	if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
		return
	}
	path := filepath.Join(s.CacheDir, "product-config-"+r+".json")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, body, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// ------------------------------------------------------------------ refresh

// RefreshResult is what the panel shows after a manual refresh.
type RefreshResult struct {
	OK         bool    `json:"ok"`
	Realm      string  `json:"realm,omitempty"`
	Source     string  `json:"source,omitempty"`
	Count      int     `json:"count"`
	Cooldown   bool    `json:"cooldown,omitempty"`
	RetryAfter float64 `json:"retry_after,omitempty"`
	Message    string  `json:"msg"`
}

// Refresh pulls the realm's catalogue from upstream now and publishes it.
// Manual refreshes are rate limited per realm (failures count too): the
// upstream has a WAF and restraint is part of not tripping risk control. A
// failed pull keeps the previous list; this never reads the desktop cache
// file, so "ok" really means upstream answered.
func (s *Service) Refresh(ctx context.Context, r string) RefreshResult {
	if r != realm.Intl && r != realm.CN {
		return RefreshResult{Message: "realm 必须是 intl 或 cn"}
	}
	now := s.now()
	s.rateMu.Lock()
	if s.lastTry == nil {
		s.lastTry, s.lastGood = map[string]time.Time{}, map[string]time.Time{}
	}
	if wait := refreshCooldown - now.Sub(s.lastTry[r]); wait > 0 {
		s.rateMu.Unlock()
		secs := wait.Seconds()
		return RefreshResult{Realm: r, Cooldown: true, RetryAfter: float64(int(secs*10+0.5)) / 10,
			Message: fmt.Sprintf("刷新太频繁：约 %d 秒后可再试", max(1, int(secs+0.5)))}
	}
	s.lastTry[r] = now
	s.rateMu.Unlock()

	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	p := s.fetchRemote(ctx, r)
	if p == nil {
		s.logger().Info("catalog refresh kept the previous list (upstream gave none)", "realm", r)
		return RefreshResult{Realm: r, Message: "上游未返回可用目录（账号不可用或网络异常），已保留现有清单"}
	}
	entries := Merge(Live(Curate(p.IDs, p.Meta), p.Meta), r, true)
	s.cache.Store(r, &cached{at: s.now(), entries: entries,
		meta: Meta{Source: SourceRemote, FetchedAt: float64(s.now().UnixNano()) / 1e9}})
	s.noteGood(r)
	s.logger().Info("catalog refresh updated from product config", "realm", r, "models", len(entries))
	return RefreshResult{OK: true, Realm: r, Source: SourceRemote, Count: len(entries),
		Message: fmt.Sprintf("已从上游 API 更新清单，共 %d 个模型", len(entries))}
}

func (s *Service) noteGood(r string) {
	s.rateMu.Lock()
	if s.lastGood == nil {
		s.lastTry, s.lastGood = map[string]time.Time{}, map[string]time.Time{}
	}
	s.lastGood[r] = s.now()
	s.rateMu.Unlock()
}

// dailyDue lists the realms whose last successful upstream pull is a day old
// (or that never had one).
func (s *Service) dailyDue(now time.Time) []string {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	var due []string
	for _, r := range []string{realm.Intl, realm.CN} {
		if now.Sub(s.lastGood[r]) >= dailyInterval {
			due = append(due, r)
		}
	}
	return due
}

// DailyPass refreshes every due realm that has an account at all.
func (s *Service) DailyPass(ctx context.Context) []RefreshResult {
	var out []RefreshResult
	for _, r := range s.dailyDue(s.now()) {
		if s.representative(r) == nil {
			continue
		}
		res := s.Refresh(ctx, r)
		s.logger().Info("catalog daily", "realm", r, "result", res.Message)
		out = append(out, res)
	}
	return out
}

// RunDaily is the once-a-day safety net for deployments nobody queries: after
// a minute for startup to settle, look hourly for realms that are due.
func (s *Service) RunDaily(ctx context.Context) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.DailyPass(ctx)
			t.Reset(time.Hour)
		}
	}
}
