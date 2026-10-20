package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/upstream/identity"
)

// Endpoints of the billing service.
const (
	resourceSummaryPath  = "/billing/meter/get-user-resource-summary"
	resourceFreePath     = "/billing/meter/get-user-resource-free-packages"
	resourcePaidPath     = "/billing/meter/get-user-resource-paid-packages"
	getResourcePath      = "/v2/billing/meter/get-user-resource"
	checkinStatusPath    = "/v2/billing/meter/checkin-activity-status"
	enterpriseUsagePath  = "/billing/meter/get-enterprise-user-usage"
	expirySentinelDays   = 730
	localTimeLayout      = "2006-01-02 15:04:05"
	creditsHTTPTimeout   = 15 * time.Second
	creditsFallbackLimit = 30 * time.Second
)

func round(f float64, digits int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', digits, 64), 64)
	return v
}

// fnum is Python's float(x): numbers, numeric strings and bools.
func fnum(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case int:
		return float64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func orStr(v any, def string) string {
	if s := jsonx.PyStrIfTruthy(v); s != "" {
		return s
	}
	return def
}

// pickNum is the legacy _val: the first listed key holding a non-empty,
// numeric value (zero included) wins; none = 0.
func pickNum(o *jsonx.Obj, keys ...string) float64 {
	for _, k := range keys {
		v := o.Get(k)
		if v == nil || v == "" {
			continue
		}
		if f, ok := fnum(v); ok {
			return f
		}
	}
	return 0
}

// deductionEndText is the package's "deduction deadline": credits stop being
// spendable at that point. Free-plan / trial packages have a monthly billing
// cycle boundary years before it, so expiry must read this field.
func deductionEndText(acc *jsonx.Obj) string {
	raw := acc.Get("DeductionEndTime")
	if raw == nil || raw == "" || raw == "0" {
		return ""
	}
	stamp, ok := fnum(raw)
	if !ok || stamp == 0 {
		return ""
	}
	if stamp > 1e11 {
		stamp /= 1000
	}
	return time.Unix(int64(stamp), int64((stamp-math.Floor(stamp))*1e9)).Format(localTimeLayout)
}

// parsePackage turns one vendor package record into the stored shape.
func parsePackage(acc *jsonx.Obj, now time.Time) map[string]any {
	size := pickNum(acc, "CycleCapacitySizePrecise", "CycleCapacitySize", "CapacitySizePrecise", "CapacitySize")
	remain := pickNum(acc, "CycleCapacityRemainPrecise", "CycleCapacityRemain", "CapacityRemainPrecise", "CapacityRemain")
	used := pickNum(acc, "CycleCapacityUsedPrecise", "CycleCapacityUsed", "CapacityUsedPrecise", "CapacityUsed")
	if size > 0 && used <= 0 && remain <= size {
		used = math.Max(0, size-remain)
	} else if size > 0 && remain <= 0 && used < size {
		remain = math.Max(0, size-used)
	}
	grant := ""
	if attrs, ok := acc.Get("AccountAttributes").([]any); ok {
		for _, a := range attrs {
			if ao, ok := a.(*jsonx.Obj); ok && ao.Get("Key") == "grantReason" {
				grant = jsonx.PyStrIfTruthy(ao.Get("Value"))
				break
			}
		}
	}
	created := ""
	if raw := acc.Get("CreateTime"); jsonx.Truthy(raw) {
		if ts, ok := fnum(raw); ok {
			if ts > 1e11 {
				ts /= 1000
			}
			created = time.Unix(int64(ts), 0).Format(localTimeLayout)
		} else {
			created = jsonx.PyStr(raw)
		}
	}
	endStr := orStr(acc.Get("CycleEndTime"), orStr(acc.Get("ExpiredTime"), ""))
	expireStr := deductionEndText(acc)
	if expireStr == "" {
		expireStr = endStr
	}
	var daysLeft any
	isExpired, noExpiry := false, false
	if expireStr != "" {
		clean := strings.Replace(expireStr, "T", " ", 1)
		if len(clean) > 19 {
			clean = clean[:19]
		}
		if end, err := time.ParseInLocation(localTimeLayout, clean, time.Local); err == nil {
			diff := end.Sub(now).Seconds()
			if diff > expirySentinelDays*86400 {
				noExpiry = true
			} else {
				daysLeft = round(diff/86400, 1)
				isExpired = diff < 0
			}
		}
	}
	inUsage := jsonx.Truthy(acc.Get("InUsage"))
	if !inUsage && !isExpired && remain > 0 && used > 0 {
		inUsage = true
	}
	return map[string]any{
		"name": orStr(acc.Get("PackageName"), "Package"), "package_code": orStr(acc.Get("PackageCode"), ""),
		"product_name": orStr(acc.Get("ProductName"), ""), "sub_product_name": orStr(acc.Get("SubProductName"), ""),
		"grant_reason": grant, "resource_id": orStr(acc.Get("ResourceId"), ""), "deal_name": orStr(acc.Get("DealName"), ""),
		"create_time": created, "remain": round(remain, 2), "used": round(used, 2), "size": round(size, 2),
		"unit": orStr(acc.Get("CapacityUnit"), "credits"), "in_usage": inUsage,
		"auto_renew":       jsonx.Truthy(acc.Get("AutoRenewFlag")) || jsonx.Truthy(acc.Get("SupportAutoRenew")),
		"cycle_start_time": orStr(acc.Get("CycleStartTime"), ""), "cycle_end_time": endStr, "expire_time": expireStr,
		"no_expiry": noExpiry, "days_left": daysLeft, "is_expired": isExpired, "status": intOr(acc.Get("Status")),
	}
}

func intOr(v any) any {
	if f, ok := fnum(v); ok {
		return int64(f)
	}
	return 0
}

func sortPackages(pkgs []map[string]any) {
	key := func(p map[string]any) (int, int, float64, float64) {
		inUse := 1
		if p["in_usage"] == true {
			inUse = 0
		}
		expired := 0
		if p["is_expired"] == true {
			expired = 1
		}
		days := 99999.0
		if d, ok := p["days_left"].(float64); ok {
			days = d
			if days < 0 {
				days = 99999 + math.Abs(days)
			}
		}
		remain, _ := p["remain"].(float64)
		return inUse, expired, days, -remain
	}
	sort.SliceStable(pkgs, func(i, j int) bool {
		a1, a2, a3, a4 := key(pkgs[i])
		b1, b2, b3, b4 := key(pkgs[j])
		switch {
		case a1 != b1:
			return a1 < b1
		case a2 != b2:
			return a2 < b2
		case a3 != b3:
			return a3 < b3
		}
		return a4 < b4
	})
}

func earliestExpiring(pkgs []map[string]any) any {
	var best map[string]any
	bestDays := 0.0
	for _, p := range pkgs {
		remain, _ := p["remain"].(float64)
		days, hasDays := p["days_left"].(float64)
		if remain > 0 && p["is_expired"] != true && hasDays && (best == nil || days < bestDays) {
			best, bestDays = p, days
		}
	}
	if best == nil {
		return nil
	}
	return map[string]any{"name": best["name"], "package_code": best["package_code"], "remain": best["remain"],
		"cycle_end_time": best["cycle_end_time"], "days_left": best["days_left"]}
}

func pct(part, whole float64, empty string) string {
	if whole > 0 {
		return fmt.Sprintf("%.1f%%", part/whole*100)
	}
	return empty
}

func packagesAsAny(pkgs []map[string]any) []any {
	out := make([]any, len(pkgs))
	for i, p := range pkgs {
		out[i] = p
	}
	return out
}

func (c *Client) creditsDoc(pkgs []map[string]any, remain, used, size float64, paid bool, checkin any, extra map[string]any) map[string]any {
	remain, used, size = round(remain, 2), round(used, 2), round(size, 2)
	now := c.now()
	doc := map[string]any{
		"remain": remain, "used": used, "size": size,
		"used_percent": pct(used, size, "0.0%"), "remain_percent": pct(remain, size, "100.0%"),
		"is_paid_user": paid, "checkin": checkin, "earliest_expiring": earliestExpiring(pkgs),
		"packages": packagesAsAny(pkgs), "updated_at": float64(now.UnixNano()) / 1e9,
		"updated_iso": now.Format(localTimeLayout),
	}
	for k, v := range extra {
		doc[k] = v
	}
	return doc
}

// billingCall posts to a billing endpoint. A 401 triggers one token refresh and
// a retry; if that does not help the account is marked as needing a re-login.
func (c *Client) billingCall(ctx context.Context, acc *pool.Account, url string, body []byte, platformWeb bool, timeout time.Duration, attempts int) (*jsonx.Obj, error) {
	build := func() identity.Headers {
		h := c.billingHeaders(acc.Record())
		if platformWeb {
			h = append(h, identity.P("X-Client-Platform", "web"))
		}
		return h
	}
	res, err := c.doJSON(ctx, acc, "POST", url, build(), body, timeout, attempts)
	if err == nil || !isAuthFailure(err) {
		return res, err
	}
	if acc.Record().RefreshToken != "" && c.Refresher != nil {
		if rerr := c.Refresher.Refresh(ctx, acc); rerr == nil {
			if res, err = c.doJSON(ctx, acc, "POST", url, build(), body, timeout, attempts); err == nil {
				return res, nil
			}
		}
	}
	acc.MarkTokenBad(true)
	acc.NoteError("登录已过期，请重新登录", "")
	return nil, errRelogin
}

type reloginError struct{}

func (reloginError) Error() string {
	return "登录已过期，请在账号管理页重新扫码登录"
}

var errRelogin error = reloginError{}

func codeOf(o *jsonx.Obj) any { return o.Get("code") }

func (c *Client) creditsCN(ctx context.Context, acc *pool.Account) (map[string]any, error) {
	rec := acc.Record()
	base := c.billingBase(rec.Realm)
	s, err := c.billingCall(ctx, acc, base+resourceSummaryPath, []byte("{}"), true, creditsHTTPTimeout, 2)
	if err != nil {
		return nil, err
	}
	if code := codeOf(s); code != nil && jsonx.PyStr(code) != "0" {
		return nil, fmt.Errorf("%s", orStr(s.Get("msg"), "code "+jsonx.PyStr(code)))
	}
	sdata, _ := s.Get("data").(*jsonx.Obj)
	if sdata == nil {
		sdata = jsonx.NewObj()
	}
	summary, _ := sdata.Get("Packages").([]any)
	var codes []any
	for _, sp := range summary {
		if so, ok := sp.(*jsonx.Obj); ok && jsonx.Truthy(so.Get("PackageCode")) {
			codes = append(codes, so.Get("PackageCode"))
		}
	}
	list := func(path string, status []any, extra bool) []any {
		if len(codes) == 0 {
			return nil
		}
		body := jsonx.NewObj()
		body.Set("PackageCodes", codes)
		body.Set("PageNumber", json.Number("1"))
		body.Set("PageSize", json.Number("200"))
		body.Set("Status", status)
		if extra {
			body.Set("NeedRenewInfo", true)
		}
		b, _ := jsonx.MarshalPy(body)
		res, err := c.billingCall(ctx, acc, base+path, b, true, creditsHTTPTimeout, 2)
		if err != nil {
			return nil
		}
		if d, ok := res.Get("data").(*jsonx.Obj); ok {
			accs, _ := d.Get("Accounts").([]any)
			return accs
		}
		return nil
	}
	free := list(resourceFreePath, []any{json.Number("0")}, false)
	paid := list(resourcePaidPath, []any{json.Number("0"), json.Number("3")}, true)

	var checkin any
	if res, err := c.billingCall(ctx, acc, c.chatBase(rec.Realm)+checkinStatusPath, []byte("{}"), true, 10*time.Second, 1); err == nil {
		if d, ok := res.Get("data").(*jsonx.Obj); ok && d.Len() > 0 {
			ci := map[string]any{"today_checked_in": false, "streak_days": 0, "daily_credit": 0, "today_credit": 0, "active": true}
			for _, k := range []string{"today_checked_in", "streak_days", "daily_credit", "today_credit", "active"} {
				if d.Has(k) {
					ci[k] = plain(d.Get(k))
				}
			}
			checkin = ci
		}
	}

	now := c.now()
	var pkgs []map[string]any
	seen := map[string]bool{}
	for _, raw := range append(append([]any{}, free...), paid...) {
		ro, ok := raw.(*jsonx.Obj)
		if !ok {
			continue
		}
		if id := jsonx.PyStrIfTruthy(ro.Get("AccountId")); id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		pkgs = append(pkgs, parsePackage(ro, now))
	}
	var remain, used, size float64
	for _, p := range pkgs {
		remain += p["remain"].(float64)
		used += p["used"].(float64)
		size += p["size"].(float64)
	}
	if len(pkgs) == 0 {
		for _, sp := range summary {
			if so, ok := sp.(*jsonx.Obj); ok {
				f, _ := fnum(so.Get("CycleTotalCapacity"))
				size += f
				f, _ = fnum(so.Get("CycleRemainCapacity"))
				remain += f
				f, _ = fnum(so.Get("CycleUsedCapacity"))
				used += f
			}
		}
	}
	sortPackages(pkgs)
	return c.creditsDoc(pkgs, remain, used, size, jsonx.Truthy(sdata.Get("IsPaidUser")), checkin, nil), nil
}

// plain converts a decoded value into plain Go data (for JSON persistence).
func plain(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case *jsonx.Obj:
		m := make(map[string]any, t.Len())
		t.Each(func(k string, x any) { m[k] = plain(x) })
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = plain(x)
		}
		return out
	}
	return v
}

func (c *Client) creditsFallback(ctx context.Context, acc *pool.Account) (map[string]any, error) {
	rec := acc.Record()
	body := jsonx.NewObj()
	body.Set("PageNumber", json.Number("1"))
	body.Set("PageSize", json.Number("100"))
	body.Set("ProductCode", "p_tcaca")
	body.Set("Status", []any{json.Number("0"), json.Number("3")})
	body.Set("PackageEndTimeRangeBegin", c.now().Format(localTimeLayout))
	body.Set("PackageEndTimeRangeEnd", "2036-01-01 00:00:00")
	b, _ := jsonx.MarshalPy(body)
	res, err := c.billingCall(ctx, acc, c.billingBase(rec.Realm)+getResourcePath, b, false, creditsFallbackLimit, 3)
	if err != nil {
		return nil, err
	}
	var accs []any
	if d, ok := res.Get("data").(*jsonx.Obj); ok {
		if r, ok := d.Get("Response").(*jsonx.Obj); ok {
			if dd, ok := r.Get("Data").(*jsonx.Obj); ok {
				accs, _ = dd.Get("Accounts").([]any)
			}
		}
	}
	now := c.now()
	var pkgs []map[string]any
	var remain, used, size float64
	for _, raw := range accs {
		if ro, ok := raw.(*jsonx.Obj); ok {
			p := parsePackage(ro, now)
			pkgs = append(pkgs, p)
			remain += p["remain"].(float64)
			used += p["used"].(float64)
			size += p["size"].(float64)
		}
	}
	sortPackages(pkgs)
	var keep any
	if prev := rec.Credits; prev != nil {
		keep = prev["checkin"]
	}
	return c.creditsDoc(pkgs, remain, used, size, false, keep, nil), nil
}

// creditsEnterprise: an enterprise member's balance is the cycle quota minus
// what the cycle has consumed; the quota resets rather than expires, so it
// never counts toward the expiring-credits preference.
func (c *Client) creditsEnterprise(ctx context.Context, acc *pool.Account) (map[string]any, error) {
	rec := acc.Record()
	res, err := c.billingCall(ctx, acc, c.billingBase(rec.Realm)+enterpriseUsagePath, []byte("{}"), true, creditsHTTPTimeout, 1)
	if err != nil {
		return nil, err
	}
	d, _ := res.Get("data").(*jsonx.Obj)
	if d == nil {
		d = jsonx.NewObj()
	}
	usedF, _ := fnum(d.Get("credit"))
	sizeF, _ := fnum(d.Get("limitNum"))
	used, size := round(usedF, 2), round(sizeF, 2)
	remain := 0.0
	if size > 0 {
		remain = round(math.Max(0, size-used), 2)
	}
	start := orStr(d.Get("cycleStartTime"), "")
	end := orStr(d.Get("cycleEndTime"), orStr(d.Get("cycleResetTime"), ""))
	name := "企业周期额度"
	if size != 0 {
		name = "企业额度"
	}
	pkg := map[string]any{
		"name": name, "package_code": "enterprise", "product_name": "", "sub_product_name": "", "grant_reason": "企业空间发放",
		"resource_id": "", "deal_name": "", "create_time": start, "remain": remain, "used": used, "size": size, "unit": "credits",
		"in_usage": used > 0, "auto_renew": true, "cycle_start_time": start, "cycle_end_time": end, "expire_time": "",
		"no_expiry": true, "days_left": nil, "is_expired": false, "status": 0,
	}
	var pkgs []map[string]any
	if size > 0 {
		pkgs = []map[string]any{pkg}
	}
	doc := c.creditsDoc(pkgs, remain, used, size, true, nil, map[string]any{"is_enterprise": true})
	doc["earliest_expiring"] = nil
	return doc, nil
}

// FetchCredits refreshes one account's balance and stores it. A refresh that
// shows credits again also lifts an early 402 park.
func (c *Client) FetchCredits(ctx context.Context, acc *pool.Account) Result {
	rec := acc.Record()
	var doc map[string]any
	var err error
	if rec.Realm == realm.CN {
		doc, err = c.creditsCN(ctx, acc)
		if err == nil && IsEnterprise(rec) {
			if size, _ := doc["size"].(float64); size == 0 {
				if ent, e2 := c.creditsEnterprise(ctx, acc); e2 == nil {
					doc = ent
				}
			}
		}
		if err == errRelogin {
			return Result{Error: err.Error(), Relogin: true}
		}
		if err != nil && IsEnterprise(rec) {
			if ent, e2 := c.creditsEnterprise(ctx, acc); e2 == nil {
				doc, err = ent, nil
			}
		}
	}
	if doc == nil {
		doc, err = c.creditsFallback(ctx, acc)
	}
	if err != nil {
		if err == errRelogin {
			return Result{Error: err.Error(), Relogin: true}
		}
		return Result{Error: err.Error()}
	}
	c.update(acc, func(r *accountsRecord) { r.Credits = doc })
	acc.ReviveBalanceCooldown()
	c.Pool.MarkPrefsDirty()
	return Result{OK: true, Extra: map[string]any{"credits": doc}}
}
