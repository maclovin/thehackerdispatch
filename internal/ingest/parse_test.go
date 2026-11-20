package ingest

import (
	"strings"
	"testing"
	"time"
)

const rssFeed = `<?xml version="1.0"?>
<rss version="2.0"><channel><title>Example</title>
<item>
  <title>Critical &amp; urgent:   XSS via &lt;svg&gt; tags</title>
  <link>https://example.com/post?id=1&amp;utm_source=rss&amp;utm_medium=feed#comments</link>
  <guid>post-1</guid>
  <pubDate>Mon, 19 Oct 2026 10:00:00 GMT</pubDate>
  <description><![CDATA[<p>Something <script>alert(1)</script>happened.</p>   <p>Patch now.</p>]]></description>
</item>
<item>
  <title>No date</title>
  <link>https://example.com/undated</link>
</item>
<item>
  <title>Bad link</title>
  <link>javascript:alert(1)</link>
</item>
</channel></rss>`

func TestParse(t *testing.T) {
	items, err := Parse([]byte(rssFeed))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (the javascript: link is dropped)", len(items))
	}
	first := items[0]
	if first.Title != "Critical & urgent: XSS via <svg> tags" {
		t.Errorf("title = %q", first.Title)
	}
	if first.URL != "https://example.com/post?id=1" {
		t.Errorf("url = %q (tracking parameters and fragment should be removed)", first.URL)
	}
	if first.Snippet != "Something happened. Patch now." {
		t.Errorf("snippet = %q", first.Snippet)
	}
	if first.PublishedAt == nil || !first.PublishedAt.Equal(time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("published = %v", first.PublishedAt)
	}
	if first.GUIDHash != hashGUID("post-1") {
		t.Error("GUID hash should come from <guid>")
	}
	if items[1].GUIDHash != hashGUID("https://example.com/undated") || items[1].PublishedAt != nil {
		t.Errorf("undated item = %+v", items[1])
	}
}

func TestParseAtom(t *testing.T) {
	atom := `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>A</title>
<entry><title>Atom entry</title><link href="https://example.org/a"/><id>tag:a</id>
<updated>2026-10-18T08:00:00Z</updated><summary>Short summary</summary></entry>
</feed>`
	items, err := Parse([]byte(atom))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].URL != "https://example.org/a" || items[0].PublishedAt == nil {
		t.Fatalf("items = %+v", items)
	}
}

func TestParseInvalid(t *testing.T) {
	if _, err := Parse([]byte("<html><body>not a feed</body></html>")); err == nil {
		t.Error("want an error for a non-feed document")
	}
}

func TestPlainTextTruncates(t *testing.T) {
	got := plainText(strings.Repeat("é", 600), 1000)
	if !strings.HasSuffix(got, "…") || len(got) > 1000+len("…") {
		t.Errorf("len = %d", len(got))
	}
	if !strings.HasPrefix(got, "éé") {
		t.Error("broke a UTF-8 character")
	}
}

func TestSelectNew(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { t := now.Add(time.Duration(h) * time.Hour); return &t }
	items := []Item{
		{GUIDHash: "old", PublishedAt: at(-72)},
		{GUIDHash: "recent", PublishedAt: at(-2)},
		{GUIDHash: "undated"},
		{GUIDHash: "recent", PublishedAt: at(-2)},
	}

	t.Run("first fetch takes only recent dated items", func(t *testing.T) {
		got := guidsOf(selectNew(items, nil, nil, true, now))
		if got != "recent" {
			t.Errorf("new = %s", got)
		}
	})
	t.Run("later fetch uses the cursor and the last GUIDs", func(t *testing.T) {
		got := guidsOf(selectNew(items, at(-3), []string{"old"}, false, now))
		if got != "recent,undated" {
			t.Errorf("new = %s", got)
		}
	})
	t.Run("nothing new", func(t *testing.T) {
		got := guidsOf(selectNew(items, at(-1), []string{"old", "recent", "undated"}, false, now))
		if got != "" {
			t.Errorf("new = %s", got)
		}
	})
}

func TestNextCursorIgnoresFutureDates(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)
	past := now.Add(-time.Hour)
	got := nextCursor([]Item{{PublishedAt: &future}, {PublishedAt: &past}}, nil, now)
	if got == nil || !got.Equal(now) {
		t.Errorf("cursor = %v, want now", got)
	}
}

func guidsOf(items []Item) string {
	var s []string
	for _, it := range items {
		s = append(s, it.GUIDHash)
	}
	return strings.Join(s, ",")
}
