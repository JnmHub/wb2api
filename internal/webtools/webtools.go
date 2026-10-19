// Package webtools executes the Responses API's web_search and web_fetch server
// tools locally. The upstream has no executor: forwarding the declaration makes
// the model answer as if no tool were offered, so the gateway swaps it for an
// ordinary function, runs the call itself and feeds the text back. Port of
// legacy wb_webtools.py (search backends trimmed to the stdlib-friendly ones).
package webtools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/jsonx"
)

const (
	searchDeclType1 = "web_search"
	searchDeclType2 = "web_search_preview"
	searchDeclType3 = "web_search_preview_2025_03_11"
	fetchDeclType   = "web_fetch"

	maxResults    = 10
	maxFetchChars = 100000
	maxBodyBytes  = 4 << 20
	searchBudget  = 25 * time.Second
	httpTimeout   = 20 * time.Second
	webUA         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// Source is one citation candidate.
type Source struct {
	Title   string
	URL     string
	Snippet string
}

// WantsWeb reports which server tools the client declared (Responses shape).
func WantsWeb(tools []any) (search, fetch bool) {
	for _, raw := range tools {
		m, ok := raw.(map[string]any)
		if !ok {
			if o, ok2 := raw.(*jsonx.Obj); ok2 {
				m = map[string]any{}
				for _, k := range o.Keys() {
					m[k] = o.Get(k)
				}
			} else {
				continue
			}
		}
		name := strings.ToLower(strings.TrimSpace(str(m["name"])))
		typ := strings.ToLower(strings.TrimSpace(str(m["type"])))
		if name == "" && typ != "" {
			name = typ
		}
		switch name {
		case searchDeclType1, searchDeclType2, searchDeclType3:
			search = true
		case fetchDeclType:
			fetch = true
		}
	}
	return search, fetch
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func searchDef() *jsonx.Obj {
	params := jsonx.NewObj()
	params.Set("type", "object")
	props := jsonx.NewObj()
	q := jsonx.NewObj()
	q.Set("type", "string")
	q.Set("description", "The search query (at least 2 characters).")
	props.Set("query", q)
	n := jsonx.NewObj()
	n.Set("type", "number")
	n.Set("description", "How many results to return (1-10, default 5).")
	props.Set("numResults", n)
	params.Set("properties", props)
	params.Set("required", []any{"query"})
	fn := jsonx.NewObj()
	fn.Set("name", "web_search")
	fn.Set("description", "Searches the web and returns ranked results with URLs and snippets. Use it for current facts; cite the URLs you used.")
	fn.Set("parameters", params)
	out := jsonx.NewObj()
	out.Set("type", "function")
	out.Set("function", fn)
	return out
}

func fetchDef() *jsonx.Obj {
	params := jsonx.NewObj()
	params.Set("type", "object")
	props := jsonx.NewObj()
	u := jsonx.NewObj()
	u.Set("type", "string")
	u.Set("description", "Absolute http:// or https:// URL to fetch.")
	props.Set("url", u)
	st := jsonx.NewObj()
	st.Set("type", "number")
	st.Set("description", "Character offset to continue reading a long page.")
	props.Set("startIndex", st)
	params.Set("properties", props)
	params.Set("required", []any{"url"})
	fn := jsonx.NewObj()
	fn.Set("name", "web_fetch")
	fn.Set("description", "Fetches a URL and returns its readable text. Use startIndex to page through a long page.")
	fn.Set("parameters", params)
	out := jsonx.NewObj()
	out.Set("type", "function")
	out.Set("function", fn)
	return out
}

// InstallDefs replaces the client's web_* server-tool declarations with the
// canonical function definitions the model can actually call. It returns
// nothing; the caller flips info.WebTools.
func InstallDefs(chat *jsonx.Obj, search, fetch bool) {
	if chat == nil {
		return
	}
	var kept []any
	if list, ok := chat.Get("tools").([]any); ok {
		for _, raw := range list {
			if isWebTool(raw) {
				continue
			}
			kept = append(kept, raw)
		}
	}
	if search {
		kept = append(kept, searchDef())
	}
	if fetch {
		kept = append(kept, fetchDef())
	}
	if len(kept) == 0 {
		chat.Delete("tools")
		return
	}
	chat.Set("tools", kept)
}

func isWebTool(raw any) bool {
	m, ok := raw.(*jsonx.Obj)
	if !ok {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(m.Get("name"))))
	typ := strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(m.Get("type"))))
	if fn, ok := m.Get("function").(*jsonx.Obj); ok {
		if n := strings.ToLower(strings.TrimSpace(jsonx.PyStrIfTruthy(fn.Get("name")))); n != "" {
			name = n
		}
	}
	base := name
	if base == "" {
		base = typ
	}
	switch base {
	case searchDeclType1, searchDeclType2, searchDeclType3, fetchDeclType:
		return true
	}
	return false
}

// WebToolName reports whether a tool call name is one of the gateway's tools.
func WebToolName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "web_search", "web_fetch":
		return true
	}
	return false
}

// ExecuteHook lets tests replace the network backends; ok=false falls through
// to the real implementation.
var ExecuteHook func(ctx context.Context, name, args string) (string, bool)

// Execute runs one tool call and always returns a string for the model.
func Execute(ctx context.Context, name, arguments string) string {
	if ExecuteHook != nil {
		if s, ok := ExecuteHook(ctx, name, arguments); ok {
			return s
		}
	}
	args := parseArgs(arguments)
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web_search":
		q := queryArg(args)
		if len([]rune(q)) < 2 {
			return fmt.Sprintf("Error: web_search needs a query of at least 2 characters; got '%s'. Pass it as {\"query\": \"...\"}.", q)
		}
		n := intArg(args, "numResults", 5)
		if n < 1 {
			n = 1
		}
		if n > maxResults {
			n = maxResults
		}
		text, _ := Search(ctx, q, n)
		return text
	case "web_fetch":
		u := urlArg(args)
		if u == "" {
			return "Error: web_fetch needs a url."
		}
		return Fetch(ctx, u, intArg(args, "startIndex", 0))
	}
	return fmt.Sprintf("Error: %s is not a tool this gateway runs.", name)
}

func parseArgs(raw string) map[string]any {
	m := map[string]any{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return m
	}
	if o, err := jsonx.UnmarshalObj([]byte(raw)); err == nil {
		for _, k := range o.Keys() {
			m[k] = o.Get(k)
		}
	}
	return m
}

func queryArg(args map[string]any) string {
	for _, k := range []string{"query", "queries", "q"} {
		switch v := args[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case []any:
			var parts []string
			for _, x := range v {
				if s := strings.TrimSpace(str(x)); s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " or ")
			}
		}
	}
	return ""
}

func urlArg(args map[string]any) string {
	if s := strings.TrimSpace(str(args["url"])); s != "" {
		return s
	}
	if list, ok := args["urls"].([]any); ok {
		for _, x := range list {
			if s := strings.TrimSpace(str(x)); s != "" {
				return s
			}
		}
	}
	return ""
}

func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// ------------------------------------------------------------------- search

type engineResult struct {
	sources []Source
	note    string
	state   string // ok | empty | blocked | error
	detail  string
}

type engine struct {
	name string
	run  func(ctx context.Context, query string, n int, client *http.Client) engineResult
}

var engines = []engine{
	{"duckduckgo", ddgSearch},
	{"wikipedia", wikiSearch},
	{"news", newsSearch},
}

func httpClient() *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// Search runs the backend chain and formats the result for the model.
func Search(ctx context.Context, query string, n int) (string, []Source) {
	ctx, cancel := context.WithTimeout(ctx, searchBudget)
	defer cancel()
	client := httpClient()
	var failures []string
	for _, e := range engines {
		res := e.run(ctx, query, n, client)
		switch res.state {
		case "ok":
			if len(res.sources) == 0 {
				res.state = "empty"
			} else {
				return formatSearch(query, res.sources, res.note), res.sources
			}
		}
		if res.detail != "" {
			failures = append(failures, e.name+": "+res.state+" ("+res.detail+")")
		} else {
			failures = append(failures, e.name+": "+res.state)
		}
		if ctx.Err() != nil {
			break
		}
	}
	if len(failures) > 0 && strings.Contains(strings.Join(failures, " "), ": empty") {
		return fmt.Sprintf("No results found for: %s\n\nTry a broader or differently worded query.", query), nil
	}
	return "Error: web search is temporarily unavailable. Every backend was tried automatically and all failed (" +
		strings.Join(failures, "; ") + "). Do not retry the same query right away; tell the user that web search is temporarily unavailable and continue with what you already know.", nil
}

func formatSearch(query string, sources []Source, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for: %s", query)
	if note != "" {
		fmt.Fprintf(&b, "\n[%s]", note)
	}
	b.WriteString("\n\n")
	for i, s := range sources {
		fmt.Fprintf(&b, "%d. %s\n   %s", i+1, s.Title, s.URL)
		if s.Snippet != "" {
			fmt.Fprintf(&b, "\n   %s", s.Snippet)
		}
		if i < len(sources)-1 {
			b.WriteString("\n\n")
		}
	}
	b.WriteString("\n\nCite the sources you used at the end of your answer.")
	return b.String()
}

var (
	ddgResultRE = regexp.MustCompile(`(?s)<div class="result__body".*?</div>\s*</div>`)
	ddgLinkRE   = regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnipRE   = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</(?:a|div)>`)
	tagRE       = regexp.MustCompile(`(?s)<[^>]+>`)
)

func ddgSearch(ctx context.Context, query string, n int, client *http.Client) engineResult {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	body, status, err := get(ctx, client, u)
	if err != nil {
		return engineResult{state: "error", detail: err.Error()}
	}
	if status == 202 {
		return engineResult{state: "blocked", detail: "HTTP 202"}
	}
	if status/100 != 2 {
		return engineResult{state: "error", detail: fmt.Sprintf("HTTP %d", status)}
	}
	text := string(body)
	if isBlockedPage(text) {
		return engineResult{state: "blocked", detail: "anti-bot page"}
	}
	var out []Source
	for _, block := range ddgResultRE.FindAllString(text, -1) {
		m := ddgLinkRE.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		link := html.UnescapeString(m[1])
		if i := strings.Index(link, "uddg="); i >= 0 {
			raw := link[i+5:]
			if j := strings.IndexByte(raw, '&'); j >= 0 {
				raw = raw[:j]
			}
			if dec, err := url.QueryUnescape(raw); err == nil {
				link = dec
			}
		}
		title := cleanText(m[2])
		snip := ""
		if sm := ddgSnipRE.FindStringSubmatch(block); sm != nil {
			snip = cleanText(sm[1])
		}
		if title == "" || !strings.HasPrefix(link, "http") {
			continue
		}
		out = append(out, Source{Title: title, URL: link, Snippet: snip})
		if len(out) >= n {
			break
		}
	}
	return engineResult{sources: out, state: "ok", note: "DuckDuckGo"}
}

func wikiSearch(ctx context.Context, query string, n int, client *http.Client) engineResult {
	lang := "en"
	if hasCJK(query) {
		lang = "zh"
	}
	u := fmt.Sprintf("https://%s.wikipedia.org/w/api.php?action=query&list=search&srsearch=%s&format=json&utf8=1&srlimit=%d",
		lang, url.QueryEscape(query), n)
	body, status, err := get(ctx, client, u)
	if err != nil {
		return engineResult{state: "error", detail: err.Error()}
	}
	if status/100 != 2 {
		return engineResult{state: "error", detail: fmt.Sprintf("HTTP %d", status)}
	}
	var doc struct {
		Query struct {
			Search []struct {
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return engineResult{state: "error", detail: "invalid JSON"}
	}
	var out []Source
	for _, r := range doc.Query.Search {
		out = append(out, Source{
			Title:   r.Title,
			URL:     fmt.Sprintf("https://%s.wikipedia.org/wiki/%s", lang, url.PathEscape(strings.ReplaceAll(r.Title, " ", "_"))),
			Snippet: cleanText(r.Snippet),
		})
		if len(out) >= n {
			break
		}
	}
	return engineResult{sources: out, state: "ok", note: "Wikipedia"}
}

var rssItemRE = regexp.MustCompile(`(?s)<item>(.*?)</item>`)
var rssTitleRE = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
var rssLinkRE = regexp.MustCompile(`(?s)<link>(.*?)</link>`)

func newsSearch(ctx context.Context, query string, n int, client *http.Client) engineResult {
	hl, gl, ceid := "en-US", "US", "US:en"
	if hasCJK(query) {
		hl, gl, ceid = "zh-TW", "TW", "TW:zh"
	}
	u := fmt.Sprintf("https://news.google.com/rss/search?q=%s&hl=%s&gl=%s&ceid=%s",
		url.QueryEscape(query), hl, gl, ceid)
	body, status, err := get(ctx, client, u)
	if err != nil {
		return engineResult{state: "error", detail: err.Error()}
	}
	if status/100 != 2 {
		return engineResult{state: "error", detail: fmt.Sprintf("HTTP %d", status)}
	}
	var out []Source
	for _, item := range rssItemRE.FindAllStringSubmatch(string(body), -1) {
		tm := rssTitleRE.FindStringSubmatch(item[1])
		lm := rssLinkRE.FindStringSubmatch(item[1])
		if tm == nil || lm == nil {
			continue
		}
		out = append(out, Source{Title: cleanText(tm[1]), URL: strings.TrimSpace(lm[1])})
		if len(out) >= n {
			break
		}
	}
	return engineResult{sources: out, state: "ok", note: "Google News"}
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

func isBlockedPage(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{"captcha", "unusual traffic", "are you a robot", "access denied",
		"request blocked", "too many requests", "bot detection", "enable javascript and cookies to continue",
		"anomaly", "bots use duckduckgo", "if this error persists"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func get(ctx context.Context, client *http.Client, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", webUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	return body, resp.StatusCode, nil
}

// -------------------------------------------------------------------- fetch

// guardFn validates a URL before fetching it (tests replace it).
var guardFn = guardURL

// Fetch downloads a text page and returns a paged plain-text view.
func Fetch(ctx context.Context, rawURL string, start int) string {
	u, err := guardFn(rawURL)
	if err != nil {
		return "Error: " + err.Error()
	}
	client := &http.Client{
		Timeout: httpTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if _, err := guardFn(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "Error: could not fetch " + rawURL + "."
	}
	req.Header.Set("User-Agent", webUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		var gerr *guardError
		if errors.As(err, &gerr) {
			return "Error: redirect blocked (" + gerr.Error() + ")."
		}
		return "Error: could not fetch " + rawURL + " (" + errType(err) + ")."
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Sprintf("Error: %s answered HTTP %d.", rawURL, resp.StatusCode)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !textContentType(ct) {
		return fmt.Sprintf("Error: %s is %s, which is not a text page the gateway can read.", rawURL, firstNonEmpty(ct, "an unknown content type"))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	text := extractText(string(body), strings.Contains(ct, "html"))
	if start < 0 {
		start = 0
	}
	total := len(text)
	if start >= total {
		return fmt.Sprintf("Error: startIndex %d is past the end of the page (%d characters total).", start, total)
	}
	end := total
	if end > start+maxFetchChars {
		end = start + maxFetchChars
	}
	out := fmt.Sprintf("URL: %s\nCharacters: %d-%d of %d\n\n%s", rawURL, start, end, total, text[start:end])
	if end < total {
		out += fmt.Sprintf("\n\n[Truncated. Call web_fetch again with startIndex=%d to continue.]", end)
	}
	return out
}

func textContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	if ct == "" || strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/json", "application/xml", "application/xhtml+xml", "application/rss+xml",
		"application/atom+xml", "application/ld+json", "application/javascript", "application/x-javascript":
		return true
	}
	return strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml")
}

var (
	scriptRE = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<noscript[^>]*>.*?</noscript>`)
	wsRE     = regexp.MustCompile(`[ \t\r\f\v]+`)
	nlRE     = regexp.MustCompile(`\n{3,}`)
)

func extractText(raw string, isHTML bool) string {
	if !isHTML {
		return strings.TrimSpace(raw)
	}
	s := scriptRE.ReplaceAllString(raw, " ")
	s = tagRE.ReplaceAllString(s, "\n")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(wsRE.ReplaceAllString(line, " "))
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(nlRE.ReplaceAllString(strings.Join(out, "\n"), "\n\n"))
}

func cleanText(s string) string {
	s = tagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsRE.ReplaceAllString(s, " "))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func errType(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "timeout"
		}
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return fmt.Sprintf("%T", err)
}

type guardError struct{ reason string }

func (e *guardError) Error() string { return e.reason }

func guardURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("only http and https URLs can be fetched")
	}
	if u.User != nil {
		return nil, errors.New("URLs with credentials are not fetched")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, errors.New("refusing to fetch a local address")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return nil, errors.New("refusing to fetch a private address")
		}
		return u, nil
	}
	if !strings.Contains(host, ".") {
		return nil, errors.New("refusing to resolve a bare host name")
	}
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
	if err != nil {
		return nil, &guardError{reason: "cannot resolve host: " + err.Error()}
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, errors.New("host resolves to a private address")
		}
	}
	return u, nil
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}

// SourcesFromResult parses the gateway's own search text back into citations.
func SourcesFromResult(text string) []Source {
	var sources []Source
	lines := strings.Split(text, "\n")
	entryRE := regexp.MustCompile(`^\d+\.\s+(.+)$`)
	for i := 0; i < len(lines); i++ {
		m := entryRE.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		src := Source{Title: strings.TrimSpace(m[1])}
		if i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if strings.HasPrefix(next, "http://") || strings.HasPrefix(next, "https://") {
				src.URL = next
			}
		}
		if src.URL != "" {
			sources = append(sources, src)
		}
	}
	return sources
}
