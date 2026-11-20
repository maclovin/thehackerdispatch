package llm

import (
	"context"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type anthropicProvider struct {
	client anthropic.Client
}

func newAnthropic(apiKey string) *anthropicProvider {
	return &anthropicProvider{client: anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithRequestTimeout(5*time.Minute),
	)}
}

func (p *anthropicProvider) Generate(ctx context.Context, model string, req Request) (Response, error) {
	system := []anthropic.TextBlockParam{{Text: req.Instructions}}
	if req.Context != "" {
		system = append(system, anthropic.TextBlockParam{Text: req.Context})
	}
	if !req.UseSchema {
		system = []anthropic.TextBlockParam{{Text: fullInstructions(req)}}
	}
	system[len(system)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: req.MaxTokens,
		System:    system,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(req.Input))},
	}
	if req.Effort != EffortDefault {
		params.OutputConfig.Effort = anthropic.OutputConfigEffort(req.Effort)
	}
	if req.UseSchema {
		params.OutputConfig.Format = anthropic.JSONOutputFormatParam{Schema: req.Schema}
	}

	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return Response{Outcome: OutcomeError}, err
	}
	resp := Response{
		InputTokens:     msg.Usage.InputTokens + msg.Usage.CacheCreationInputTokens,
		OutputTokens:    msg.Usage.OutputTokens,
		CacheReadTokens: msg.Usage.CacheReadInputTokens,
	}
	switch msg.StopReason {
	case anthropic.StopReasonRefusal:
		resp.Outcome = OutcomeRefusal
		resp.Detail = strings.TrimSpace(string(msg.StopDetails.Category) + " " + msg.StopDetails.Explanation)
		return resp, nil
	case anthropic.StopReasonMaxTokens:
		resp.Outcome = OutcomeTruncated
		resp.Detail = "max tokens reached"
		return resp, nil
	}

	var text strings.Builder
	for _, block := range msg.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	resp.JSON, err = extractJSON(text.String())
	if err != nil {
		resp.Outcome = OutcomeError
		return resp, err
	}
	resp.Outcome = OutcomeOK
	return resp, nil
}
