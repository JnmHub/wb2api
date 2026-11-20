package raccoon

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"wb2api/internal/store/accounts"
)

// Credential is one account's redacted view of the raccoon secret. It never
// carries the raw Extra map, so a caller cannot accidentally write through it.
type Credential struct {
	// Token is the bearer sent upstream (AccessToken).
	Token string
	// RefreshToken is empty for IAM credentials (no refresh grant).
	RefreshToken string
	// UserID is the platform account id, used to build the record uid.
	UserID string
	// ExpiresAt is the token expiry (zero when the JWT carries no exp).
	ExpiresAt time.Time
	// OrgCode is X-Org-Code; empty for a personal account.
	OrgCode string
	// OrgName/OrgRole are display-only.
	OrgName string
	OrgRole string
	// IAM marks a credential the IAM identity service issued.
	IAM bool
}

// CredentialOf assembles the credential of one account record. The token lives
// in the record's AccessToken so the pool's expiry and refresh scheduling see
// it like any other channel; everything else lives in Extra["raccoon"].
func CredentialOf(rec *accounts.Record) Credential {
	if rec == nil {
		return Credential{}
	}
	fields := rec.RaccoonFields()
	token := StripBearer(strings.TrimSpace(rec.AccessToken))
	if token == "" {
		token = StripBearer(accounts.RaccoonStringOf(fields, "accessToken", "access_token", "token"))
	}
	cred := Credential{
		Token:        token,
		RefreshToken: StripBearer(firstNonEmpty(rec.RefreshToken, accounts.RaccoonStringOf(fields, "refreshToken", "refresh_token"))),
		UserID:       accounts.RaccoonStringOf(fields, "userId", "uid", "user_id"),
		OrgCode:      rec.RaccoonOrgCode(),
		OrgName:      accounts.RaccoonStringOf(fields, "officeOrgName", "office_org_name"),
		OrgRole:      accounts.RaccoonStringOf(fields, "officeOrgRole", "office_org_role"),
		IAM:          IsIAM(token),
	}
	if rec.ExpiresAt > 0 {
		cred.ExpiresAt = time.Unix(rec.ExpiresAt, 0)
	} else {
		cred.ExpiresAt = Expiry(token)
	}
	if cred.IAM {
		// IAM has no refresh grant; a stale refresh token on disk must not be
		// mistaken for a usable one.
		cred.RefreshToken = ""
	}
	return cred
}

// ApplyToken returns a copy of rec carrying the given credential. Only the
// fields a refresh may change are touched; the record must be written back with
// a compare-and-swap (see refresh.Refresher).
func ApplyToken(rec *accounts.Record, cred Credential) *accounts.Record {
	if rec == nil {
		return nil
	}
	next := rec.WithRaccoonFields(func(fields map[string]any) {
		fields["accessToken"] = cred.Token
		fields["iam"] = cred.IAM
		if cred.IAM {
			delete(fields, "refreshToken")
			delete(fields, "refresh_token")
		} else if cred.RefreshToken != "" {
			fields["refreshToken"] = cred.RefreshToken
		}
		if cred.UserID != "" {
			fields["userId"] = cred.UserID
		}
		if cred.OrgCode != "" {
			fields["officeIdentity"] = cred.OrgCode
		}
	})
	next.AccessToken = cred.Token
	if cred.IAM {
		next.RefreshToken = ""
	} else if cred.RefreshToken != "" {
		next.RefreshToken = cred.RefreshToken
	}
	next.ExpiresAt = 0
	if !cred.ExpiresAt.IsZero() {
		next.ExpiresAt = cred.ExpiresAt.Unix()
	}
	return next.WithRaccoonAuth(accounts.RaccoonAuth{})
}

// NeedsLogin reports a credential the upstream has rejected and that cannot be
// repaired without signing in again.
func NeedsLogin(rec *accounts.Record) bool {
	if rec == nil {
		return false
	}
	if rec.RaccoonNeedsLogin() {
		return true
	}
	cred := CredentialOf(rec)
	return cred.Token == "" || (cred.RefreshToken == "" && !cred.ExpiresAt.IsZero() &&
		!cred.ExpiresAt.After(time.Now()))
}

// AccessUsable reports whether the token is present, unexpired and has no
// recorded upstream rejection.
func AccessUsable(rec *accounts.Record, now time.Time) bool {
	if rec == nil || rec.RaccoonNeedsLogin() || rec.RaccoonAuthState().AccessRejected {
		return false
	}
	cred := CredentialOf(rec)
	if cred.Token == "" {
		return false
	}
	if cred.ExpiresAt.IsZero() {
		return true
	}
	return cred.ExpiresAt.After(now)
}

// IAMOrg returns the organisation code to send; a personal identity sends none.
func IAMOrg(cred Credential) string {
	org := strings.TrimSpace(cred.OrgCode)
	if strings.EqualFold(org, "personal") {
		return ""
	}
	return org
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ------------------------------------------------------------------ request id

// RequestID returns a per-request X-Request-Id in the official shape
// "relay_js_<unix_ms>_<8 hex>". The random tail comes from an xorshift seeded
// with the clock and a process counter, so concurrent requests in the same
// millisecond do not collide.
func RequestID() string {
	counter := requestCounter.Add(1)
	now := time.Now()
	state := uint64(now.UnixNano()) ^ counter*0x9E3779B97F4A7C15 ^ uint64(now.UnixMilli())
	if state == 0 {
		state = 1
	}
	state ^= state << 13
	state ^= state >> 7
	state ^= state << 17
	return "relay_js_" + strconv.FormatInt(now.UnixMilli(), 10) + "_" + fmt.Sprintf("%08x", uint32(state>>32))
}

var requestCounter atomic.Uint64
