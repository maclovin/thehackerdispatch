package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/settings"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

type fakeSlack struct {
	mu       sync.Mutex
	posts    []map[string]any
	failures map[string][]string
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/chat.postMessage" || r.Header.Get("Authorization") != "Bearer xoxb-test" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var msg map[string]any
	json.NewDecoder(r.Body).Decode(&msg)
	channel := msg["channel"].(string)
	if codes := f.failures[channel]; len(codes) > 0 {
		f.failures[channel] = codes[1:]
		if codes[0] == "rate_limited" {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprintf(w, `{"ok": false, "error": %q}`, codes[0])
		return
	}
	f.posts = append(f.posts, msg)
	fmt.Fprintf(w, `{"ok": true, "channel": "C0SEC", "ts": "1700000000.%06d"}`, len(f.posts))
}

func setup(t *testing.T, slack *fakeSlack) (*Notifier, *pgxpool.Pool) {
	pool := storetest.New(t)
	st := settings.New(pool)
	if err := st.SetBranding(context.Background(), settings.Branding{DisplayName: "MyInc News"}, audit.Actor{Label: "test"}); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://news.example.com")
	var client *Slack
	if slack != nil {
		srv := httptest.NewServer(slack)
		t.Cleanup(srv.Close)
		client = NewSlack("xoxb-test")
		client.baseURL = srv.URL
	}
	return New(pool, st, client, base, slog.New(slog.NewTextHandler(io.Discard, nil))), pool
}

func publish(t *testing.T, ctx context.Context, pool *pgxpool.Pool, title string, critical bool) int64 {
	priority := "normal"
	if critical {
		priority = "critical"
	}
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO stories (title, tldr, section_id, status, priority, published_at)
		VALUES ($1, 'One line <with> markup & more.', 1, 'published', $2, now()) RETURNING id`, title, priority).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	storetest.MustExec(t, pool, `INSERT INTO story_links (story_id, url, title, is_primary)
		VALUES ($1, 'https://www.example.org/a?b=1&c=2|x', 't', true)`, id)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnqueuePost(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

func addUpdate(t *testing.T, ctx context.Context, pool *pgxpool.Pool, story int64, note string) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var id int64
	if err := tx.QueryRow(ctx, "INSERT INTO story_updates (story_id, note, url) VALUES ($1, $2, 'https://vendor.example/advisory') RETURNING id",
		story, note).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := EnqueueReply(ctx, tx, story, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDelivery(t *testing.T) {
	slack := &fakeSlack{failures: map[string][]string{}}
	n, pool := setup(t, slack)
	ctx := context.Background()
	channel, err := n.CreateChannel(ctx, ChannelInput{Name: "Security news", SlackChannel: "sec-news"}, audit.Actor{Label: "test"})
	if err != nil {
		t.Fatal(err)
	}
	storetest.MustExec(t, pool, "UPDATE sections SET notification_channel_id = $1 WHERE slug = 'security'", channel)

	story := publish(t, ctx, pool, "Big flaw", true)
	addUpdate(t, ctx, pool, story, "Patch released.")
	quiet := publish(t, ctx, pool, "No channel", false)
	storetest.MustExec(t, pool, "DELETE FROM slack_outbox WHERE story_id = $1", quiet)
	addUpdate(t, ctx, pool, quiet, "Nobody hears this.")

	sent, err := n.RunOnce(ctx)
	if err != nil || sent != 2 {
		t.Fatalf("RunOnce = %d, %v", sent, err)
	}
	alert, reply := slack.posts[0], slack.posts[1]
	wantAlert := "🚨 CRITICAL [SECURITY] *Big flaw*\nOne line &lt;with&gt; markup &amp; more.\n" +
		fmt.Sprintf("<https://news.example.com/s/%d|Discuss on MyInc News>", story) +
		" · <https://www.example.org/a?b=1&amp;c=2%7Cx|example.org>"
	if alert["channel"] != "#sec-news" || alert["text"] != wantAlert || alert["unfurl_links"] != false {
		t.Errorf("alert = %v\nwant text:\n%s", alert, wantAlert)
	}
	if reply["channel"] != "C0SEC" || reply["thread_ts"] != "1700000000.000001" ||
		reply["text"] != "Update: Patch released. · <https://vendor.example/advisory|vendor.example>" {
		t.Errorf("reply = %v", reply)
	}
	var channelID, ts string
	if err := pool.QueryRow(ctx, "SELECT slack_channel, slack_ts FROM stories WHERE id = $1", story).Scan(&channelID, &ts); err != nil {
		t.Fatal(err)
	}
	if channelID != "C0SEC" || ts != "1700000000.000001" {
		t.Errorf("story's alert recorded as %q %q", channelID, ts)
	}
	var pending int
	pool.QueryRow(ctx, "SELECT count(*) FROM slack_outbox WHERE status = 'pending'").Scan(&pending)
	if pending != 0 {
		t.Errorf("%d messages still pending; a story without alert gets no reply", pending)
	}

	c, err := n.Channel(ctx, channel)
	if err != nil || c.LastStatus != "sent" || strings.Join(c.Sections, ",") != "Security" {
		t.Errorf("channel = %+v, %v", c, err)
	}
}

func TestRetries(t *testing.T) {
	slack := &fakeSlack{failures: map[string][]string{"#sec-news": {"not_in_channel", "rate_limited"}}}
	n, pool := setup(t, slack)
	ctx := context.Background()
	storetest.MustExec(t, pool, "INSERT INTO notification_channels (name, slack_channel) VALUES ('Security', '#sec-news')")
	storetest.MustExec(t, pool, "UPDATE sections SET notification_channel_id = 1")
	publish(t, ctx, pool, "Flaw", false)

	due := func() {
		storetest.MustExec(t, pool, "UPDATE slack_outbox SET next_attempt_at = now()")
	}
	waitAndError := func() (time.Duration, string) {
		var wait time.Duration
		var lastError string
		if err := pool.QueryRow(ctx, "SELECT next_attempt_at - now(), last_error FROM slack_outbox").Scan(&wait, &lastError); err != nil {
			t.Fatal(err)
		}
		return wait, lastError
	}

	if sent, err := n.RunOnce(ctx); err != nil || sent != 0 {
		t.Fatalf("RunOnce = %d, %v", sent, err)
	}
	if wait, lastError := waitAndError(); lastError != "Slack: not_in_channel" || wait < 50*time.Second || wait > time.Minute {
		t.Errorf("after the first failure: wait %v, error %q", wait, lastError)
	}
	c, _ := n.Channel(ctx, 1)
	if c.LastStatus != "pending" || c.LastError != "Slack: not_in_channel" {
		t.Errorf("the channel page should show the failure: %+v", c)
	}
	if sent, _ := n.RunOnce(ctx); sent != 0 {
		t.Error("retried before its time")
	}
	due()
	n.RunOnce(ctx)
	if wait, lastError := waitAndError(); lastError != "Slack: rate_limited" || wait < 9*time.Minute {
		t.Errorf("after rate limiting: wait %v, error %q", wait, lastError)
	}
	due()
	if sent, _ := n.RunOnce(ctx); sent != 1 {
		t.Error("the third attempt should go through")
	}

	slack.failures["#sec-news"] = []string{"channel_not_found"}
	publish(t, ctx, pool, "Another", false)
	storetest.MustExec(t, pool, "UPDATE slack_outbox SET attempts = $1 - 1 WHERE status = 'pending'", maxAttempts)
	n.RunOnce(ctx)
	var failed int
	pool.QueryRow(ctx, "SELECT count(*) FROM slack_outbox WHERE status = 'failed' AND last_error = 'Slack: channel_not_found'").Scan(&failed)
	if failed != 1 {
		t.Error("the message should fail after the last attempt")
	}
}

func TestWithoutToken(t *testing.T) {
	n, pool := setup(t, nil)
	ctx := context.Background()
	storetest.MustExec(t, pool, "INSERT INTO notification_channels (name, slack_channel) VALUES ('Security', '#sec-news')")
	storetest.MustExec(t, pool, "UPDATE sections SET notification_channel_id = 1")
	publish(t, ctx, pool, "Flaw", false)

	if n.Enabled() {
		t.Error("enabled without a token")
	}
	if sent, err := n.RunOnce(ctx); err != nil || sent != 0 {
		t.Fatalf("RunOnce = %d, %v", sent, err)
	}
	var status, lastError string
	pool.QueryRow(ctx, "SELECT status, last_error FROM slack_outbox").Scan(&status, &lastError)
	if status != "failed" || lastError != "SLACK_BOT_TOKEN isn't set" {
		t.Errorf("message %s: %q; it shouldn't wait for a token that may never come", status, lastError)
	}
	if err := n.Test(ctx, 1); err != ErrDisabled {
		t.Errorf("Test = %v", err)
	}
}

func TestChannels(t *testing.T) {
	slack := &fakeSlack{failures: map[string][]string{"C0PRIVATE01": {"not_in_channel"}}}
	n, pool := setup(t, slack)
	ctx := context.Background()
	actor := audit.Actor{Label: "admin"}

	if _, err := n.CreateChannel(ctx, ChannelInput{Name: " ", SlackChannel: "Sec News"}, actor); err == nil ||
		!strings.Contains(err.Error(), "name must have") || !strings.Contains(err.Error(), "channel name like #sec-news") {
		t.Errorf("invalid input: %v", err)
	}
	id, err := n.CreateChannel(ctx, ChannelInput{Name: "Security", SlackChannel: "C0PRIVATE01"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.CreateChannel(ctx, ChannelInput{Name: "security", SlackChannel: "#other"}, actor); err != ErrDuplicateChannel {
		t.Errorf("duplicate name: %v", err)
	}
	if err := n.Test(ctx, id); err == nil || err.Error() != "Slack: not_in_channel" {
		t.Errorf("test message to a channel without the bot: %v", err)
	}
	if err := n.UpdateChannel(ctx, id, ChannelInput{Name: "Security alerts", SlackChannel: "#sec-alerts"}, actor); err != nil {
		t.Fatal(err)
	}
	if err := n.Test(ctx, id); err != nil {
		t.Fatal(err)
	}
	if text := slack.posts[0]["text"]; slack.posts[0]["channel"] != "#sec-alerts" ||
		text != `Test message from MyInc News: alerts for the notification channel "Security alerts" will show up here.` {
		t.Errorf("test message = %v", slack.posts[0])
	}

	storetest.MustExec(t, pool, "UPDATE sections SET notification_channel_id = $1", id)
	if err := n.DeleteChannel(ctx, id, actor); err != nil {
		t.Fatal(err)
	}
	var pointing int
	pool.QueryRow(ctx, "SELECT count(*) FROM sections WHERE notification_channel_id IS NOT NULL").Scan(&pointing)
	if pointing != 0 {
		t.Error("sections still point to a deleted channel")
	}
	if err := n.DeleteChannel(ctx, id, actor); err != ErrChannelNotFound {
		t.Errorf("deleting twice: %v", err)
	}
	var audited int
	pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action LIKE 'channel.%'").Scan(&audited)
	if audited != 3 {
		t.Errorf("%d audit entries, want 3 (created, updated, deleted)", audited)
	}
}
