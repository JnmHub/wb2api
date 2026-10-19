package ops

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"wb2api/internal/activity"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream"
	"wb2api/internal/upstream/identity"
)

// The INTL web-console daily activity: create a conversation, attach to its
// sandbox over ACP (SSE + JSON-RPC), request one turn, and wait for the console
// to report the conversation completed. Port of wb_webagent.py + the web leg of
// wb_accounts.daily_chat.

const (
	webOrigin          = "https://www.workbuddy.ai"
	webConversations   = "/console/as/conversations/"
	webPrompt          = "Hi"
	acpProtocolVersion = 1
	acpPollInterval    = 3 * time.Second
	acpRequestTimeout  = 20 * time.Second
)

// WebTurnResult is the outcome of one web turn.
type WebTurnResult struct {
	OK           bool   `json:"ok"`
	Conversation string `json:"conversation,omitempty"`
	Status       string `json:"status,omitempty"`
	Events       int    `json:"events,omitempty"`
	Chunks       int    `json:"chunks,omitempty"`
	ElapsedMS    int64  `json:"elapsed_ms,omitempty"`
	Attempts     int    `json:"attempts,omitempty"`
	Error        string `json:"error,omitempty"`
	Retryable    bool   `json:"retryable,omitempty"`
}

// DailyChatWeb runs only the web-console leg (manual action/debugging).
func (c *Client) DailyChatWeb(ctx context.Context, acc *pool.Account) Result {
	res := c.dailyChatWebUpstream(ctx, acc)
	c.record(acc, activity.TaskDailyChat, "daily-chat-web", res.OK, firstNonEmpty(res.Msg, res.Error))
	return res
}

func (c *Client) dailyChatWebUpstream(ctx context.Context, acc *pool.Account) Result {
	rec := acc.Record()
	if rec.Realm != realm.Intl {
		return failure("web daily chat is only for international accounts")
	}
	base := c.Settings.DailyChatWebTimeout()
	total := 1
	if c.Settings.DailyChatWebRetry() {
		total = c.Settings.DailyChatWebAttempts()
	}
	var last Result
	for attempt := 0; attempt < total; attempt++ {
		wait := base << attempt
		turn := c.webTurnOnce(ctx, acc, wait)
		turn.Attempts = attempt + 1
		res := Result{OK: turn.OK, Msg: turn.summary(), Error: turn.Error,
			Extra: map[string]any{"conversation": turn.Conversation, "status": turn.Status,
				"events": turn.Events, "chunks": turn.Chunks, "elapsed_ms": turn.ElapsedMS,
				"attempts": turn.Attempts, "retryable": turn.Retryable}}
		last = res
		if turn.OK || !turn.Retryable || ctx.Err() != nil {
			return res
		}
	}
	return last
}

func (t WebTurnResult) summary() string {
	if t.OK {
		return fmt.Sprintf("网页通道会话跑完：%d 段输出，%d ms（第 %d 轮）", t.Chunks, t.ElapsedMS, t.Attempts)
	}
	if t.Conversation != "" {
		return fmt.Sprintf("网页通道失败（会话 %s，第 %d 轮）：%s", t.Conversation, t.Attempts, t.Error)
	}
	return fmt.Sprintf("网页通道失败（第 %d 轮）：%s", t.Attempts, t.Error)
}

func (c *Client) webTurnOnce(ctx context.Context, acc *pool.Account, waitSeconds int) WebTurnResult {
	started := time.Now()
	fail := func(format string, args ...any) WebTurnResult {
		return WebTurnResult{ElapsedMS: time.Since(started).Milliseconds(), Error: fmt.Sprintf(format, args...), Retryable: true}
	}
	model := c.dailyModel(acc)
	conv, err := c.createWebConversation(ctx, acc, model)
	if err != nil {
		var business *webBusinessError
		if errors.As(err, &business) {
			return WebTurnResult{ElapsedMS: time.Since(started).Milliseconds(), Error: business.Error(), Retryable: false}
		}
		return fail("建会话失败：%v", err)
	}
	session, err := c.webGetJSON(ctx, acc, c.webConvURL(conv)+"/session")
	if err != nil {
		return WebTurnResult{Conversation: conv, ElapsedMS: time.Since(started).Milliseconds(),
			Error: "session 查询失败: " + err.Error(), Retryable: true}
	}
	data, _ := session.Get("data").(*jsonx.Obj)
	link := firstNonEmptyObj(data, "link", "endpoint")
	token := firstNonEmptyObj(data, "token")
	sessionID := firstNonEmptyObj(data, "sessionId", "session_id")
	if sessionID == "" {
		sessionID = conv
	}
	cwd := firstNonEmptyObj(data, "cwd")
	if cwd == "" {
		cwd = "/workspace"
	}
	if link == "" || token == "" {
		return WebTurnResult{Conversation: conv, ElapsedMS: time.Since(started).Milliseconds(),
			Error: "沙箱未就绪（没有 link/token）", Retryable: true}
	}
	turn := c.runACPTurn(ctx, acc, conv, link, token, sessionID, cwd, model, waitSeconds)
	turn.Conversation = conv
	if turn.OK {
		turn.ElapsedMS = time.Since(started).Milliseconds()
	}
	return turn
}

func (c *Client) webConvURL(conv string) string {
	return c.webBase() + webConversations + strings.TrimSpace(conv)
}

func (c *Client) webBase() string {
	if c.BaseOverride != "" {
		return strings.TrimRight(c.BaseOverride, "/")
	}
	return webOrigin
}

func (c *Client) webHeaders(acc *pool.Account, accept string, extra ...identity.Pair) identity.Headers {
	rec := acc.Record()
	h := identity.Headers{
		identity.P("Authorization", "Bearer "+rec.AccessToken),
		identity.P("X-User-Id", rec.UID),
		identity.P("Content-Type", "application/json"),
		identity.P("Accept", accept),
		identity.P("Origin", webOrigin),
		identity.P("Referer", webOrigin+"/app"),
		identity.P("User-Agent", webUserAgent),
	}
	for _, e := range extra {
		h.Set(e.Name, e.Value)
	}
	return h
}

func (c *Client) webDo(ctx context.Context, acc *pool.Account, method, url string, body []byte, accept string, extra ...identity.Pair) (*http.Response, error) {
	return c.Up.Wire(acc.UID, acc.Proxy()).Do(ctx, upstream.Request{
		Method: method, URL: url, Headers: c.webHeaders(acc, accept, extra...), Body: body,
		HeaderTimeout: acpRequestTimeout,
	})
}

type webBusinessError struct{ code, msg string }

func (e *webBusinessError) Error() string {
	return fmt.Sprintf("code=%s msg=%s", e.code, e.msg)
}

func (c *Client) createWebConversation(ctx context.Context, acc *pool.Account, model string) (string, error) {
	body := jsonx.NewObj()
	body.Set("prompt", webPrompt)
	body.Set("model", model)
	body.Set("conversationOrigin", "workbuddy-app")
	plugin := jsonx.NewObj()
	plugin.Set("name", "weixinpay")
	plugin.Set("marketplace", "codebuddy-builtin")
	body.Set("plugins", []any{plugin})
	raw, err := jsonx.Marshal(body)
	if err != nil {
		return "", err
	}
	resp, err := c.webDo(ctx, acc, http.MethodPost, c.webBase()+webConversations, raw, "application/json, text/plain, */*")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 120))
	}
	o, err := jsonx.UnmarshalObj(data)
	if err != nil {
		return "", fmt.Errorf("unexpected response: %w", err)
	}
	code := ""
	if v, ok := o.Lookup("code"); ok && v != nil {
		code = fmt.Sprint(v)
	}
	if code != "" && code != "0" {
		return "", &webBusinessError{code: code, msg: fmt.Sprint(o.Get("msg"))}
	}
	d, _ := o.Get("data").(*jsonx.Obj)
	id := ""
	if d != nil {
		id = jsonx.PyStrIfTruthy(d.Get("id"))
	}
	if id == "" {
		return "", errors.New("unexpected response: no conversation id")
	}
	return id, nil
}

func (c *Client) webGetJSON(ctx context.Context, acc *pool.Account, url string) (*jsonx.Obj, error) {
	resp, err := c.webDo(ctx, acc, http.MethodGet, url, nil, "application/json, text/plain, */*")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 120))
	}
	return jsonx.UnmarshalObj(data)
}

func (c *Client) webConversationStatus(ctx context.Context, acc *pool.Account, conv string) string {
	o, err := c.webGetJSON(ctx, acc, c.webConvURL(conv))
	if err != nil {
		return ""
	}
	d, _ := o.Get("data").(*jsonx.Obj)
	if d == nil {
		return ""
	}
	return jsonx.PyStrIfTruthy(d.Get("status"))
}

func firstNonEmptyObj(o *jsonx.Obj, keys ...string) string {
	if o == nil {
		return ""
	}
	for _, k := range keys {
		if s := strings.TrimSpace(jsonx.PyStrIfTruthy(o.Get(k))); s != "" {
			return s
		}
	}
	return ""
}

// runACPTurn attaches to the sandbox, drives initialize → session/load →
// session/set_model → session/prompt, then waits for the console to report the
// conversation completed.
func (c *Client) runACPTurn(ctx context.Context, acc *pool.Account, conv, link, token, sessionID, cwd, model string, waitSeconds int) WebTurnResult {
	started := time.Now()
	turn := WebTurnResult{Retryable: true}
	fail := func(format string, args ...any) WebTurnResult {
		turn.OK, turn.Error, turn.ElapsedMS = false, fmt.Sprintf(format, args...), time.Since(started).Milliseconds()
		return turn
	}
	if waitSeconds < 1 {
		waitSeconds = 1
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hdrs := identity.Headers{
		identity.P("Accept", "text/event-stream"),
		identity.P("Authorization", "Bearer "+token),
		identity.P("User-Agent", webUserAgent),
	}
	resp, err := c.Up.Wire(acc.UID, acc.Proxy()).Do(rctx, upstream.Request{
		Method: http.MethodGet, URL: link, Headers: hdrs, HeaderTimeout: acpRequestTimeout,
	})
	if err != nil {
		return fail("SSE 连接失败：%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fail("SSE 通道返回 HTTP %d", resp.StatusCode)
	}
	connID := resp.Header.Get("Acp-Connection-Id")
	if connID == "" {
		resp.Body.Close()
		return fail("SSE 通道没有返回 Acp-Connection-Id")
	}
	var updates, chunks int32
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		readACPStream(resp.Body, &updates, &chunks)
	}()
	defer func() {
		resp.Body.Close()
		select {
		case <-streamDone:
		case <-time.After(2 * time.Second):
		}
	}()

	post := func(method string, id int, params map[string]any) error {
		pctx, pcancel := context.WithTimeout(rctx, acpRequestTimeout)
		defer pcancel()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		h := identity.Headers{
			identity.P("Content-Type", "application/json"),
			identity.P("Accept", "application/json, text/event-stream"),
			identity.P("Acp-Connection-Id", connID),
			identity.P("Authorization", "Bearer "+token),
			identity.P("User-Agent", webUserAgent),
		}
		r, err := c.Up.Wire(acc.UID, acc.Proxy()).Do(pctx, upstream.Request{
			Method: http.MethodPost, URL: link, Headers: h, Body: body, HeaderTimeout: acpRequestTimeout,
		})
		if err != nil {
			return err
		}
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		if r.StatusCode != http.StatusOK && r.StatusCode != http.StatusAccepted {
			return fmt.Errorf("%s 返回 HTTP %d", method, r.StatusCode)
		}
		return nil
	}
	caps := map[string]any{
		"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
		"terminal": false,
	}
	if err := post("initialize", 1, map[string]any{"protocolVersion": acpProtocolVersion, "clientCapabilities": caps}); err != nil {
		turn.Events, turn.Chunks = int(atomic.LoadInt32(&updates)), int(atomic.LoadInt32(&chunks))
		return fail("initialize 失败：%v", err)
	}
	if err := post("session/load", 2, map[string]any{"sessionId": sessionID, "cwd": cwd, "mcpServers": []any{}}); err != nil {
		turn.Events, turn.Chunks = int(atomic.LoadInt32(&updates)), int(atomic.LoadInt32(&chunks))
		return fail("session/load 失败：%v", err)
	}
	if model != "" {
		// Failure is non-fatal: the turn still runs on the session default.
		_ = post("session/set_model", 3, map[string]any{"sessionId": sessionID, "modelId": model})
	}
	if err := post("session/prompt", 4, map[string]any{"sessionId": sessionID,
		"prompt": []any{map[string]any{"type": "text", "text": webPrompt}}}); err != nil {
		turn.Events, turn.Chunks = int(atomic.LoadInt32(&updates)), int(atomic.LoadInt32(&chunks))
		return fail("session/prompt 失败：%v", err)
	}

	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	status := ""
	for {
		status = c.webConversationStatus(rctx, acc, conv)
		if status == "completed" {
			turn.OK = true
			break
		}
		if status == "failed" || status == "error" {
			turn.Error = "会话状态=" + status
			break
		}
		if time.Now().After(deadline) {
			turn.Error = fmt.Sprintf("会话在 %ds 内没有跑完（状态=%s）", waitSeconds, firstNonEmpty(status, "未知"))
			break
		}
		select {
		case <-time.After(acpPollInterval):
		case <-rctx.Done():
			turn.Error = rctx.Err().Error()
			break
		}
		if turn.Error != "" {
			break
		}
	}
	turn.Status = status
	turn.Events = int(atomic.LoadInt32(&updates))
	turn.Chunks = int(atomic.LoadInt32(&chunks))
	turn.ElapsedMS = time.Since(started).Milliseconds()
	return turn
}

func readACPStream(body io.Reader, updates, chunks *int32) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(payload), &msg) != nil {
			continue
		}
		if msg["method"] != "session/update" {
			continue
		}
		atomic.AddInt32(updates, 1)
		params, _ := msg["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		if update["sessionUpdate"] == "agent_message_chunk" {
			atomic.AddInt32(chunks, 1)
		}
	}
}
