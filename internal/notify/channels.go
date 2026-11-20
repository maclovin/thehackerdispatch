package notify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

var (
	ErrChannelNotFound  = errors.New("notification channel not found")
	ErrDuplicateChannel = errors.New("a notification channel with this name already exists")
	ErrDisabled         = errors.New("SLACK_BOT_TOKEN isn't set, so nothing can be sent to Slack")
)

type Channel struct {
	ID           int64
	Name         string
	SlackChannel string
	Sections     []string
	LastStatus   string
	LastError    string
	LastAt       *time.Time
}

type ChannelInput struct {
	Name         string
	SlackChannel string
}

var (
	slackChannelName = regexp.MustCompile(`^#?[a-z0-9][a-z0-9_-]{0,79}$`)
	slackChannelID   = regexp.MustCompile(`^[CG][A-Z0-9]{8,}$`)
)

func (in *ChannelInput) Validate() error {
	in.Name = strings.TrimSpace(in.Name)
	in.SlackChannel = strings.TrimSpace(in.SlackChannel)
	var errs []error
	if in.Name == "" || len([]rune(in.Name)) > 40 {
		errs = append(errs, errors.New("name must have 1 to 40 characters"))
	}
	switch {
	case slackChannelID.MatchString(in.SlackChannel):
	case slackChannelName.MatchString(in.SlackChannel):
		in.SlackChannel = "#" + strings.TrimPrefix(in.SlackChannel, "#")
	default:
		errs = append(errs, errors.New("Slack channel must be a channel name like #sec-news, or a channel ID like C0123456789"))
	}
	return errors.Join(errs...)
}

const channelColumns = `
	c.id, c.name, c.slack_channel,
	coalesce((SELECT array_agg(sec.name ORDER BY sec.sort, sec.name) FROM sections sec
	          WHERE sec.notification_channel_id = c.id), '{}'),
	coalesce(last.status, ''), coalesce(last.last_error, ''), coalesce(last.sent_at, last.created_at)
	FROM notification_channels c
	LEFT JOIN LATERAL (
		SELECT status, last_error, sent_at, created_at FROM slack_outbox
		WHERE channel_id = c.id AND attempts > 0 ORDER BY id DESC LIMIT 1
	) last ON true`

func (n *Notifier) Channels(ctx context.Context) ([]Channel, error) {
	rows, err := n.pool.Query(ctx, "SELECT "+channelColumns+" ORDER BY lower(c.name)")
	if err != nil {
		return nil, fmt.Errorf("list notification channels: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Channel])
}

func (n *Notifier) Channel(ctx context.Context, id int64) (Channel, error) {
	rows, err := n.pool.Query(ctx, "SELECT "+channelColumns+" WHERE c.id = $1", id)
	if err != nil {
		return Channel{}, fmt.Errorf("get notification channel: %w", err)
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Channel])
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, ErrChannelNotFound
	}
	return c, err
}

func (n *Notifier) CreateChannel(ctx context.Context, in ChannelInput, actor audit.Actor) (int64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	var id int64
	err := pgx.BeginFunc(ctx, n.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "INSERT INTO notification_channels (name, slack_channel) VALUES ($1, $2) RETURNING id",
			in.Name, in.SlackChannel).Scan(&id)
		if err != nil {
			return mapChannelError(err)
		}
		return audit.Record(ctx, tx, actor.Entry("channel.created", channelTarget(id),
			map[string]any{"name": in.Name, "slack_channel": in.SlackChannel}))
	})
	return id, err
}

func (n *Notifier) UpdateChannel(ctx context.Context, id int64, in ChannelInput, actor audit.Actor) error {
	if err := in.Validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, n.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE notification_channels SET name = $2, slack_channel = $3 WHERE id = $1",
			id, in.Name, in.SlackChannel)
		if err != nil {
			return mapChannelError(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrChannelNotFound
		}
		return audit.Record(ctx, tx, actor.Entry("channel.updated", channelTarget(id),
			map[string]any{"name": in.Name, "slack_channel": in.SlackChannel}))
	})
}

func (n *Notifier) DeleteChannel(ctx context.Context, id int64, actor audit.Actor) error {
	return pgx.BeginFunc(ctx, n.pool, func(tx pgx.Tx) error {
		var name string
		err := tx.QueryRow(ctx, "DELETE FROM notification_channels WHERE id = $1 RETURNING name", id).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChannelNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Entry("channel.deleted", channelTarget(id), map[string]any{"name": name}))
	})
}

func (n *Notifier) Test(ctx context.Context, id int64) error {
	if n.slack == nil {
		return ErrDisabled
	}
	c, err := n.Channel(ctx, id)
	if err != nil {
		return err
	}
	branding, err := n.settings.Branding(ctx)
	if err != nil {
		return err
	}
	_, _, err = n.slack.Post(ctx, Message{
		Channel: c.SlackChannel,
		Text:    fmt.Sprintf("Test message from %s: alerts for the notification channel %q will show up here.", escape(branding.DisplayName), escape(c.Name)),
	})
	return err
}

func channelTarget(id int64) string { return fmt.Sprintf("channel:%d", id) }

func mapChannelError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicateChannel
	}
	return err
}
