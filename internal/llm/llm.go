package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	TypeAnthropic        = "anthropic"
	TypeOpenAI           = "openai"
	TypeOpenAICompatible = "openai-compatible"
)

var Types = []string{TypeAnthropic, TypeOpenAI, TypeOpenAICompatible}

var errEmptyResponse = errors.New("the model returned no answer")

type Effort string

const (
	EffortDefault Effort = ""
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
)

type Request struct {
	// Instructions and Context go first so providers can cache them; Input changes on every call.
	Instructions string
	Context      string
	Input        string
	// Schema must be strict: every property required and additionalProperties false.
	Schema     map[string]any
	SchemaName string
	Effort     Effort
	MaxTokens  int64
	UseSchema  bool
}

type Outcome string

const (
	OutcomeOK        Outcome = "ok"
	OutcomeRefusal   Outcome = "refusal"
	OutcomeTruncated Outcome = "truncated"
	OutcomeError     Outcome = "error"
)

type Response struct {
	JSON            json.RawMessage
	Outcome         Outcome
	Detail          string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}

type Provider interface {
	Generate(ctx context.Context, model string, req Request) (Response, error)
}

func NewProvider(ctx context.Context, typ, baseURL, apiKey string) (Provider, error) {
	switch typ {
	case TypeAnthropic:
		return newAnthropic(apiKey), nil
	case TypeOpenAI:
		return newOpenAI(apiKey, ""), nil
	case TypeOpenAICompatible:
		if baseURL == "" {
			return nil, errors.New("openai-compatible providers need a base URL")
		}
		return newOpenAI(apiKey, baseURL), nil
	default:
		return nil, fmt.Errorf("unknown provider type %q", typ)
	}
}

func fullInstructions(req Request) string {
	s := req.Instructions
	if req.Context != "" {
		s += "\n\n" + req.Context
	}
	if !req.UseSchema && req.Schema != nil {
		schema, _ := json.Marshal(req.Schema)
		s += "\n\nRespond with a single JSON object, and nothing else, that matches this JSON schema:\n" + string(schema)
	}
	return s
}

func extractJSON(text string) (json.RawMessage, error) {
	start := bytes.IndexByte([]byte(text), '{')
	end := bytes.LastIndexByte([]byte(text), '}')
	if start < 0 || end < start {
		return nil, errors.New("no JSON object in the response")
	}
	raw := json.RawMessage(text[start : end+1])
	if !json.Valid(raw) {
		return nil, errors.New("the response is not valid JSON")
	}
	return raw, nil
}
