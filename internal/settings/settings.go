package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type Branding struct {
	DisplayName       string `json:"display_name"`
	PrimaryColor      string `json:"primary_color"`
	LinkColorLight    string `json:"link_color_light"`
	VisitedColorLight string `json:"visited_color_light"`
	LinkColorDark     string `json:"link_color_dark"`
	VisitedColorDark  string `json:"visited_color_dark"`
}

func DefaultBranding() Branding {
	return Branding{
		DisplayName:       "The Hacker Dispatch",
		PrimaryColor:      "#ff6600",
		LinkColorLight:    "#0000ee",
		VisitedColorLight: "#551a8b",
		LinkColorDark:     "#8ab4f8",
		VisitedColorDark:  "#c58af9",
	}
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func ValidColor(c string) bool { return hexColor.MatchString(c) }

func (s *Store) Branding(ctx context.Context) (Branding, error) {
	b := DefaultBranding()
	if err := s.get(ctx, "branding", &b); err != nil {
		return DefaultBranding(), err
	}
	def := DefaultBranding()
	if b.DisplayName == "" {
		b.DisplayName = def.DisplayName
	}
	for _, c := range []struct{ v, d *string }{
		{&b.PrimaryColor, &def.PrimaryColor},
		{&b.LinkColorLight, &def.LinkColorLight},
		{&b.VisitedColorLight, &def.VisitedColorLight},
		{&b.LinkColorDark, &def.LinkColorDark},
		{&b.VisitedColorDark, &def.VisitedColorDark},
	} {
		if !ValidColor(*c.v) {
			*c.v = *c.d
		}
	}
	return b, nil
}

func (s *Store) get(ctx context.Context, key string, dst any) error {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key = $1", key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read setting %q: %w", key, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode setting %q: %w", key, err)
	}
	return nil
}
