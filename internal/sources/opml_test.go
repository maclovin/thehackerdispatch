package sources

import (
	"os"
	"strings"
	"testing"
)

func TestCatalogParses(t *testing.T) {
	f, err := os.Open("../../docs/sources.opml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	feeds, err := ParseOPML(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) < 30 {
		t.Errorf("only %d feeds in the catalog", len(feeds))
	}
	seen := map[string]bool{}
	for _, feed := range feeds {
		in := Input{Name: feed.Name, URL: feed.URL, SectionID: 1}
		if err := in.Validate(); err != nil {
			t.Errorf("%s: %v", feed.URL, err)
		}
		if len(feed.Groups) == 0 || feed.Groups[0] != "Security" {
			t.Errorf("%s is outside the Security group: %v", feed.Name, feed.Groups)
		}
		if seen[feed.URL] {
			t.Errorf("%s is listed twice", feed.URL)
		}
		seen[feed.URL] = true
	}
}

func TestParseOPMLRejectsOtherFiles(t *testing.T) {
	for _, doc := range []string{"", "<rss></rss>", `<opml><body><outline text="empty group"/></body></opml>`} {
		if _, err := ParseOPML(strings.NewReader(doc)); err == nil {
			t.Errorf("ParseOPML(%q) succeeded", doc)
		}
	}
}
