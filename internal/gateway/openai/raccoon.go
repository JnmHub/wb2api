package openai

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"wb2api/internal/catalog"
	"wb2api/internal/jsonx"
	"wb2api/internal/pool"
	"wb2api/internal/realm"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/upstream/raccoon"
)

// This file holds the raccoon channel's gateway-side orchestration. The
// upstream knowledge (URLs, headers, body shape, error codes) lives in
// internal/upstream/raccoon; nothing here may reach into the WorkBuddy shaping
// layer.

// classifyRaccoon files one raccoon failure. handled=false means "not a raccoon
// specific verdict" and lets the generic status switch take over (429, 5xx).
func (h *Handler) classifyRaccoon(q *request, acc *pool.Account, status int, body []byte,
	observed *accounts.Record, now time.Time) (result, bool) {
	if !realm.IsRaccoon(acc.Realm) {
		return result{}, false
	}
	verdict := raccoon.Classify(status, redactBody(body, observed), now)
	if verdict.Code != 0 {
		q.upstreamCode = itoaCode(verdict.Code)
	}
	switch verdict.Class {
	case raccoon.ClassOK:
		return result{}, false

	case raccoon.ClassTokenExpired:
		q.demote(h, acc)
		err := h.refreshRaccoonOnce(q, acc, observed)
		if err == nil {
			h.logger().Info("raccoon token rejected; refreshed, retrying the same account",
				"account", short(acc.UID))
			return result{kind: retrySame, status: status, body: body}, true
		}
		if q.r.Context().Err() != nil {
			return result{kind: done}, true
		}
		if errors.Is(err, refresh.ErrNeedsLogin) || errors.Is(err, raccoon.ErrNeedsLogin) {
			h.markRaccoonNeedsLogin(acc, observed, verdict.Message)
		} else if pool.SameRaccoonCredential(acc.Record(), observed) {
			// A temporary refresh failure must leave the credential renewable.
			// The refresher owns durable retry timing for the rejected token.
			acc.CoolDown(now.Add(3 * time.Second))
			h.logger().Warn("raccoon credential refresh did not complete", "account", short(acc.UID))
		}
		return result{kind: retry, status: status, body: body}, true

	case raccoon.ClassQuota:
		// Credits are an account-level budget, not a model window: parking the
		// model would keep dispatching other models to a broke account.
		until := acc.NoteBalanceCooled("小浣熊积分不足: "+truncate(verdict.Message, 120), now)
		q.demote(h, acc)
		h.logger().Warn("raccoon account out of credits; parked",
			"account", short(acc.UID), "until", until.Format(time.RFC3339))
		return result{kind: retry, status: status, body: body}, true

	case raccoon.ClassRateLimited:
		q.at.last429, q.at.last429Detail = true, truncate(verdict.Message, 600)
		wait := 60 * time.Second
		var until time.Time
		if !verdict.Reset.IsZero() {
			until = verdict.Reset
			if d := time.Until(verdict.Reset); d > time.Second {
				wait = d
			}
		}
		if rec, isNew := acc.NoteModelError(q.model, until, wait, "raccoon rate limited",
			truncate(verdict.Message, 200), now); isNew && rec != nil {
			h.persistRecord(rec)
		}
		q.demote(h, acc)
		h.logger().Warn("raccoon model throttled", "account", short(acc.UID),
			"model", q.model, "retry_in", wait.Round(time.Second))
		return result{kind: retry, status: status, body: body}, true

	case raccoon.ClassBlocked:
		// This channel does not rewrite prompts. Return a refusal once.
		return result{kind: stop, status: status, body: body}, true
	}
	return result{}, false
}

// refreshRaccoonOnce renews the credential of one account at most once per
// request. The observed record lets the refresher detect that the credential
// changed under it (panel re-import, another replica's refresh).
func (h *Handler) refreshRaccoonOnce(q *request, acc *pool.Account, observed *accounts.Record) error {
	if h.Refresher == nil || q.refreshed[acc.UID] {
		cred := raccoon.CredentialOf(observed)
		if cred.IAM || cred.RefreshToken == "" {
			return refresh.ErrNeedsLogin
		}
		return errors.New("no remaining Raccoon refresh attempt for this request")
	}
	q.refreshed[acc.UID] = true
	ctx, cancel := context.WithTimeout(q.r.Context(), 20*time.Second)
	defer cancel()
	if r, ok := h.Refresher.(observedRefresher); ok {
		return r.RefreshObserved(ctx, acc, observed)
	}
	return h.Refresher.Refresh(ctx, acc)
}

// markRaccoonNeedsLogin persists the terminal state of a rejected credential.
func (h *Handler) markRaccoonNeedsLogin(acc *pool.Account, observed *accounts.Record, reason string) {
	if current := acc.Record(); !pool.SameRaccoonCredential(current, observed) || current.RaccoonNeedsLogin() {
		return
	}
	err := pool.SaveRaccoonUpdate(h.Pool, acc, observed, h.Persist, func(rec *accounts.Record) *accounts.Record {
		return rec.WithRaccoonAuth(accounts.RaccoonAuth{
			Reason:   firstNonEmptyStr(reason, "登录态被上游拒绝"),
			Login:    true,
			Failures: rec.RaccoonAuthState().Failures + 1,
		})
	})
	if err != nil && !errors.Is(err, accounts.ErrConflict) && !errors.Is(err, accounts.ErrDeleted) {
		h.logger().Warn("raccoon rejection state not persisted", "account", short(acc.UID), "err", err)
	}
}

// raccoonModels builds /v1/models for the raccoon channel from what the pool's
// accounts declare. The upstream marks its internal Raccoon-Work ids hidden;
// discovery stores that subset separately so it is not advertised, while still
// being reachable when a client names it explicitly.
func (i *Info) raccoonModels(rl string) ([]*jsonx.Obj, catalog.Meta) {
	seen := map[string]bool{}
	var ids []string
	discovered := false
	freshEligible := false
	var fetchedAt int64
	now := time.Now()
	if i.Chat != nil && i.Chat.Pool != nil {
		for _, a := range i.Chat.Pool.All() {
			if a == nil || a.Realm != rl {
				continue
			}
			rec := a.Record()
			if raccoon.CatalogueDiscovered(rec) {
				discovered = true
			} else if a.Enabled() && rec.RaccoonAccessUsable(now) {
				freshEligible = true
			}
			for _, id := range raccoon.PublicModelsOfRecord(rec) {
				if raccoon.IsImageModel(id) || seen[id] {
					continue
				}
				seen[id] = true
				ids = append(ids, id)
			}
			if at := raccoon.ModelsDiscoveredAt(rec); at > fetchedAt {
				fetchedAt = at
			}
		}
	}
	if !discovered && freshEligible {
		for _, m := range raccoon.FallbackModels() {
			if m.Kind != "chat" || !m.Visible || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	out := make([]*jsonx.Obj, 0, len(ids))
	for _, id := range ids {
		e := jsonx.NewObj()
		e.Set("id", id)
		e.Set("object", "model")
		e.Set("created", 0)
		e.Set("owned_by", "raccoon")
		out = append(out, e)
	}
	meta := catalog.Meta{Source: "raccoon"}
	if fetchedAt > 0 {
		meta.FetchedAt = float64(fetchedAt)
	}
	return out, meta
}

// raccoonRejectsModel checks the pinned account or all accounts visible to the key.
func (h *Handler) raccoonRejectsModel(q *request, pinned *pool.Account) bool {
	if q.model == "" {
		return true
	}
	if pinned != nil {
		// The usual pinned-realm conflict message takes precedence.
		return pinned.Realm == q.realm && !raccoon.AllowsModel(pinned.Record(), q.model)
	}
	if h.Pool == nil {
		return !raccoon.KnownModel(q.model)
	}
	visible := false
	for _, a := range h.Pool.All() {
		if a == nil || !realm.IsRaccoon(a.Realm) {
			continue
		}
		if !accountAllowed(q.keyAccounts, a.UID) {
			continue
		}
		visible = true
		if raccoon.AllowsModel(a.Record(), q.model) {
			return false
		}
	}
	// With no accounts, let the pool report unavailability for known models.
	return visible || !raccoon.KnownModel(q.model)
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoaCode(code int) string {
	return strconv.Itoa(code)
}
