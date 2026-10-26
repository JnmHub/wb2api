// Package updates discovers new releases of the gateway on GitHub. It only
// reads: it never downloads, unpacks, replaces or restarts anything. The check
// is opt-in (settings.update_check_enabled) and runs at most once a day in the
// background, with a manual "check now" that bypasses both gates.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wb2api/internal/store/settings"
)

const (
	DefaultRepo = "JnmHub/wb2api"
	userAgent   = "wb2api-hub-update-check"
	maxBody     = 2 << 20
	interval    = 24 * time.Hour
)

var (
	tagRE  = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)
	urlTag = regexp.MustCompile(`/releases/tag/([^/?#]+)`)
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
)

// Status is what the panel shows.
type Status struct {
	CurrentVersion  string  `json:"current_version"`
	LatestVersion   *string `json:"latest_version"`
	LatestTag       string  `json:"latest_tag"`
	UpdateAvailable bool    `json:"update_available"`
	Enabled         bool    `json:"enabled"`
	Checking        bool    `json:"checking"`
	LastAttempt     string  `json:"last_attempt"`
	LastSuccess     bool    `json:"last_success"`
	LastError       string  `json:"last_error"`
	ReleaseURL      string  `json:"release_url"`
	RepoURL         string  `json:"repo_url"`
	PublishedAt     string  `json:"published_at"`
	Skipped         string  `json:"skipped,omitempty"`
}

type release struct {
	Tag        string
	URL        string
	Published  string
	Prerelease bool
	Draft      bool
}

// Checker performs the discovery.
type Checker struct {
	Repo    string
	Current string
	Enabled func() bool
	Store   *settings.Store
	Log     *slog.Logger
	Now     func() time.Time
	// APIBase / WebBase override github.com (tests).
	APIBase string
	WebBase string
	Client  *http.Client

	mu       sync.Mutex
	checking bool
	last     *Status
	lastErr  string
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Checker) repo() string {
	r := strings.TrimSpace(c.Repo)
	if r == "" {
		r = strings.TrimSpace(os.Getenv("WB_UPDATE_REPO"))
	}
	if r == "" || !repoRE.MatchString(r) {
		return DefaultRepo
	}
	return r
}

func (c *Checker) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return "https://api.github.com"
}

func (c *Checker) webBase() string {
	if c.WebBase != "" {
		return strings.TrimRight(c.WebBase, "/")
	}
	return "https://github.com"
}

func (c *Checker) enabled() bool { return c.Enabled == nil || c.Enabled() }

func (c *Checker) current() string {
	if c.Current == "" {
		return "0.0.0"
	}
	return strings.TrimPrefix(c.Current, "v")
}

// Status reads the persisted state without running a check.
func (c *Checker) Status() Status {
	st := Status{CurrentVersion: c.current(), Enabled: c.enabled(), RepoURL: c.webBase() + "/" + c.repo()}
	if c.Store != nil {
		rec := c.Store.UpdateCheckState()
		st.LastAttempt, _ = rec["last_attempt"].(string)
		st.LastSuccess, _ = rec["last_success"].(bool)
		if v, _ := rec["latest_version"].(string); v != "" {
			st.LatestVersion = &v
			st.UpdateAvailable = isNewer(v, st.CurrentVersion)
		}
	}
	c.mu.Lock()
	if c.last != nil {
		st.LatestVersion = c.last.LatestVersion
		st.LatestTag = c.last.LatestTag
		st.UpdateAvailable = c.last.UpdateAvailable
		st.ReleaseURL = c.last.ReleaseURL
		st.PublishedAt = c.last.PublishedAt
	}
	st.LastError = c.lastErr
	st.Checking = c.checking
	c.mu.Unlock()
	return st
}

// Check performs one discovery pass. manual bypasses the switch and the daily
// window (the panel's "check now").
func (c *Checker) Check(ctx context.Context, manual bool) Status {
	return c.check(ctx, manual, manual)
}

// CheckStartup performs the discovery pass at process start. The update switch
// still applies, but the daily window is bypassed: a restart must not hide a
// release published since the last check, which is what a plain Check would do
// for up to 24 h.
func (c *Checker) CheckStartup(ctx context.Context) Status {
	return c.check(ctx, false, true)
}

func (c *Checker) check(ctx context.Context, manual, skipDue bool) Status {
	c.mu.Lock()
	if c.checking {
		c.mu.Unlock()
		st := c.Status()
		st.Skipped = "already running"
		return st
	}
	c.checking = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.checking = false; c.mu.Unlock() }()

	now := c.now()
	if !manual && !c.enabled() {
		return c.Status()
	}
	if !skipDue && !c.due(now) {
		st := c.Status()
		st.Skipped = "not due"
		return st
	}

	rel, err := c.fetch(ctx)
	st := Status{CurrentVersion: c.current(), Enabled: c.enabled(), LastAttempt: now.Format("2006-01-02 15:04:05"), RepoURL: c.webBase() + "/" + c.repo()}
	if err != nil {
		st.LastError = describe(err)
		c.mu.Lock()
		c.lastErr = st.LastError
		c.mu.Unlock()
		if c.Store != nil {
			_ = c.Store.RecordUpdateCheck(st.LastAttempt, false, "")
		}
		c.logger().Warn("update check failed", "err", st.LastError)
		return st
	}
	st.LastSuccess = true
	ver := strings.TrimPrefix(rel.Tag, "v")
	st.LatestVersion = &ver
	st.LatestTag = rel.Tag
	st.ReleaseURL = rel.URL
	st.PublishedAt = rel.Published
	st.UpdateAvailable = isNewer(ver, st.CurrentVersion)
	c.mu.Lock()
	c.last = &st
	c.lastErr = ""
	c.mu.Unlock()
	if c.Store != nil {
		_ = c.Store.RecordUpdateCheck(st.LastAttempt, true, ver)
	}
	return st
}

func (c *Checker) due(now time.Time) bool {
	if c.Store == nil {
		return true
	}
	rec := c.Store.UpdateCheckState()
	last, _ := rec["last_attempt"].(string)
	if last == "" {
		return true
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", last, time.Local)
	if err != nil {
		return true
	}
	return now.Sub(t) >= interval
}

func (c *Checker) fetch(ctx context.Context) (release, error) {
	apiErr := c.fetchAPI(ctx)
	if apiErr.err == nil {
		return apiErr.rel, nil
	}
	rel, webErr := c.fetchWeb(ctx)
	if webErr == nil {
		return rel, nil
	}
	return release{}, apiErr.err
}

type fetched struct {
	rel release
	err error
}

func (c *Checker) fetchAPI(ctx context.Context) fetched {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase()+"/repos/"+c.repo()+"/releases", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.do(req)
	if err != nil {
		return fetched{err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fetched{err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	var list []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		HTMLURL    string `json:"html_url"`
		Published  string `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&list); err != nil {
		return fetched{err: fmt.Errorf("invalid JSON")}
	}
	var rels []release
	for _, r := range list {
		if r.Draft || r.Prerelease || !tagRE.MatchString(r.TagName) {
			continue
		}
		rel := release{Tag: r.TagName, Published: r.Published}
		if strings.HasPrefix(r.HTMLURL, "https://github.com/") {
			rel.URL = truncate(r.HTMLURL, 300)
		}
		rels = append(rels, rel)
	}
	if len(rels) == 0 {
		return fetched{err: fmt.Errorf("unexpected release document")}
	}
	sort.Slice(rels, func(i, j int) bool { return versionLess(rels[i].Tag, rels[j].Tag) })
	return fetched{rel: rels[len(rels)-1]}
}

func (c *Checker) fetchWeb(ctx context.Context) (release, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.webBase()+"/"+c.repo()+"/releases/latest", nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.do(req)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 || resp.Request == nil || resp.Request.URL == nil {
		return release{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	m := urlTag.FindStringSubmatch(resp.Request.URL.Path)
	if m == nil {
		return release{}, fmt.Errorf("unexpected release redirect")
	}
	tag := m[1]
	if !tagRE.MatchString(tag) {
		return release{}, fmt.Errorf("unexpected release tag")
	}
	rel := release{Tag: tag}
	if u := resp.Request.URL.String(); strings.HasPrefix(u, "https://github.com/") {
		rel.URL = truncate(u, 300)
	}
	return rel, nil
}

func (c *Checker) do(req *http.Request) (*http.Response, error) {
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return client.Do(req)
}

func describe(err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "HTTP "):
		return msg
	case msg == "invalid JSON", msg == "unexpected release document", msg == "unexpected release redirect", msg == "unexpected release tag":
		return msg
	default:
		return "network error: " + strings.Split(msg, ":")[0]
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func parseVersion(v string) ([3]int, bool) {
	m := tagRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

func versionLess(a, b string) bool {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	if !oka {
		return true
	}
	if !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if va[i] != vb[i] {
			return va[i] < vb[i]
		}
	}
	return false
}

func isNewer(candidate, current string) bool {
	vc, okc := parseVersion(candidate)
	vcur, okcur := parseVersion(current)
	if !okc || !okcur {
		return false
	}
	for i := 0; i < 3; i++ {
		if vc[i] != vcur[i] {
			return vc[i] > vcur[i]
		}
	}
	return false
}
