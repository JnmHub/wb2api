package usagedb

import "time"

// DayUse is one (account, model) pair's usage on a local day.
type DayUse struct {
	Account string
	Model   string
	Tokens  int64
	Credit  float64
}

// DayUsage folds the day's usage by (account, model) from the daily rollup. It
// is what the daily guards (token / credit / per-model limits) are computed
// from, so it reads a table of at most accounts x models rows, never requests.
func (d *DB) DayUsage(day time.Time) ([]DayUse, error) {
	rows, err := d.sql.Query(`SELECT account, model, SUM(total), SUM(credit) FROM daily
		WHERE day = ? AND account <> '' GROUP BY account, model`, localDay(day))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayUse
	for rows.Next() {
		var u DayUse
		if err := rows.Scan(&u.Account, &u.Model, &u.Tokens, &u.Credit); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
