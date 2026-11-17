package api

import (
	"context"
	"errors"

	"wb2api/internal/pool"
)

// checkinCodeArts claims the Huawei Cloud channel's daily points for one
// account. The work itself lives in codeartsync so the panel button and the
// scheduler cannot drift.
func (a *Accounts) checkinCodeArts(ctx context.Context, acc *pool.Account) error {
	if a.CodeArts == nil {
		return errors.New("CodeArts 渠道未启用")
	}
	_, _, err := a.CodeArts.Checkin(ctx, acc)
	return err
}
