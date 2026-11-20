package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/maclovin/thehackerdispatch/internal/auth"
	"github.com/maclovin/thehackerdispatch/internal/discuss"
)

func (s *Server) addCommentRoutes(mux *http.ServeMux) {
	user := func(h http.HandlerFunc) http.Handler { return s.requireUser(h) }
	mux.Handle("POST /s/{id}/comments", user(s.handleCommentAdd))
	mux.Handle("GET /c/{id}/reply", user(s.handleCommentReplyPage))
	mux.Handle("GET /c/{id}/edit", user(s.handleCommentEditPage))
	mux.Handle("POST /c/{id}/edit", user(s.handleCommentEdit))
	mux.Handle("POST /c/{id}/delete", user(s.handleCommentDelete))
	mux.Handle("POST /c/{id}/vote", user(s.handleCommentVote))
}

type commentNode struct {
	ID        int64
	StoryID   int64
	Author    string
	Body      template.HTML
	Deleted   bool
	Edited    bool
	CreatedAt time.Time
	Vote      voteWidget
	CanEdit   bool
	CanDelete bool
	Replies   []commentNode
}

type voteWidget struct {
	CommentID int64
	Points    int
	MyVote    int
	CanVote   bool
	Error     string
}

func (v voteWidget) UpValue() int {
	if v.MyVote == 1 {
		return 0
	}
	return 1
}

func (v voteWidget) DownValue() int {
	if v.MyVote == -1 {
		return 0
	}
	return -1
}

func buildNodes(storyID int64, comments []*discuss.Comment, me auth.User) []commentNode {
	nodes := make([]commentNode, 0, len(comments))
	for _, c := range comments {
		own := c.UserID != nil && *c.UserID == me.ID
		nodes = append(nodes, commentNode{
			ID:        c.ID,
			StoryID:   storyID,
			Author:    c.Author,
			Body:      renderMarkdown(c.Body),
			Deleted:   c.Deleted,
			Edited:    c.EditedAt != nil,
			CreatedAt: c.CreatedAt,
			Vote:      voteWidget{CommentID: c.ID, Points: c.Points, MyVote: c.MyVote, CanVote: !own && !c.Deleted},
			CanEdit:   c.CanEdit(me.ID),
			CanDelete: !c.Deleted && (own || me.IsAdmin()),
			Replies:   buildNodes(storyID, c.Replies, me),
		})
	}
	return nodes
}

func commentError(err error) (string, bool) {
	for _, known := range []error{discuss.ErrEmpty, discuss.ErrTooLong, discuss.ErrTooFast, discuss.ErrEditWindow,
		discuss.ErrForbidden, discuss.ErrNotFound} {
		if errors.Is(err, known) {
			return known.Error(), true
		}
	}
	return "", false
}

func (s *Server) handleCommentAdd(w http.ResponseWriter, r *http.Request) {
	storyID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "This story doesn't exist, or it has expired.")
		return
	}
	me, _ := currentUser(r)
	parentID, _ := strconv.ParseInt(r.PostFormValue("parent"), 10, 64)
	body := r.PostFormValue("body")
	id, err := s.Discuss.Add(r.Context(), me.ID, storyID, parentID, body)
	if err != nil {
		msg, known := commentError(err)
		if !known {
			s.serverError(w, r, err)
			return
		}
		s.renderStory(w, r, storyID, http.StatusUnprocessableEntity, storyForm{Error: msg, Draft: body, ParentID: parentID})
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/s/%d#c%d", storyID, id), http.StatusSeeOther)
}

type commentPage struct {
	StoryID int64
	Comment commentNode
	Body    string
	Error   string
}

func (s *Server) loadComment(w http.ResponseWriter, r *http.Request) (discuss.Comment, int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		c, storyID, err := s.Discuss.Get(r.Context(), id)
		if err == nil {
			return c, storyID, true
		}
		if !errors.Is(err, discuss.ErrNotFound) {
			s.serverError(w, r, err)
			return discuss.Comment{}, 0, false
		}
	}
	s.renderError(w, r, http.StatusNotFound, "This comment doesn't exist, or it has expired.")
	return discuss.Comment{}, 0, false
}

func (s *Server) handleCommentReplyPage(w http.ResponseWriter, r *http.Request) {
	c, storyID, ok := s.loadComment(w, r)
	if !ok {
		return
	}
	me, _ := currentUser(r)
	node := buildNodes(storyID, []*discuss.Comment{&c}, me)[0]
	node.Replies = nil
	s.render(w, r, http.StatusOK, "comment_reply", "Reply", commentPage{StoryID: storyID, Comment: node})
}

func (s *Server) handleCommentEditPage(w http.ResponseWriter, r *http.Request) {
	c, storyID, ok := s.loadComment(w, r)
	if !ok {
		return
	}
	me, _ := currentUser(r)
	if !c.CanEdit(me.ID) {
		s.renderError(w, r, http.StatusForbidden, discuss.ErrEditWindow.Error()+".")
		return
	}
	node := buildNodes(storyID, []*discuss.Comment{&c}, me)[0]
	s.render(w, r, http.StatusOK, "comment_edit", "Edit comment", commentPage{StoryID: storyID, Comment: node, Body: c.Body})
}

func (s *Server) handleCommentEdit(w http.ResponseWriter, r *http.Request) {
	c, storyID, ok := s.loadComment(w, r)
	if !ok {
		return
	}
	me, _ := currentUser(r)
	body := r.PostFormValue("body")
	if err := s.Discuss.Edit(r.Context(), me.ID, c.ID, body); err != nil {
		msg, known := commentError(err)
		if !known {
			s.serverError(w, r, err)
			return
		}
		node := buildNodes(storyID, []*discuss.Comment{&c}, me)[0]
		s.render(w, r, http.StatusUnprocessableEntity, "comment_edit", "Edit comment",
			commentPage{StoryID: storyID, Comment: node, Body: body, Error: msg})
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/s/%d#c%d", storyID, c.ID), http.StatusSeeOther)
}

func (s *Server) handleCommentDelete(w http.ResponseWriter, r *http.Request) {
	c, storyID, ok := s.loadComment(w, r)
	if !ok {
		return
	}
	me, _ := currentUser(r)
	if err := s.Discuss.Delete(r.Context(), me.Actor(), me.IsAdmin(), c.ID); err != nil {
		if msg, known := commentError(err); known {
			s.renderError(w, r, http.StatusForbidden, msg)
			return
		}
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/s/%d#comments", storyID))
}

func (s *Server) handleCommentVote(w http.ResponseWriter, r *http.Request) {
	c, storyID, ok := s.loadComment(w, r)
	if !ok {
		return
	}
	me, _ := currentUser(r)
	value, _ := strconv.Atoi(r.PostFormValue("value"))
	points, err := s.Discuss.Vote(r.Context(), me.ID, c.ID, value)
	widget := voteWidget{CommentID: c.ID, Points: points, MyVote: value, CanVote: true}
	if err != nil {
		msg, known := commentError(err)
		if !known {
			s.serverError(w, r, err)
			return
		}
		widget = voteWidget{CommentID: c.ID, Points: c.Points, CanVote: true, Error: msg}
	}
	if r.Header.Get("HX-Request") == "true" {
		s.renderFragment(w, http.StatusOK, "vote", widget)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/s/%d#c%d", storyID, c.ID), http.StatusSeeOther)
}
