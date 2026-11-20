package settings

import (
	"context"
	"slices"
	"time"
)

var PollIntervals = []int{30, 60, 120, 180, 300, 480}

type Ingest struct {
	PollIntervalMinutes int `json:"poll_interval_minutes"`
}

func DefaultIngest() Ingest {
	return Ingest{PollIntervalMinutes: 60}
}

func (i Ingest) PollInterval() time.Duration {
	return time.Duration(i.PollIntervalMinutes) * time.Minute
}

func (s *Store) Ingest(ctx context.Context) (Ingest, error) {
	i := DefaultIngest()
	if err := s.get(ctx, "ingest", &i); err != nil {
		return DefaultIngest(), err
	}
	if !slices.Contains(PollIntervals, i.PollIntervalMinutes) {
		i.PollIntervalMinutes = DefaultIngest().PollIntervalMinutes
	}
	return i, nil
}
