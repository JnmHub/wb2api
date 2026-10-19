package updates

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wb2api/internal/store/settings"
)

func newChecker(t *testing.T, api, web string, enabled bool) (*Checker, *settings.Store) {
	t.Helper()
	st := settings.Open(t.TempDir())
	if enabled {
		if err := st.SetUpdateCheckEnabled(true); err != nil {
			t.Fatal(err)
		}
	}
	c := &Checker{Current: "1.0.0", Enabled: st.UpdateCheckEnabled, Store: st, APIBase: api, WebBase: web,
		Client: &http.Client{Timeout: 3 * time.Second}}
	return c, st
}

func apiServer(t *testing.T, body string, status int, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckPicksTheHighestStableRelease(t *testing.T) {
	body := `[{"tag_name":"v1.2.3","html_url":"https://github.com/x/y/releases/tag/v1.2.3"},
	          {"tag_name":"v2.0.0-beta","prerelease":true},
	          {"tag_name":"v1.10.0","html_url":"https://github.com/x/y/releases/tag/v1.10.0"},
	          {"tag_name":"nightly"}]`
	api := apiServer(t, body, 200, nil)
	c, st := newChecker(t, api.URL, "http://127.0.0.1:1", true)
	st2 := c.Check(context.Background(), false)
	if st2.LatestVersion == nil || *st2.LatestVersion != "1.10.0" || !st2.UpdateAvailable {
		t.Fatalf("%+v", st2)
	}
	if st2.LatestTag != "v1.10.0" || !strings.Contains(st2.ReleaseURL, "v1.10.0") {
		t.Fatalf("tag/url: %+v", st2)
	}
	rec := st.UpdateCheckState()
	if rec["latest_version"] != "1.10.0" || rec["last_success"] != true {
		t.Fatalf("state: %v", rec)
	}
}

func TestDisabledSwitchMakesNoRequestUnlessManual(t *testing.T) {
	var hits atomic.Int32
	api := apiServer(t, `[]`, 200, &hits)
	c, _ := newChecker(t, api.URL, "http://127.0.0.1:1", false)
	if st := c.Check(context.Background(), false); st.LatestVersion != nil || hits.Load() != 0 {
		t.Fatalf("a disabled checker must not call out: %+v hits=%d", st, hits.Load())
	}
	// manual bypasses the switch
	api2 := apiServer(t, `[{"tag_name":"v1.1.0"}]`, 200, &hits)
	c.APIBase = api2.URL
	if st := c.Check(context.Background(), true); st.LatestVersion == nil || *st.LatestVersion != "1.1.0" {
		t.Fatalf("manual check: %+v", st)
	}
}

func TestAPIFailureFallsBackToTheLatestReleaseRedirect(t *testing.T) {
	var webHits atomic.Int32
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webHits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			http.Redirect(w, r, "/releases/tag/v9.9.9", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/releases/tag/v9.9.9"):
			io.WriteString(w, "<html>release</html>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer web.Close()
	api := apiServer(t, `oops`, 500, nil)
	c, _ := newChecker(t, api.URL, web.URL, true)
	st := c.Check(context.Background(), false)
	if st.LatestVersion == nil || *st.LatestVersion != "9.9.9" || webHits.Load() != 2 {
		t.Fatalf("%+v hits=%d", st, webHits.Load())
	}
}

func TestBackgroundCheckIsDaily(t *testing.T) {
	var hits atomic.Int32
	api := apiServer(t, `[{"tag_name":"v1.0.0"}]`, 200, &hits)
	c, _ := newChecker(t, api.URL, "http://127.0.0.1:1", true)
	c.Check(context.Background(), false)
	before := hits.Load()
	st := c.Check(context.Background(), false)
	if hits.Load() != before || st.Skipped != "not due" {
		t.Fatalf("second check within a day must be skipped: %+v hits=%d", st, hits.Load())
	}
	c.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	c.Check(context.Background(), false)
	if hits.Load() <= before {
		t.Fatal("a check after 24h must run")
	}
}

func TestVersionCompare(t *testing.T) {
	if !isNewer("1.10.0", "1.9.9") || isNewer("1.0.0", "1.0.0") || isNewer("v1.0.0", "1.0.1") {
		t.Fatal("version comparison")
	}
	if !versionLess("1.2.3", "1.10.0") {
		t.Fatal("numeric, not lexicographic")
	}
}
