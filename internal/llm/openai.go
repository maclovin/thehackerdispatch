package llm

import (
	"context"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

type openAIProvider struct {
	client openai.Client
}

func newOpenAI(apiKey, baseURL string) *openAIProvider {
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithRequestTimeout(5 * time.Minute)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &openAIProvider{client: openai.NewClient(opts...)}
}

func (p *openAIProvider) Generate(ctx context.Context, model string, req Request) (Response, error) {
	params := openai.ChatCompletionNewParams{
		Model: shared.ChatModel(model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(fullInstructions(req)),
			openai.UserMessage(req.Input),
		},
		MaxCompletionTokens: openai.Int(req.MaxTokens),
	}
	if req.Effort != EffortDefault {
		params.ReasoningEffort = shared.ReasoningEffort(req.Effort)
	}
	if req.UseSchema {
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   req.SchemaName,
					Schema: req.Schema,
					Strict: openai.Bool(true),
				},
			},
		}
	} else {
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	}

	completion, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{Outcome: OutcomeError}, err
	}
	cached := completion.Usage.PromptTokensDetails.CachedTokens
	resp := Response{
		InputTokens:     completion.Usage.PromptTokens - cached,
		OutputTokens:    completion.Usage.CompletionTokens,
		CacheReadTokens: cached,
	}
	if len(completion.Choices) == 0 {
		resp.Outcome = OutcomeError
		return resp, errEmptyResponse
	}
	choice := completion.Choices[0]
	switch {
	case choice.Message.Refusal != "":
		resp.Outcome = OutcomeRefusal
		resp.Detail = choice.Message.Refusal
		return resp, nil
	case choice.FinishReason == "content_filter":
		resp.Outcome = OutcomeRefusal
		resp.Detail = "content filter"
		return resp, nil
	case choice.FinishReason == "length":
		resp.Outcome = OutcomeTruncated
		resp.Detail = "max tokens reached"
		return resp, nil
	}
	resp.JSON, err = extractJSON(choice.Message.Content)
	if err != nil {
		resp.Outcome = OutcomeError
		return resp, err
	}
	resp.Outcome = OutcomeOK
	return resp, nil
}
