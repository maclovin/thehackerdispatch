package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/maclovin/thehackerdispatch/internal/llm"
	"github.com/maclovin/thehackerdispatch/internal/settings"
)

func (s *Server) addAdminLLMRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return s.requireAdmin(h) }
	mux.Handle("GET /admin/llm", admin(s.handleLLM))
	mux.Handle("POST /admin/llm/providers", admin(s.handleProviderCreate))
	mux.Handle("POST /admin/llm/providers/{id}/delete", admin(s.handleProviderDelete))
	mux.Handle("POST /admin/llm/models", admin(s.handleModelCreate))
	mux.Handle("POST /admin/llm/models/{id}/delete", admin(s.handleModelDelete))
	mux.Handle("POST /admin/llm/models/{id}/test", admin(s.handleModelTest))
	mux.Handle("POST /admin/llm/stages", admin(s.handleStagesSave))
}

const stageSlots = 3

type stageForm struct {
	Name   string
	Label  string
	Slots  []int64
	Effort string
}

type usageRow struct {
	Stage  string
	Calls  int
	Failed int
}

type llmPage struct {
	Providers []llm.ProviderInfo
	Models    []llm.Model
	Stages    []stageForm
	Usage     []usageRow
	Types     []string
	Efforts   []string
	Errors    []string
}

func (s *Server) handleLLM(w http.ResponseWriter, r *http.Request) {
	s.renderLLM(w, r, http.StatusOK, nil)
}

func (s *Server) renderLLM(w http.ResponseWriter, r *http.Request, status int, problems []string) {
	ctx := r.Context()
	providers, err := s.LLM.Providers(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	models, err := s.LLM.Models(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	stages, err := s.Settings.LLMStages(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT stage, count(*), count(*) FILTER (WHERE outcome <> 'ok')
		FROM llm_calls WHERE created_at > now() - interval '24 hours'
		GROUP BY stage ORDER BY stage`)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	usage, err := pgx.CollectRows(rows, pgx.RowToStructByPos[usageRow])
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "admin_llm", "LLM", llmPage{
		Providers: providers,
		Models:    models,
		Stages: []stageForm{
			newStageForm("curation", "Curation (decides what gets in)", stages.Curation),
			newStageForm("writing", "Writing (writes the posts; uses curation's models if empty)", stages.Writing),
		},
		Usage:   usage,
		Types:   llm.Types,
		Efforts: []string{"", "low", "medium", "high"},
		Errors:  problems,
	})
}

func newStageForm(name, label string, st settings.LLMStage) stageForm {
	f := stageForm{Name: name, Label: label, Slots: make([]int64, stageSlots), Effort: st.Effort}
	copy(f.Slots, st.ModelIDs)
	return f
}

func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	_, err := s.LLM.CreateProvider(r.Context(), llm.ProviderInput{
		Name:    r.PostFormValue("name"),
		Type:    r.PostFormValue("type"),
		BaseURL: r.PostFormValue("base_url"),
		APIKey:  r.PostFormValue("api_key"),
	}, user.Actor())
	s.afterLLMChange(w, r, err)
}

func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	user, _ := currentUser(r)
	s.afterLLMChange(w, r, s.LLM.DeleteProvider(r.Context(), id, user.Actor()))
}

func (s *Server) handleModelCreate(w http.ResponseWriter, r *http.Request) {
	providerID, _ := strconv.ParseInt(r.PostFormValue("provider_id"), 10, 64)
	user, _ := currentUser(r)
	_, err := s.LLM.CreateModel(r.Context(), llm.ModelInput{
		ProviderID:         providerID,
		ModelID:            r.PostFormValue("model_id"),
		SupportsJSONSchema: r.PostFormValue("supports_json_schema") == "on",
	}, user.Actor())
	s.afterLLMChange(w, r, err)
}

func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	user, _ := currentUser(r)
	s.afterLLMChange(w, r, s.LLM.DeleteModel(r.Context(), id, user.Actor()))
}

func (s *Server) handleStagesSave(w http.ResponseWriter, r *http.Request) {
	stage := func(name string) settings.LLMStage {
		st := settings.LLMStage{Effort: r.PostFormValue(name + "_effort")}
		for i := 1; i <= stageSlots; i++ {
			if id, err := strconv.ParseInt(r.PostFormValue(fmt.Sprintf("%s_model_%d", name, i)), 10, 64); err == nil && id > 0 {
				st.ModelIDs = append(st.ModelIDs, id)
			}
		}
		switch llm.Effort(st.Effort) {
		case llm.EffortDefault, llm.EffortLow, llm.EffortMedium, llm.EffortHigh:
		default:
			st.Effort = ""
		}
		return st
	}
	user, _ := currentUser(r)
	err := s.Settings.SetLLMStages(r.Context(), settings.LLMStages{Curation: stage("curation"), Writing: stage("writing")}, user.Actor())
	s.afterLLMChange(w, r, err)
}

func (s *Server) afterLLMChange(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		redirect(w, r, "/admin/llm")
		return
	}
	var joined interface{ Unwrap() []error }
	switch {
	case errors.As(err, &joined):
		var problems []string
		for _, e := range joined.Unwrap() {
			problems = append(problems, e.Error())
		}
		s.renderLLM(w, r, http.StatusUnprocessableEntity, problems)
	case errors.Is(err, pgx.ErrNoRows):
		s.renderLLM(w, r, http.StatusNotFound, []string{"That item no longer exists."})
	case errors.Is(err, llm.ErrDuplicateProvider), errors.Is(err, llm.ErrDuplicateModel):
		s.renderLLM(w, r, http.StatusUnprocessableEntity, []string{err.Error()})
	default:
		s.serverError(w, r, err)
	}
}

type testResult struct {
	OK      bool
	Message string
}

var healthSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"ok"},
	"properties":           map[string]any{"ok": map[string]any{"type": "boolean"}},
}

func (s *Server) handleModelTest(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	start := time.Now()
	resp, _, err := s.LLM.Generate(r.Context(), "test", []int64{id}, llm.EffortDefault, llm.Request{
		Instructions: "You are a health check for an API integration.",
		Input:        `Reply with {"ok": true}.`,
		Schema:       healthSchema,
		SchemaName:   "health_check",
		MaxTokens:    2048,
	})
	result := testResult{OK: err == nil}
	if err != nil {
		result.Message = err.Error()
	} else {
		result.Message = fmt.Sprintf("answered %s in %.1fs (%d input, %d output tokens)",
			resp.JSON, time.Since(start).Seconds(), resp.InputTokens+resp.CacheReadTokens, resp.OutputTokens)
	}
	s.renderFragment(w, http.StatusOK, "test_result", result)
}
