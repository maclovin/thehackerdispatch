package llm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/maclovin/thehackerdispatch/internal/audit"
	"github.com/maclovin/thehackerdispatch/internal/secretbox"
	"github.com/maclovin/thehackerdispatch/internal/store/storetest"
)

type fakeProvider struct{ key string }

func (f fakeProvider) Generate(_ context.Context, model string, req Request) (Response, error) {
	switch {
	case strings.HasPrefix(model, "ok"):
		return Response{Outcome: OutcomeOK, JSON: []byte(`{"key":"` + f.key + `"}`), InputTokens: 1000000, OutputTokens: 100000}, nil
	case strings.HasPrefix(model, "refuse"):
		return Response{Outcome: OutcomeRefusal, Detail: "cyber"}, nil
	default:
		return Response{Outcome: OutcomeError}, errors.New("upstream 500")
	}
}

func newTestStore(t *testing.T) *Store {
	pool := storetest.New(t)
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(pool, box, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(_ context.Context, _, _, key string) (Provider, error) { return fakeProvider{key: key}, nil })
}

var admin = audit.Actor{Label: "test"}

func addModels(t *testing.T, s *Store, names ...string) []int64 {
	ctx := context.Background()
	pid, err := s.CreateProvider(ctx, ProviderInput{Name: "P", Type: TypeOpenAI, APIKey: "sk-test-1234"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, n := range names {
		id, err := s.CreateModel(ctx, ModelInput{ProviderID: pid, ModelID: n, SupportsJSONSchema: true}, admin)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestProviderKeyIsEncrypted(t *testing.T) {
	s := newTestStore(t)
	addModels(t, s)
	var (
		sealed []byte
		last4  string
	)
	if err := s.pool.QueryRow(context.Background(), "SELECT api_key_enc, api_key_last4 FROM llm_providers").Scan(&sealed, &last4); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk-test")) || last4 != "1234" {
		t.Errorf("stored key not encrypted, or wrong last4 %q", last4)
	}
}

func TestGenerateFallsBack(t *testing.T) {
	s := newTestStore(t)
	ids := addModels(t, s, "broken", "refuse-1", "ok-1")
	ctx := context.Background()

	resp, m, err := s.Generate(ctx, "curation", ids, EffortLow, Request{Input: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if m.ModelID != "ok-1" || string(resp.JSON) != `{"key":"sk-test-1234"}` {
		t.Errorf("answered by %s with %s", m.ModelID, resp.JSON)
	}

	rows, err := s.pool.Query(ctx, "SELECT model_id, outcome FROM llm_calls ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	for rows.Next() {
		var model, outcome string
		if err := rows.Scan(&model, &outcome); err != nil {
			t.Fatal(err)
		}
		log = append(log, model+":"+outcome)
	}
	if got := strings.Join(log, " "); got != "broken:error refuse-1:refusal ok-1:ok" {
		t.Errorf("logged calls = %s", got)
	}
}

func TestGenerateErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.Generate(ctx, "curation", nil, EffortDefault, Request{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no models: err = %v", err)
	}
	ids := addModels(t, s, "refuse-1", "refuse-2")
	if _, _, err := s.Generate(ctx, "curation", ids, EffortDefault, Request{}); !errors.Is(err, ErrRefused) {
		t.Errorf("all refused: err = %v", err)
	}
	broken := addModelsTo(t, s, "Q", "broken")
	if _, _, err := s.Generate(ctx, "curation", broken, EffortDefault, Request{}); err == nil || errors.Is(err, ErrRefused) {
		t.Errorf("all failed: err = %v", err)
	}
}

func addModelsTo(t *testing.T, s *Store, provider string, names ...string) []int64 {
	ctx := context.Background()
	pid, err := s.CreateProvider(ctx, ProviderInput{Name: provider, Type: TypeAnthropic, APIKey: "k"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, n := range names {
		id, err := s.CreateModel(ctx, ModelInput{ProviderID: pid, ModelID: n}, admin)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestExtractJSON(t *testing.T) {
	tests := map[string]string{
		`{"a":1}`:                        `{"a":1}`,
		"Sure!\n```json\n{\"a\":1}\n```": `{"a":1}`,
	}
	for in, want := range tests {
		got, err := extractJSON(in)
		if err != nil || string(got) != want {
			t.Errorf("extractJSON(%q) = %s, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "no json", "{broken"} {
		if _, err := extractJSON(bad); err == nil {
			t.Errorf("extractJSON(%q): want error", bad)
		}
	}
}

func TestProviderInputValidate(t *testing.T) {
	in := ProviderInput{Name: "OR", Type: TypeOpenAICompatible, APIKey: "k"}
	if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Errorf("compatible without base URL: err = %v", err)
	}
	in = ProviderInput{Name: "A", Type: TypeAnthropic, APIKey: "k", BaseURL: "https://ignored"}
	if err := in.Validate(); err != nil || in.BaseURL != "" {
		t.Errorf("anthropic: err = %v, base URL kept = %q", err, in.BaseURL)
	}
}
