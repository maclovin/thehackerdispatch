package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAdminChannels(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	admin := adminCookie(t, srv, h)

	body := do(t, h, request{path: "/admin/channels", cookie: admin}).Body.String()
	if !strings.Contains(body, "SLACK_BOT_TOKEN isn't set") || !strings.Contains(body, "No notification channels yet") {
		t.Errorf("the page should say that Slack is off and that there are no channels:\n%s", body)
	}
	if !strings.Contains(do(t, h, request{path: "/admin/sections/1", cookie: admin}).Body.String(), "No notification channels yet") {
		t.Error("the section page should point to the channels page")
	}
	rec := do(t, h, request{method: http.MethodPost, path: "/admin/channels", cookie: admin,
		form: url.Values{"name": {"Security news"}, "slack_channel": {"Sec News"}}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "channel name like #sec-news") {
		t.Fatalf("invalid channel: status = %d", rec.Code)
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/admin/channels", cookie: admin,
		form: url.Values{"name": {"Security news"}, "slack_channel": {"sec-news"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: status = %d", rec.Code)
	}
	channels, _ := srv.Notify.Channels(t.Context())
	if len(channels) != 1 || channels[0].SlackChannel != "#sec-news" {
		t.Fatalf("channels = %+v", channels)
	}
	id := channels[0].ID

	rec = do(t, h, request{method: http.MethodPost, path: "/admin/sections/1/channel", cookie: admin,
		form: url.Values{"channel_id": {fmt.Sprint(id)}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("set the section's channel: status = %d", rec.Code)
	}
	if body := do(t, h, request{path: "/admin/sections/1", cookie: admin}).Body.String(); !strings.Contains(body,
		fmt.Sprintf(`<option value="%d" selected>Security news (#sec-news)</option>`, id)) {
		t.Errorf("the section page doesn't show its channel:\n%s", body)
	}
	if body := do(t, h, request{path: "/admin/channels", cookie: admin}).Body.String(); !strings.Contains(body, "<td>Security</td>") {
		t.Error("the channel list doesn't show the section")
	}
	rec = do(t, h, request{method: http.MethodPost, path: "/admin/sections/1/channel", cookie: admin, form: url.Values{"channel_id": {"999"}}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no longer exists") {
		t.Errorf("unknown channel: status = %d", rec.Code)
	}

	storyID := addPublished(t, srv, "Alerted story", "x", "7 days")
	srv.Pool.Exec(t.Context(), `INSERT INTO slack_outbox (story_id, channel_id, slack_channel, attempts, last_error)
		VALUES ($1, $2, '#sec-news', 1, 'Slack: not_in_channel')`, storyID, id)
	if body := do(t, h, request{path: "/admin/channels", cookie: admin}).Body.String(); !strings.Contains(body,
		`<span class="error">failing, retrying:</span> Slack: not_in_channel`) {
		t.Errorf("the channel list doesn't show the failing message:\n%s", body)
	}

	rec = do(t, h, request{method: http.MethodPost, path: fmt.Sprintf("/admin/channels/%d/test", id), cookie: admin})
	if !strings.Contains(rec.Body.String(), "SLACK_BOT_TOKEN isn&#39;t set") {
		t.Errorf("test without a token: %s", rec.Body)
	}

	page := fmt.Sprintf("/admin/channels/%d", id)
	rec = do(t, h, request{method: http.MethodPost, path: page, cookie: admin,
		form: url.Values{"name": {"Security alerts"}, "slack_channel": {"C0123456789"}}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update: status = %d", rec.Code)
	}
	if body := do(t, h, request{path: page, cookie: admin}).Body.String(); !strings.Contains(body, "Notification channel: Security alerts") ||
		!strings.Contains(body, `value="C0123456789"`) {
		t.Errorf("channel page after update:\n%s", body)
	}
	rec = do(t, h, request{method: http.MethodPost, path: page + "/delete", cookie: admin, header: map[string]string{"HX-Request": "true"}})
	if rec.Header().Get("HX-Redirect") != "/admin/channels" {
		t.Fatalf("delete: status = %d", rec.Code)
	}
	if body := do(t, h, request{path: "/admin/sections", cookie: admin}).Body.String(); strings.Contains(body, "Security alerts") {
		t.Error("the section still shows the deleted channel")
	}

	member := addUser(t, srv, h, "bob@example.com", "bob")
	if rec := do(t, h, request{path: "/admin/channels", cookie: member}); rec.Code != http.StatusForbidden {
		t.Errorf("member on the channels page: status = %d", rec.Code)
	}
}
