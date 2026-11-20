package discuss

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

type fixture struct {
	store        *Store
	pool         *pgxpool.Pool
	story        int64
	alice, bob   int64
	expiredStory int64
}

func setup(t *testing.T) fixture {
	pool := storetest.New(t)
	ctx := context.Background()
	f := fixture{store: New(pool), pool: pool}
	scan := func(dst *int64, sql string) {
		if err := pool.QueryRow(ctx, sql).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	scan(&f.alice, "INSERT INTO users (auth_mode, email, nickname) VALUES ('sso', 'alice@example.com', 'alice') RETURNING id")
	scan(&f.bob, "INSERT INTO users (auth_mode, email, nickname) VALUES ('sso', 'bob@example.com', 'bob') RETURNING id")
	scan(&f.story, "INSERT INTO stories (status, title, section_id, published_at) VALUES ('published', 's', 1, now()) RETURNING id")
	scan(&f.expiredStory, `INSERT INTO stories (status, title, section_id, published_at, expires_at)
		VALUES ('published', 'old', 1, now(), now() - interval '1 minute') RETURNING id`)
	return f
}

func (f fixture) commentCount(t *testing.T) int {
	var n int
	if err := f.pool.QueryRow(context.Background(), "SELECT comment_count FROM stories WHERE id = $1", f.story).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestThread(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	root, err := f.store.Add(ctx, f.alice, f.story, 0, "  First!  ")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := f.store.Add(ctx, f.bob, f.story, root, "A reply")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Add(ctx, f.bob, f.story, 0, "Second top-level"); err != nil {
		t.Fatal(err)
	}

	thread, err := f.store.Thread(ctx, f.story, f.bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 2 || thread[0].Body != "First!" || len(thread[0].Replies) != 1 || thread[0].Replies[0].ID != reply {
		t.Fatalf("thread shape is wrong: %+v", thread)
	}
	if thread[0].Author != "alice" || thread[0].Replies[0].Author != "bob" {
		t.Errorf("authors = %s, %s", thread[0].Author, thread[0].Replies[0].Author)
	}
	if f.commentCount(t) != 3 {
		t.Errorf("comment_count = %d, want 3", f.commentCount(t))
	}

	if err := f.store.Delete(ctx, audit.Actor{ID: f.bob}, false, root); !errors.Is(err, ErrForbidden) {
		t.Errorf("deleting someone else's comment: err = %v", err)
	}
	if err := f.store.Delete(ctx, audit.Actor{ID: f.alice}, false, root); err != nil {
		t.Fatal(err)
	}
	thread, _ = f.store.Thread(ctx, f.story, f.bob)
	if !thread[0].Deleted || thread[0].Body != "" || thread[0].Author != "" || len(thread[0].Replies) != 1 {
		t.Errorf("deleted parent: %+v", thread[0])
	}
	if err := f.store.Delete(ctx, audit.Actor{ID: f.alice, Label: "admin"}, true, reply); err != nil {
		t.Fatalf("admin deleting a reply: %v", err)
	}
	var audited int
	f.pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action = 'comment.deleted'").Scan(&audited)
	if audited != 1 {
		t.Errorf("admin deletion audited %d times, want 1", audited)
	}
	if f.commentCount(t) != 1 {
		t.Errorf("comment_count after deletions = %d, want 1", f.commentCount(t))
	}
}

func TestAddRules(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	if _, err := f.store.Add(ctx, f.alice, f.story, 0, "   "); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty: err = %v", err)
	}
	if _, err := f.store.Add(ctx, f.alice, f.story, 0, strings.Repeat("x", 5001)); !errors.Is(err, ErrTooLong) {
		t.Errorf("too long: err = %v", err)
	}
	if _, err := f.store.Add(ctx, f.alice, f.expiredStory, 0, "hi"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired story: err = %v", err)
	}
	other, _ := f.store.Add(ctx, f.alice, f.story, 0, "on the right story")
	if _, err := f.store.Add(ctx, f.alice, f.expiredStory, other, "reply across stories"); !errors.Is(err, ErrNotFound) {
		t.Errorf("parent from another story: err = %v", err)
	}
	for range 9 {
		if _, err := f.store.Add(ctx, f.alice, f.story, 0, "spam"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.Add(ctx, f.alice, f.story, 0, "one too many"); !errors.Is(err, ErrTooFast) {
		t.Errorf("11th comment in 10 minutes: err = %v", err)
	}
}

func TestEdit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id, _ := f.store.Add(ctx, f.alice, f.story, 0, "typo")
	if err := f.store.Edit(ctx, f.bob, id, "hijack"); !errors.Is(err, ErrForbidden) {
		t.Errorf("editing someone else's comment: err = %v", err)
	}
	if err := f.store.Edit(ctx, f.alice, id, "fixed"); err != nil {
		t.Fatal(err)
	}
	c, _, _ := f.store.Get(ctx, id)
	if c.Body != "fixed" || c.EditedAt == nil {
		t.Errorf("after edit: %+v", c)
	}
	storetest.MustExec(t, f.pool, "UPDATE comments SET created_at = now() - interval '16 minutes' WHERE id = $1", id)
	if err := f.store.Edit(ctx, f.alice, id, "too late"); !errors.Is(err, ErrEditWindow) {
		t.Errorf("edit after 15 minutes: err = %v", err)
	}
}

func TestVotes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var carol int64
	if err := f.pool.QueryRow(ctx, "INSERT INTO users (auth_mode, email, nickname) VALUES ('sso', 'carol@example.com', 'carol') RETURNING id").Scan(&carol); err != nil {
		t.Fatal(err)
	}
	c, _ := f.store.Add(ctx, f.alice, f.story, 0, "alice's comment")

	steps := []struct {
		user  int64
		value int
		want  int
	}{
		{f.bob, 1, 1},
		{f.bob, 1, 1},
		{carol, 1, 2},
		{f.bob, -1, 0},
		{f.bob, 0, 1},
	}
	for i, s := range steps {
		got, err := f.store.Vote(ctx, s.user, c, s.value)
		if err != nil || got != s.want {
			t.Fatalf("step %d: points = %d, err = %v, want %d", i, got, err, s.want)
		}
	}
	if _, err := f.store.Vote(ctx, f.alice, c, 1); !errors.Is(err, ErrForbidden) {
		t.Errorf("voting on your own comment: err = %v", err)
	}
	if _, err := f.store.Vote(ctx, f.bob, c, 2); !errors.Is(err, ErrForbidden) {
		t.Errorf("vote of 2: err = %v", err)
	}
	thread, _ := f.store.Thread(ctx, f.story, carol)
	if thread[0].MyVote != 1 || thread[0].Points != 1 {
		t.Errorf("thread as carol: %+v", thread[0])
	}
}

func TestThreadRankedByVotes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	older, _ := f.store.Add(ctx, f.alice, f.story, 0, "older, no votes")
	tied, _ := f.store.Add(ctx, f.alice, f.story, 0, "newer, no votes")
	popular, _ := f.store.Add(ctx, f.alice, f.story, 0, "newest, upvoted")
	disliked, _ := f.store.Add(ctx, f.alice, f.story, 0, "downvoted")
	f.store.Vote(ctx, f.bob, popular, 1)
	f.store.Vote(ctx, f.bob, disliked, -1)

	r1, _ := f.store.Add(ctx, f.bob, f.story, older, "first reply")
	r2, _ := f.store.Add(ctx, f.bob, f.story, older, "second reply, upvoted")
	f.store.Vote(ctx, f.alice, r2, 1)

	thread, err := f.store.Thread(ctx, f.story, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, c := range thread {
		got = append(got, c.ID)
	}
	if want := []int64{popular, older, tied, disliked}; !slices.Equal(got, want) {
		t.Errorf("top level = %v, want %v (votes first, then oldest)", got, want)
	}
	if replies := thread[1].Replies; len(replies) != 2 || replies[0].ID != r2 || replies[1].ID != r1 {
		t.Errorf("replies of the older comment are not ranked by votes")
	}
}
