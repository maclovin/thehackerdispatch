package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/settings"
)

const JobName = "notify"

const (
	maxAttempts = 10
	maxBackoff  = time.Hour
	perRun      = 50
	// Slack allows about one message per second per channel.
	sendEvery = time.Second
)

type Notifier struct {
	pool     *pgxpool.Pool
	settings *settings.Store
	slack    *Slack
	baseURL  *url.URL
	logger   *slog.Logger
}

func New(pool *pgxpool.Pool, settings *settings.Store, slack *Slack, baseURL *url.URL, logger *slog.Logger) *Notifier {
	return &Notifier{pool: pool, settings: settings, slack: slack, baseURL: baseURL, logger: logger}
}

func (n *Notifier) Enabled() bool { return n.slack != nil }

func EnqueuePost(ctx context.Context, tx pgx.Tx, storyID int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO slack_outbox (story_id, channel_id, slack_channel)
		SELECT s.id, c.id, c.slack_channel
		FROM stories s JOIN sections sec ON sec.id = s.section_id
		JOIN notification_channels c ON c.id = sec.notification_channel_id
		WHERE s.id = $1`, storyID)
	return err
}

func EnqueueReply(ctx context.Context, tx pgx.Tx, storyID, updateID int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO slack_outbox (story_id, update_id, channel_id)
		SELECT story_id, $2, channel_id FROM slack_outbox
		WHERE story_id = $1 AND update_id IS NULL AND status <> 'failed'
		LIMIT 1`, storyID, updateID)
	return err
}

type outgoing struct {
	ID           int64
	StoryID      int64
	IsReply      bool
	SlackChannel string
	Attempts     int
	Title        string
	TLDR         string
	Critical     bool
	Section      string
	SourceURL    string
	StoryChannel string
	StoryTS      string
	Note         string
	NoteURL      string
	AlertStatus  *string
}

func (n *Notifier) RunOnce(ctx context.Context) (int, error) {
	rows, err := n.pool.Query(ctx, `
		SELECT o.id, o.story_id, o.update_id IS NOT NULL, o.slack_channel, o.attempts,
		       s.title, s.tldr, s.priority = 'critical', sec.name, coalesce(l.url, ''),
		       s.slack_channel, s.slack_ts, coalesce(u.note, ''), coalesce(u.url, ''),
		       (SELECT a.status FROM slack_outbox a WHERE a.story_id = o.story_id AND a.update_id IS NULL ORDER BY a.id LIMIT 1)
		FROM slack_outbox o
		JOIN stories s ON s.id = o.story_id
		JOIN sections sec ON sec.id = s.section_id
		LEFT JOIN story_links l ON l.story_id = s.id AND l.is_primary
		LEFT JOIN story_updates u ON u.id = o.update_id
		WHERE o.status = 'pending' AND o.next_attempt_at <= now()
		ORDER BY o.id
		LIMIT $1`, perRun)
	if err != nil {
		return 0, fmt.Errorf("list due messages: %w", err)
	}
	due, err := pgx.CollectRows(rows, pgx.RowToStructByPos[outgoing])
	if err != nil {
		return 0, fmt.Errorf("list due messages: %w", err)
	}
	if len(due) == 0 {
		return 0, nil
	}
	branding, err := n.settings.Branding(ctx)
	if err != nil {
		n.logger.Error("load branding, using defaults", "err", err)
	}

	sent := 0
	var last time.Time
	threads := map[int64][2]string{}
	for _, m := range due {
		if n.slack == nil {
			if err := n.fail(ctx, m.ID, "SLACK_BOT_TOKEN isn't set"); err != nil {
				return sent, err
			}
			continue
		}
		msg := Message{Channel: m.SlackChannel, Text: n.alertText(m, branding.DisplayName)}
		if m.IsReply {
			if t, ok := threads[m.StoryID]; ok && m.StoryTS == "" {
				m.StoryChannel, m.StoryTS = t[0], t[1]
			}
			if m.StoryTS == "" {
				if m.AlertStatus == nil || *m.AlertStatus == "failed" {
					if err := n.fail(ctx, m.ID, "the story's alert wasn't sent"); err != nil {
						return sent, err
					}
				}
				continue
			}
			msg = Message{Channel: m.StoryChannel, ThreadTS: m.StoryTS, Text: replyText(m)}
		}
		if wait := sendEvery - time.Since(last); wait > 0 {
			select {
			case <-ctx.Done():
				return sent, ctx.Err()
			case <-time.After(wait):
			}
		}
		last = time.Now()
		channel, ts, err := n.slack.Post(ctx, msg)
		if err != nil {
			if ctx.Err() != nil {
				return sent, ctx.Err()
			}
			n.logger.Warn("slack message failed", "message", m.ID, "story", m.StoryID, "attempt", m.Attempts+1, "err", err)
			if err := n.retry(ctx, m, err); err != nil {
				return sent, err
			}
			continue
		}
		err = pgx.BeginFunc(ctx, n.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				UPDATE slack_outbox SET status = 'sent', sent_at = now(), attempts = attempts + 1, last_error = ''
				WHERE id = $1`, m.ID); err != nil {
				return err
			}
			if m.IsReply {
				return nil
			}
			_, err := tx.Exec(ctx, "UPDATE stories SET slack_channel = $2, slack_ts = $3 WHERE id = $1", m.StoryID, channel, ts)
			return err
		})
		if err != nil {
			return sent, fmt.Errorf("record sent message: %w", err)
		}
		if !m.IsReply {
			threads[m.StoryID] = [2]string{channel, ts}
		}
		sent++
	}
	return sent, nil
}

func (n *Notifier) retry(ctx context.Context, m outgoing, cause error) error {
	if m.Attempts+1 >= maxAttempts {
		return n.fail(ctx, m.ID, cause.Error())
	}
	wait := min(time.Minute<<m.Attempts, maxBackoff)
	var slackErr *Error
	if errors.As(cause, &slackErr) && slackErr.RetryAfter > wait {
		wait = slackErr.RetryAfter
	}
	_, err := n.pool.Exec(ctx, `
		UPDATE slack_outbox SET attempts = attempts + 1, last_error = $2, next_attempt_at = now() + make_interval(secs => $3)
		WHERE id = $1`, m.ID, truncate(cause.Error(), 300), wait.Seconds())
	return err
}

func (n *Notifier) fail(ctx context.Context, id int64, reason string) error {
	_, err := n.pool.Exec(ctx, `
		UPDATE slack_outbox SET status = 'failed', attempts = attempts + 1, last_error = $2 WHERE id = $1`,
		id, truncate(reason, 300))
	return err
}

func (n *Notifier) alertText(m outgoing, displayName string) string {
	var b strings.Builder
	if m.Critical {
		b.WriteString("🚨 CRITICAL ")
	}
	fmt.Fprintf(&b, "[%s] *%s*\n", escape(strings.ToUpper(m.Section)), escape(m.Title))
	if m.TLDR != "" {
		b.WriteString(escape(m.TLDR) + "\n")
	}
	b.WriteString(link(n.storyURL(m.StoryID), "Discuss on "+displayName))
	if m.SourceURL != "" {
		b.WriteString(" · " + link(m.SourceURL, domainOf(m.SourceURL)))
	}
	return b.String()
}

func replyText(m outgoing) string {
	text := "Update: " + escape(m.Note)
	if m.NoteURL != "" {
		text += " · " + link(m.NoteURL, domainOf(m.NoteURL))
	}
	return text
}

func (n *Notifier) storyURL(id int64) string {
	return n.baseURL.String() + "/s/" + strconv.FormatInt(id, 10)
}

func domainOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return raw
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // don't cut a UTF-8 character in half
		n--
	}
	return s[:n]
}
