package settings

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const codeArtsDailyKey = "codearts_daily_checkin"

type CodeArtsDailyConfig struct {
	Enabled bool     `json:"enabled"`
	Time    string   `json:"time"`
	UIDs    []string `json:"uids"`
}

type CodeArtsDailyPatch struct {
	Enabled *bool     `json:"enabled"`
	Time    *string   `json:"time"`
	UIDs    *[]string `json:"uids"`
}

func codeArtsDailyConfig(doc Doc) CodeArtsDailyConfig {
	cfg := CodeArtsDailyConfig{Enabled: true, Time: "00:10", UIDs: []string{}}
	if raw, ok := doc[codeArtsDailyKey].(map[string]any); ok {
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
	}
	return cfg
}

func (s *Store) CodeArtsDaily() CodeArtsDailyConfig { return codeArtsDailyConfig(s.View()) }

func (p CodeArtsDailyPatch) Validate() error {
	if p.Time != nil {
		if _, err := time.Parse("15:04", *p.Time); err != nil || len(*p.Time) != 5 {
			return errors.New("CodeArts 签到时间应为 HH:mm（北京时间）")
		}
	}
	return nil
}

func (s *Store) SetCodeArtsDaily(p CodeArtsDailyPatch) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.Update(func(doc Doc) error {
		cfg := codeArtsDailyConfig(doc)
		if p.Enabled != nil {
			cfg.Enabled = *p.Enabled
		}
		if p.Time != nil {
			cfg.Time = *p.Time
		}
		if p.UIDs != nil {
			cfg.UIDs = []string{}
			seen := make(map[string]bool, len(*p.UIDs))
			for _, uid := range *p.UIDs {
				uid = strings.TrimSpace(uid)
				if uid != "" && !seen[uid] {
					seen[uid] = true
					cfg.UIDs = append(cfg.UIDs, uid)
				}
			}
		}
		doc[codeArtsDailyKey] = map[string]any{"enabled": cfg.Enabled, "time": cfg.Time, "uids": cfg.UIDs}
		return nil
	})
}
