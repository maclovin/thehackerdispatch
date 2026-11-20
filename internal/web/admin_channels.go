package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/maclovin/thehackerdispatch/internal/notify"
	"github.com/maclovin/thehackerdispatch/internal/sources"
)

func (s *Server) addAdminChannelRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.requireAdmin(h) }
	mux.Handle("GET /admin/channels", admin(s.handleChannels))
	mux.Handle("POST /admin/channels", admin(s.handleChannelCreate))
	mux.Handle("GET /admin/channels/{id}", admin(s.handleChannel))
	mux.Handle("POST /admin/channels/{id}", admin(s.handleChannelUpdate))
	mux.Handle("POST /admin/channels/{id}/test", admin(s.handleChannelTest))
	mux.Handle("POST /admin/channels/{id}/delete", admin(s.handleChannelDelete))
	mux.Handle("POST /admin/sections/{id}/channel", admin(s.handleSectionChannel))
}

type channelsPage struct {
	SlackEnabled bool
	Channels     []notify.Channel
	Input        notify.ChannelInput
	Errors       []string
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	s.renderChannels(w, r, http.StatusOK, channelsPage{})
}

func (s *Server) renderChannels(w http.ResponseWriter, r *http.Request, status int, page channelsPage) {
	var err error
	if page.Channels, err = s.Notify.Channels(r.Context()); err != nil {
		s.serverError(w, r, err)
		return
	}
	page.SlackEnabled = s.Notify.Enabled()
	s.render(w, r, status, "admin_channels", "Notification channels", page)
}

func (s *Server) handleChannelCreate(w http.ResponseWriter, r *http.Request) {
	in := channelInput(r)
	user, _ := currentUser(r)
	_, err := s.Notify.CreateChannel(r.Context(), in, user.Actor())
	if problems := channelProblems(err); problems != nil {
		s.renderChannels(w, r, http.StatusUnprocessableEntity, channelsPage{Input: in, Errors: problems})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/channels", http.StatusSeeOther)
}

type channelPage struct {
	Channel notify.Channel
	Input   notify.ChannelInput
	Errors  []string
}

func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request) {
	c, ok := s.channelFromPath(w, r)
	if !ok {
		return
	}
	in := notify.ChannelInput{Name: c.Name, SlackChannel: c.SlackChannel}
	s.render(w, r, http.StatusOK, "admin_channel", c.Name, channelPage{Channel: c, Input: in})
}

func (s *Server) handleChannelUpdate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.channelFromPath(w, r)
	if !ok {
		return
	}
	in := channelInput(r)
	user, _ := currentUser(r)
	err := s.Notify.UpdateChannel(r.Context(), c.ID, in, user.Actor())
	if problems := channelProblems(err); problems != nil {
		s.render(w, r, http.StatusUnprocessableEntity, "admin_channel", c.Name, channelPage{Channel: c, Input: in, Errors: problems})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/channels", http.StatusSeeOther)
}

func (s *Server) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	result := testResult{OK: true, Message: "message sent"}
	if err := s.Notify.Test(r.Context(), id); err != nil {
		result = testResult{Message: err.Error()}
	}
	s.renderFragment(w, http.StatusOK, "test_result", result)
}

func (s *Server) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	user, _ := currentUser(r)
	err := s.Notify.DeleteChannel(r.Context(), id, user.Actor())
	switch {
	case errors.Is(err, notify.ErrChannelNotFound):
		s.renderError(w, r, http.StatusNotFound, "Notification channel not found.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		redirect(w, r, "/admin/channels")
	}
}

func (s *Server) handleSectionChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	channel, _ := strconv.ParseInt(r.PostFormValue("channel_id"), 10, 64)
	user, _ := currentUser(r)
	err := s.Sources.SetSectionChannel(r.Context(), id, channel, user.Actor())
	switch {
	case errors.Is(err, sources.ErrSectionNotFound):
		s.renderError(w, r, http.StatusNotFound, "Section not found.")
	case errors.Is(err, sources.ErrUnknownChannel):
		s.renderSection(w, r, http.StatusUnprocessableEntity, nil, nil, []string{err.Error()})
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/admin/sections/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	}
}

func (s *Server) channelFromPath(w http.ResponseWriter, r *http.Request) (notify.Channel, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := s.Notify.Channel(r.Context(), id)
	if errors.Is(err, notify.ErrChannelNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Notification channel not found.")
		return c, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return c, false
	}
	return c, true
}

func channelInput(r *http.Request) notify.ChannelInput {
	return notify.ChannelInput{Name: r.PostFormValue("name"), SlackChannel: r.PostFormValue("slack_channel")}
}

func channelProblems(err error) []string {
	var invalid interface{ Unwrap() []error }
	switch {
	case err == nil:
		return nil
	case errors.Is(err, notify.ErrDuplicateChannel):
		return []string{err.Error()}
	case errors.As(err, &invalid):
		var problems []string
		for _, e := range invalid.Unwrap() {
			problems = append(problems, e.Error())
		}
		return problems
	}
	return nil
}
