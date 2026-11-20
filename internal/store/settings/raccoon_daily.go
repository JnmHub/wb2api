package settings

import (
	"encoding/json"
	"errors"
	"time"
)

const raccoonDailyKey = "raccoon_daily_checkin"

// RaccoonDailyConfig is the raccoon channel's own daily task schedule. It is a
// separate key on purpose: WorkBuddy's check-in grid and the raccoon daily
// points link have nothing in common, and sharing one switch would make one
// channel's outage stall the other.
type RaccoonDailyConfig struct {
	Enabled bool     `json:"enabled"`
	Time    string   `json:"time"`
	UIDs    []string `json:"uids"`
}

// RaccoonDailyPatch is a partial update.
type RaccoonDailyPatch struct {
	Enabled *bool     `json:"enabled"`
	Time    *string   `json:"time"`
	UIDs    *[]string `json:"uids"`
}

func raccoonDailyConfig(doc Doc) RaccoonDailyConfig {
	cfg := RaccoonDailyConfig{Enabled: true, Time: "00:20", UIDs: []string{}}
	raw, ok := doc[raccoonDailyKey].(map[string]any)
	if !ok {
		return cfg
	}
	if v, ok := raw["enabled"].(bool); ok {
		cfg.Enabled = v
	}
	if v, ok := raw["time"].(string); ok {
		if _, err := time.Parse("15:04", v); err == nil && len(v) == 5 {
			cfg.Time = v
		}
	}
	if encoded, err := json.Marshal(raw["uids"]); err == nil {
		var uids []string
		if json.Unmarshal(encoded, &uids) == nil && uids != nil {
			cfg.UIDs = uids
		}
	}
	return cfg
}

// RaccoonDaily returns the raccoon daily schedule.
func (s *Store) RaccoonDaily() RaccoonDailyConfig { return raccoonDailyConfig(s.View()) }

// Validate checks the patch before it is stored.
func (p RaccoonDailyPatch) Validate() error {
	if p.Time != nil {
		if _, err := time.Parse("15:04", *p.Time); err != nil || len(*p.Time) != 5 {
			return errors.New("小浣熊签到时间应为 HH:mm（北京时间）")
		}
	}
	return nil
}

// SetRaccoonDaily stores the raccoon daily schedule.
func (s *Store) SetRaccoonDaily(p RaccoonDailyPatch) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.Update(func(doc Doc) error {
		cfg := raccoonDailyConfig(doc)
		if p.Enabled != nil {
			cfg.Enabled = *p.Enabled
		}
		if p.Time != nil {
			cfg.Time = *p.Time
		}
		if p.UIDs != nil {
			cfg.UIDs = *p.UIDs
		}
		doc[raccoonDailyKey] = map[string]any{
			"enabled": cfg.Enabled, "time": cfg.Time, "uids": cfg.UIDs,
		}
		return nil
	})
}
