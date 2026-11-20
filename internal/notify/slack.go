package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Slack struct {
	token   string
	baseURL string
	http    *http.Client
}

func NewSlack(token string) *Slack {
	if token == "" {
		return nil
	}
	return &Slack{token: token, baseURL: "https://slack.com/api", http: &http.Client{Timeout: 15 * time.Second}}
}

type Error struct {
	Code       string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return "Slack: " + e.Code }

type Message struct {
	Channel  string
	Text     string
	ThreadTS string
}

func (s *Slack) Post(ctx context.Context, m Message) (channel, ts string, err error) {
	payload := map[string]any{"channel": m.Channel, "text": m.Text, "unfurl_links": false, "unfurl_media": false}
	if m.ThreadTS != "" {
		payload["thread_ts"] = m.ThreadTS
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		secs, _ := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
		return "", "", &Error{Code: "rate_limited", RetryAfter: time.Duration(max(secs, 1)) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("Slack: HTTP %d", resp.StatusCode)
	}
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("Slack: read answer: %w", err)
	}
	if !out.OK {
		return "", "", &Error{Code: out.Error}
	}
	return out.Channel, out.TS, nil
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// link percent-encodes the characters that would end the link markup: URLs come from feeds.
func link(rawURL, label string) string {
	u := strings.NewReplacer("&", "&amp;", "<", "%3C", ">", "%3E", "|", "%7C").Replace(rawURL)
	return "<" + u + "|" + escape(label) + ">"
}
