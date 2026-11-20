package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/secretbox"
)

var (
	ErrNotConfigured     = errors.New("no model is configured for this stage")
	ErrRefused           = errors.New("every model refused")
	ErrDuplicateProvider = errors.New("a provider with this name already exists")
	ErrDuplicateModel    = errors.New("this provider already has that model")
)

type ProviderInfo struct {
	ID          int64
	Name        string
	Type        string
	BaseURL     string
	APIKeyLast4 string
	Enabled     bool
}

type Model struct {
	ID                 int64
	ProviderID         int64
	ProviderName       string
	ProviderType       string
	ModelID            string
	SupportsJSONSchema bool
	Enabled            bool
}

func (m Model) Label() string { return m.ProviderName + " / " + m.ModelID }

type ProviderFactory func(ctx context.Context, typ, baseURL, apiKey string) (Provider, error)

type Store struct {
	pool        *pgxpool.Pool
	box         *secretbox.Box
	logger      *slog.Logger
	newProvider ProviderFactory
}

func NewStore(pool *pgxpool.Pool, box *secretbox.Box, logger *slog.Logger, factory ProviderFactory) *Store {
	if factory == nil {
		factory = NewProvider
	}
	return &Store{pool: pool, box: box, logger: logger, newProvider: factory}
}

type ProviderInput struct {
	Name    string
	Type    string
	BaseURL string
	APIKey  string
}

func (in *ProviderInput) Validate() error {
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.APIKey = strings.TrimSpace(in.APIKey)
	var errs []error
	if in.Name == "" || len(in.Name) > 60 {
		errs = append(errs, errors.New("name must have 1 to 60 characters"))
	}
	if !slices.Contains(Types, in.Type) {
		errs = append(errs, errors.New("pick a provider type"))
	}
	if in.Type == TypeOpenAICompatible {
		if u, err := url.Parse(in.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("OpenAI-compatible providers need a base URL, e.g. https://openrouter.ai/api/v1"))
		}
	} else {
		in.BaseURL = ""
	}
	if in.APIKey == "" {
		errs = append(errs, errors.New("the API key is required"))
	}
	return errors.Join(errs...)
}

func (s *Store) Providers(ctx context.Context) ([]ProviderInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, type, base_url, api_key_last4, enabled FROM llm_providers ORDER BY lower(name)`)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ProviderInfo])
}

func (s *Store) CreateProvider(ctx context.Context, in ProviderInput, actor audit.Actor) (int64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	last4 := in.APIKey
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO llm_providers (name, type, base_url, api_key_enc, api_key_last4)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			in.Name, in.Type, in.BaseURL, s.box.Seal([]byte(in.APIKey)), last4).Scan(&id)
		if isUniqueViolation(err) {
			return ErrDuplicateProvider
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Entry("llm.provider_created", fmt.Sprintf("llm_provider:%d", id),
			map[string]any{"name": in.Name, "type": in.Type}))
	})
	return id, err
}

func (s *Store) DeleteProvider(ctx context.Context, id int64, actor audit.Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, "DELETE FROM llm_providers WHERE id = $1 RETURNING name", id).Scan(&name); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Entry("llm.provider_deleted", fmt.Sprintf("llm_provider:%d", id),
			map[string]any{"name": name}))
	})
}

type ModelInput struct {
	ProviderID         int64
	ModelID            string
	SupportsJSONSchema bool
}

func (in *ModelInput) Validate() error {
	in.ModelID = strings.TrimSpace(in.ModelID)
	var errs []error
	if in.ProviderID == 0 {
		errs = append(errs, errors.New("pick a provider"))
	}
	if in.ModelID == "" || len(in.ModelID) > 100 {
		errs = append(errs, errors.New("model id must have 1 to 100 characters"))
	}
	return errors.Join(errs...)
}

const modelColumns = `m.id, m.provider_id, p.name, p.type, m.model_id, m.supports_json_schema, m.enabled AND p.enabled`

func (s *Store) Models(ctx context.Context) ([]Model, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+modelColumns+`
		FROM llm_models m JOIN llm_providers p ON p.id = m.provider_id
		ORDER BY lower(p.name), m.model_id`)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Model])
}

func (s *Store) CreateModel(ctx context.Context, in ModelInput, actor audit.Actor) (int64, error) {
	if err := in.Validate(); err != nil {
		return 0, err
	}
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO llm_models (provider_id, model_id, supports_json_schema)
			VALUES ($1, $2, $3) RETURNING id`,
			in.ProviderID, in.ModelID, in.SupportsJSONSchema).Scan(&id)
		if isUniqueViolation(err) {
			return ErrDuplicateModel
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Entry("llm.model_created", fmt.Sprintf("llm_model:%d", id),
			map[string]any{"model": in.ModelID}))
	})
	return id, err
}

func (s *Store) DeleteModel(ctx context.Context, id int64, actor audit.Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var model string
		if err := tx.QueryRow(ctx, "DELETE FROM llm_models WHERE id = $1 RETURNING model_id", id).Scan(&model); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actor.Entry("llm.model_deleted", fmt.Sprintf("llm_model:%d", id),
			map[string]any{"model": model}))
	})
}

func (s *Store) Generate(ctx context.Context, stage string, modelIDs []int64, effort Effort, req Request) (Response, Model, error) {
	if len(modelIDs) == 0 {
		return Response{}, Model{}, ErrNotConfigured
	}
	var (
		lastErr  error
		refusals int
	)
	for _, id := range modelIDs {
		m, baseURL, key, err := s.loadModel(ctx, id)
		if err != nil {
			lastErr = err
			continue
		}
		provider, err := s.newProvider(ctx, m.ProviderType, baseURL, key)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", m.Label(), err)
			continue
		}
		r := req
		r.Effort = effort
		r.UseSchema = m.SupportsJSONSchema
		start := time.Now()
		resp, err := provider.Generate(ctx, m.ModelID, r)
		s.logCall(ctx, stage, m, resp, err, time.Since(start))
		switch {
		case err != nil:
			lastErr = fmt.Errorf("%s: %w", m.Label(), err)
		case resp.Outcome == OutcomeRefusal:
			refusals++
			lastErr = fmt.Errorf("%s refused: %s", m.Label(), resp.Detail)
		case resp.Outcome != OutcomeOK:
			lastErr = fmt.Errorf("%s: %s (%s)", m.Label(), resp.Outcome, resp.Detail)
		default:
			return resp, m, nil
		}
		if ctx.Err() != nil {
			return Response{}, Model{}, ctx.Err()
		}
	}
	if refusals == len(modelIDs) {
		return Response{}, Model{}, fmt.Errorf("%w: %v", ErrRefused, lastErr)
	}
	if lastErr == nil {
		lastErr = ErrNotConfigured
	}
	return Response{}, Model{}, lastErr
}

func (s *Store) loadModel(ctx context.Context, id int64) (Model, string, string, error) {
	var (
		m       Model
		baseURL string
		sealed  []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT `+modelColumns+`, p.base_url, p.api_key_enc
		FROM llm_models m JOIN llm_providers p ON p.id = m.provider_id WHERE m.id = $1`, id).
		Scan(&m.ID, &m.ProviderID, &m.ProviderName, &m.ProviderType, &m.ModelID,
			&m.SupportsJSONSchema, &m.Enabled, &baseURL, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Model{}, "", "", fmt.Errorf("model %d no longer exists", id)
	}
	if err != nil {
		return Model{}, "", "", err
	}
	if !m.Enabled {
		return Model{}, "", "", fmt.Errorf("%s is disabled", m.Label())
	}
	key, err := s.box.Open(sealed)
	if err != nil {
		return Model{}, "", "", fmt.Errorf("%s: %w", m.Label(), err)
	}
	return m, baseURL, string(key), nil
}

func (s *Store) logCall(ctx context.Context, stage string, m Model, resp Response, callErr error, latency time.Duration) {
	outcome, errText := resp.Outcome, ""
	if callErr != nil {
		outcome, errText = OutcomeError, callErr.Error()
	} else if resp.Detail != "" {
		errText = resp.Detail
	}
	if len(errText) > 500 {
		errText = errText[:500]
	}
	_, err := s.pool.Exec(context.WithoutCancel(ctx), `
		INSERT INTO llm_calls (stage, provider_id, model_id, input_tokens, output_tokens, cache_read_tokens,
			latency_ms, outcome, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		stage, m.ProviderID, m.ModelID, resp.InputTokens, resp.OutputTokens, resp.CacheReadTokens,
		latency.Milliseconds(), string(outcome), errText)
	if err != nil {
		s.logger.Error("log llm call", "err", err)
	}
	if callErr != nil || resp.Outcome != OutcomeOK {
		s.logger.Warn("llm call failed", "stage", stage, "model", m.Label(), "outcome", outcome, "error", errText)
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
