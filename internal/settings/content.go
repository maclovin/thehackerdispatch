package settings

import (
	"context"
	"slices"
	"time"
)

var TTLDays = []int{1, 7, 15, 30, 90, 0}

type Content struct {
	TTLDays       int `json:"ttl_days"`
	DailyCap      int `json:"daily_cap"`
	MinScore      int `json:"min_score"`
	MinGapMinutes int `json:"min_gap_minutes"`
}

func DefaultContent() Content {
	return Content{TTLDays: 7, DailyCap: 10, MinScore: 40, MinGapMinutes: 30}
}

func (c Content) MinGap() time.Duration {
	return time.Duration(c.MinGapMinutes) * time.Minute
}

func (c Content) ExpiresAt(t time.Time) *time.Time {
	if c.TTLDays == 0 {
		return nil
	}
	e := t.Add(time.Duration(c.TTLDays) * 24 * time.Hour)
	return &e
}

func (s *Store) Content(ctx context.Context) (Content, error) {
	c := DefaultContent()
	c.MinGapMinutes = -1
	if err := s.get(ctx, "content", &c); err != nil {
		return DefaultContent(), err
	}
	def := DefaultContent()
	if c.MinGapMinutes < 0 || c.MinGapMinutes > 720 {
		c.MinGapMinutes = def.MinGapMinutes
	}
	if !slices.Contains(TTLDays, c.TTLDays) {
		c.TTLDays = def.TTLDays
	}
	if c.DailyCap < 1 || c.DailyCap > 100 {
		c.DailyCap = def.DailyCap
	}
	if c.MinScore < 0 || c.MinScore > 100 {
		c.MinScore = def.MinScore
	}
	return c, nil
}

type LLMStage struct {
	ModelIDs []int64 `json:"model_ids"`
	Effort   string  `json:"effort"`
}

type LLMStages struct {
	Curation LLMStage `json:"curation"`
	Writing  LLMStage `json:"writing"`
}

func (s *Store) LLMStages(ctx context.Context) (LLMStages, error) {
	var v LLMStages
	if err := s.get(ctx, "llm_stages", &v); err != nil {
		return LLMStages{}, err
	}
	return v, nil
}
