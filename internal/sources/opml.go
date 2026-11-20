package sources

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

const maxImportFeeds = 1000

type Feed struct {
	Name   string
	URL    string
	Groups []string
}

type outline struct {
	Text     string    `xml:"text,attr"`
	Title    string    `xml:"title,attr"`
	XMLURL   string    `xml:"xmlUrl,attr"`
	Outlines []outline `xml:"outline"`
}

func ParseOPML(r io.Reader) ([]Feed, error) {
	var doc struct {
		XMLName xml.Name `xml:"opml"`
		Body    struct {
			Outlines []outline `xml:"outline"`
		} `xml:"body"`
	}
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, errors.New("this is not an OPML file")
	}
	var feeds []Feed
	var walk func(outlines []outline, groups []string)
	walk = func(outlines []outline, groups []string) {
		for _, o := range outlines {
			name := strings.TrimSpace(o.Title)
			if name == "" {
				name = strings.TrimSpace(o.Text)
			}
			if u := strings.TrimSpace(o.XMLURL); u != "" {
				feeds = append(feeds, Feed{Name: name, URL: u, Groups: groups})
			}
			walk(o.Outlines, append(groups[:len(groups):len(groups)], name))
		}
	}
	walk(doc.Body.Outlines, nil)
	switch {
	case len(feeds) == 0:
		return nil, errors.New("the file has no feeds")
	case len(feeds) > maxImportFeeds:
		return nil, fmt.Errorf("the file has %d feeds; at most %d can be imported at once", len(feeds), maxImportFeeds)
	}
	return feeds, nil
}

type ImportResult struct {
	Added   []string
	Skipped []string
	Invalid []string
}

func (s *Store) Import(ctx context.Context, feeds []Feed, defaultSection int64, enabled bool, actor audit.Actor) (ImportResult, error) {
	var result ImportResult
	sections, err := s.Sections(ctx)
	if err != nil {
		return result, err
	}
	byName := map[string]int64{}
	for _, sec := range sections {
		byName[strings.ToLower(sec.Name)] = sec.ID
		byName[sec.Slug] = sec.ID
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, f := range feeds {
			in := Input{Name: f.Name, URL: f.URL, SectionID: defaultSection, Enabled: enabled}
			for _, g := range f.Groups {
				if id, ok := byName[strings.ToLower(strings.TrimSpace(g))]; ok {
					in.SectionID = id
				}
			}
			if in.Name == "" {
				if u, err := url.Parse(in.URL); err == nil {
					in.Name = u.Host
				}
			}
			if r := []rune(in.Name); len(r) > 100 {
				in.Name = string(r[:100])
			}
			if err := in.Validate(); err != nil {
				result.Invalid = append(result.Invalid, fmt.Sprintf("%s: %s", f.URL, strings.ReplaceAll(err.Error(), "\n", "; ")))
				continue
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO sources (name, url, section_id, enabled) VALUES ($1, $2, $3, $4)
				ON CONFLICT (url) DO NOTHING`, in.Name, in.URL, in.SectionID, in.Enabled)
			if err != nil {
				return mapError(err)
			}
			if tag.RowsAffected() == 0 {
				result.Skipped = append(result.Skipped, in.Name)
			} else {
				result.Added = append(result.Added, in.Name)
			}
		}
		return audit.Record(ctx, tx, actor.Entry("sources.imported", "sources", map[string]any{
			"added": len(result.Added), "skipped": len(result.Skipped), "invalid": len(result.Invalid),
		}))
	})
	if err != nil {
		return ImportResult{}, err
	}
	return result, nil
}
