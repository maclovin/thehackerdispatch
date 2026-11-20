package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestIsPublic(t *testing.T) {
	tests := map[string]bool{
		"8.8.8.8":         true,
		"2606:4700::1111": true,
		"127.0.0.1":       false,
		"10.1.2.3":        false,
		"172.16.0.1":      false,
		"192.168.1.1":     false,
		"169.254.169.254": false,
		"100.64.0.1":      false,
		"0.0.0.0":         false,
		"::1":             false,
		"fd00::1":         false,
		"::ffff:10.0.0.1": false,
	}
	for addr, want := range tests {
		if got := isPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestGetBlocksPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	_, err := New(Options{}).Get(context.Background(), srv.URL, nil)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
}

func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed":
			w.Header().Set("ETag", `"v1"`)
			w.Write([]byte("ua=" + r.UserAgent()))
		case "/big":
			w.Write([]byte(strings.Repeat("x", 2048)))
		case "/redirect-file":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(Options{UserAgent: "hd-test", MaxBytes: 1024, AllowPrivate: true})
	ctx := context.Background()

	resp, err := c.Get(ctx, srv.URL+"/feed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "ua=hd-test" || resp.Header.Get("ETag") != `"v1"` {
		t.Errorf("body = %q, etag = %q", resp.Body, resp.Header.Get("ETag"))
	}

	resp, err = c.Get(ctx, srv.URL+"/feed", http.Header{"User-Agent": {"Mozilla/5.0"}})
	if err != nil || string(resp.Body) != "ua=Mozilla/5.0" {
		t.Errorf("User-Agent override: body = %q, err = %v", resp.Body, err)
	}

	if resp, err := c.Get(ctx, srv.URL+"/missing", nil); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("404 should be a response, not an error: err = %v", err)
	}
	if _, err := c.Get(ctx, srv.URL+"/big", nil); !errors.Is(err, ErrTooLarge) {
		t.Errorf("big body: err = %v, want ErrTooLarge", err)
	}
	if _, err := c.Get(ctx, srv.URL+"/redirect-file", nil); !errors.Is(err, ErrScheme) {
		t.Errorf("redirect to file://: err = %v, want ErrScheme", err)
	}
	if _, err := c.Get(ctx, srv.URL+"/loop", nil); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("redirect loop: err = %v", err)
	}
	if _, err := c.Get(ctx, "ftp://example.com/x", nil); !errors.Is(err, ErrScheme) {
		t.Errorf("ftp URL: err = %v, want ErrScheme", err)
	}
}
