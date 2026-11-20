package discuss

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

var (
	ErrNotFound   = errors.New("not found, or already expired")
	ErrForbidden  = errors.New("not allowed")
	ErrTooFast    = errors.New("slow down a little and try again in a few minutes")
	ErrEmpty      = errors.New("write something first")
	ErrTooLong    = errors.New("comments are limited to 5,000 characters")
	ErrEditWindow = errors.New("comments can only be edited in the first 15 minutes")
)

const (
	maxCommentChars   = 5000
	editWindow        = 15 * time.Minute
	commentsPerWindow = 10
	commentWindow     = 10 * time.Minute
	votesPerMinute    = 60
)

type Comment struct {
	ID        int64
	ParentID  *int64
	UserID    *int64
	Author    string
	Body      string
	Deleted   bool
	Points    int
	CreatedAt time.Time
	EditedAt  *time.Time
	MyVote    int
	Replies   []*Comment
}

func (c *Comment) CanEdit(userID int64) bool {
	return !c.Deleted && c.UserID != nil && *c.UserID == userID && time.Since(c.CreatedAt) < editWindow
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Thread(ctx context.Context, storyID, viewerID int64) ([]*Comment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.parent_id, c.user_id, coalesce(u.nickname, '[deleted]'), c.body_md, c.deleted, c.points,
		       c.created_at, c.edited_at, coalesce(v.value, 0)
		FROM comments c
		LEFT JOIN users u ON u.id = c.user_id
		LEFT JOIN comment_votes v ON v.comment_id = c.id AND v.user_id = $2
		WHERE c.story_id = $1
		ORDER BY c.created_at, c.id`, storyID, viewerID)
	if err != nil {
		return nil, fmt.Errorf("load comments: %w", err)
	}
	var all []*Comment
	byID := map[int64]*Comment{}
	var c Comment
	_, err = pgx.ForEachRow(rows, []any{&c.ID, &c.ParentID, &c.UserID, &c.Author, &c.Body, &c.Deleted, &c.Points,
		&c.CreatedAt, &c.EditedAt, &c.MyVote}, func() error {
		cc := c
		all = append(all, &cc)
		byID[cc.ID] = &cc
		return nil
	})
	if err != nil {
		return nil, err
	}
	var roots []*Comment
	for _, cc := range all {
		if cc.Deleted {
			cc.Author, cc.Body = "", ""
		}
		if cc.ParentID != nil && byID[*cc.ParentID] != nil {
			parent := byID[*cc.ParentID]
			parent.Replies = append(parent.Replies, cc)
		} else {
			roots = append(roots, cc)
		}
	}
	rank(roots)
	return roots, nil
}

// rank relies on a stable sort: comments are loaded oldest first, so ties stay in posting order.
func rank(level []*Comment) {
	slices.SortStableFunc(level, func(a, b *Comment) int { return b.Points - a.Points })
	for _, c := range level {
		rank(c.Replies)
	}
}

func (s *Store) Add(ctx context.Context, userID, storyID, parentID int64, body string) (int64, error) {
	body, err := cleanBody(body)
	if err != nil {
		return 0, err
	}
	var id int64
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var recent int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM comments WHERE user_id = $1 AND created_at > $2",
			userID, time.Now().Add(-commentWindow)).Scan(&recent); err != nil {
			return err
		}
		if recent >= commentsPerWindow {
			return ErrTooFast
		}
		var parent *int64
		if parentID != 0 {
			parent = &parentID
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO comments (story_id, parent_id, user_id, body_md)
			SELECT $1, $2, $3, $4
			WHERE EXISTS (SELECT 1 FROM stories WHERE id = $1 AND status = 'published' AND (expires_at IS NULL OR expires_at > now()))
			  AND ($2::bigint IS NULL OR EXISTS (SELECT 1 FROM comments WHERE id = $2 AND story_id = $1))
			RETURNING id`, storyID, parent, userID, body).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE stories SET comment_count = comment_count + 1 WHERE id = $1", storyID)
		return err
	})
	return id, err
}

func (s *Store) Get(ctx context.Context, commentID int64) (Comment, int64, error) {
	var (
		c       Comment
		storyID int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT c.id, c.parent_id, c.user_id, coalesce(u.nickname, '[deleted]'), c.body_md, c.deleted, c.points,
		       c.created_at, c.edited_at, c.story_id
		FROM comments c LEFT JOIN users u ON u.id = c.user_id WHERE c.id = $1`, commentID).
		Scan(&c.ID, &c.ParentID, &c.UserID, &c.Author, &c.Body, &c.Deleted, &c.Points, &c.CreatedAt, &c.EditedAt, &storyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Comment{}, 0, ErrNotFound
	}
	return c, storyID, err
}

func (s *Store) Edit(ctx context.Context, userID, commentID int64, body string) error {
	body, err := cleanBody(body)
	if err != nil {
		return err
	}
	c, _, err := s.Get(ctx, commentID)
	if err != nil {
		return err
	}
	if c.UserID == nil || *c.UserID != userID || c.Deleted {
		return ErrForbidden
	}
	if !c.CanEdit(userID) {
		return ErrEditWindow
	}
	_, err = s.pool.Exec(ctx, "UPDATE comments SET body_md = $2, edited_at = now() WHERE id = $1", commentID, body)
	return err
}

func (s *Store) Delete(ctx context.Context, actor audit.Actor, isAdmin bool, commentID int64) error {
	c, storyID, err := s.Get(ctx, commentID)
	if err != nil {
		return err
	}
	own := c.UserID != nil && *c.UserID == actor.ID
	if !own && !isAdmin {
		return ErrForbidden
	}
	if c.Deleted {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var hasReplies bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM comments WHERE parent_id = $1)", commentID).Scan(&hasReplies); err != nil {
			return err
		}
		if hasReplies {
			_, err = tx.Exec(ctx, "UPDATE comments SET deleted = true, body_md = '' WHERE id = $1", commentID)
		} else {
			_, err = tx.Exec(ctx, "DELETE FROM comments WHERE id = $1", commentID)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE stories SET comment_count = greatest(0, comment_count - 1) WHERE id = $1", storyID); err != nil {
			return err
		}
		if own {
			return nil
		}
		return audit.Record(ctx, tx, actor.Entry("comment.deleted", fmt.Sprintf("comment:%d", commentID),
			map[string]any{"story": storyID}))
	})
}

func (s *Store) Vote(ctx context.Context, userID, commentID int64, value int) (int, error) {
	if value < -1 || value > 1 {
		return 0, ErrForbidden
	}
	c, _, err := s.Get(ctx, commentID)
	if err != nil {
		return 0, err
	}
	if c.Deleted {
		return 0, ErrNotFound
	}
	if c.UserID != nil && *c.UserID == userID {
		return 0, ErrForbidden
	}
	var points int
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var recent int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM comment_votes WHERE user_id = $1 AND created_at > $2",
			userID, time.Now().Add(-time.Minute)).Scan(&recent); err != nil {
			return err
		}
		if recent >= votesPerMinute {
			return ErrTooFast
		}
		var old int
		err := tx.QueryRow(ctx, "SELECT value FROM comment_votes WHERE user_id = $1 AND comment_id = $2 FOR UPDATE",
			userID, commentID).Scan(&old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if value == 0 {
			_, err = tx.Exec(ctx, "DELETE FROM comment_votes WHERE user_id = $1 AND comment_id = $2", userID, commentID)
		} else {
			_, err = tx.Exec(ctx, `
				INSERT INTO comment_votes (user_id, comment_id, value) VALUES ($1, $2, $3)
				ON CONFLICT (user_id, comment_id) DO UPDATE SET value = $3, created_at = now()`,
				userID, commentID, value)
		}
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "UPDATE comments SET points = points + $2 WHERE id = $1 RETURNING points",
			commentID, value-old).Scan(&points)
	})
	return points, err
}

func cleanBody(body string) (string, error) {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	switch {
	case body == "":
		return "", ErrEmpty
	case utf8.RuneCountInString(body) > maxCommentChars:
		return "", ErrTooLong
	}
	return body, nil
}
