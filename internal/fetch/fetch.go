package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

var (
	ErrBlockedAddress = errors.New("address not allowed")
	ErrTooLarge       = errors.New("response too large")
	ErrScheme         = errors.New("only http and https URLs are allowed")
)

type Options struct {
	UserAgent string
	Timeout   time.Duration
	MaxBytes  int64
	// AllowPrivate disables the address check. Only for tests against local servers.
	AllowPrivate bool
}

type Client struct {
	http      *http.Client
	userAgent string
	maxBytes  int64
}

type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	URL        string
}

const maxRedirects = 5

func New(opts Options) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 5 << 20
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !opts.AllowPrivate {
		// Control runs after DNS resolution on every connection, redirects included, so a hostname can't bypass the check.
		dialer.Control = checkAddress
	}
	transport := &http.Transport{
		// No proxy: a proxy connection would go to an internal address and be refused.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   opts.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("stopped after %d redirects", maxRedirects)
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return ErrScheme
				}
				return nil
			},
		},
		userAgent: opts.UserAgent,
		maxBytes:  opts.MaxBytes,
	}
}

func (c *Client) Get(ctx context.Context, rawURL string, header http.Header) (*Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, ErrScheme
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > c.maxBytes {
		return nil, fmt.Errorf("%w (over %d bytes)", ErrTooLarge, c.maxBytes)
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body, URL: resp.Request.URL.String()}, nil
}

func checkAddress(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !isPublic(ip) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, host)
	}
	return nil
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, also used inside some clusters
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can map to private IPv4
}

func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
