package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/maclovin/thehackerdispatch/internal/audit"
)

func (s *Server) addAdminStoryRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.requireAdmin(h) }
	mux.Handle("GET /admin/stories", admin(s.handleStories))
	mux.Handle("POST /admin/stories/{id}/delete", admin(s.handleStoryDelete))
}

type storiesPage struct {
	Stories []boardStory
}

func (s *Server) handleStories(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
		SELECT s.id, s.title, sec.name, coalesce(l.url, ''), s.published_at, s.expires_at, s.priority = 'critical',
		       (SELECT count(*) FROM story_links WHERE story_id = s.id), s.comment_count
		FROM stories s
		JOIN sections sec ON sec.id = s.section_id
		LEFT JOIN story_links l ON l.story_id = s.id AND l.is_primary
		WHERE s.status = 'published' AND (s.expires_at IS NULL OR s.expires_at > now())
		ORDER BY s.published_at DESC, s.id DESC
		LIMIT $1`, boardLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[boardStory])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_stories", "Stories", storiesPage{Stories: list})
}

func (s *Server) handleStoryDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	user, _ := currentUser(r)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var title string
		if err := tx.QueryRow(ctx, "DELETE FROM stories WHERE id = $1 RETURNING title", id).Scan(&title); err != nil {
			return err
		}
		return audit.Record(ctx, tx, user.Actor().Entry("story.deleted", fmt.Sprintf("story:%d", id), map[string]any{"title": title}))
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.renderError(w, r, http.StatusNotFound, "This story doesn't exist, or it has expired.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/admin/stories")
}
