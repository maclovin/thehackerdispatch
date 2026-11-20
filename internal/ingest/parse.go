package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"github.com/mmcdole/gofeed"
)

const (
	maxTitleLen   = 300
	maxSnippetLen = 1000
)

type Item struct {
	GUIDHash    string
	URL         string
	Title       string
	Snippet     string
	PublishedAt *time.Time
}

var stripTags = bluemonday.StrictPolicy()

func Parse(body []byte) ([]Item, error) {
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	items := make([]Item, 0, len(feed.Items))
	for _, it := range feed.Items {
		link := canonicalURL(it.Link)
		title := cleanTitle(it.Title)
		if link == "" || title == "" {
			continue
		}
		guid := it.GUID
		if guid == "" {
			guid = link
		}
		text := it.Description
		if strings.TrimSpace(text) == "" {
			text = it.Content
		}
		published := it.PublishedParsed
		if published == nil {
			published = it.UpdatedParsed
		}
		items = append(items, Item{
			GUIDHash:    hashGUID(guid),
			URL:         link,
			Title:       title,
			Snippet:     plainText(text, maxSnippetLen),
			PublishedAt: published,
		})
	}
	return items, nil
}

func hashGUID(guid string) string {
	sum := sha256.Sum256([]byte(guid))
	return hex.EncodeToString(sum[:16])
}

var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "mc_cid": true, "mc_eid": true, "_hsenc": true, "_hsmi": true,
}

func canonicalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || trackingParams[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

func cleanTitle(s string) string {
	return truncate(strings.Join(strings.Fields(html.UnescapeString(s)), " "), maxTitleLen)
}

func plainText(s string, max int) string {
	s = html.UnescapeString(stripTags.Sanitize(s))
	return truncate(strings.Join(strings.Fields(s), " "), max)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

const firstFetchWindow = 48 * time.Hour

func selectNew(items []Item, cursor *time.Time, lastGUIDs []string, firstFetch bool, now time.Time) []Item {
	seen := make(map[string]bool, len(lastGUIDs))
	for _, g := range lastGUIDs {
		seen[g] = true
	}
	var out []Item
	for _, it := range items {
		if seen[it.GUIDHash] {
			continue
		}
		switch {
		case firstFetch:
			if it.PublishedAt == nil || it.PublishedAt.Before(now.Add(-firstFetchWindow)) {
				continue
			}
		// Strictly older: an item published in the same second as the cursor can still be new.
		case it.PublishedAt != nil && cursor != nil && it.PublishedAt.Before(*cursor):
			continue
		}
		seen[it.GUIDHash] = true
		out = append(out, it)
	}
	return out
}

func nextCursor(items []Item, cursor *time.Time, now time.Time) *time.Time {
	next := cursor
	for _, it := range items {
		if it.PublishedAt == nil {
			continue
		}
		p := *it.PublishedAt
		if p.After(now) {
			p = now
		}
		if next == nil || p.After(*next) {
			next = &p
		}
	}
	return next
}
