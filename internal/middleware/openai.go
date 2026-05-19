//go:build openai

package middleware

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/history"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
)

// OpenAITranslator is the Chat Completions–backed Translator. It is built
// only when the `openai` build tag is set so the default orchestrator binary
// doesn't pull in the SDK. The same struct also serves DeepSeek: the factory
// flips BaseURL to https://api.deepseek.com/v1 and the model to deepseek-chat.
type OpenAITranslator struct {
	client      openai.Client
	model       string
	temperature float64
	maxTokens   int64
	log         *slog.Logger
}

// OpenAIOptions is the constructor input for NewOpenAITranslator.
type OpenAIOptions struct {
	APIKey      string
	BaseURL     string  // empty → SDK default (api.openai.com); set for DeepSeek / Azure
	Model       string  // default "gpt-4o-mini"
	Temperature float64 // default 0.2
	MaxTokens   int     // default 4096
	Logger      *slog.Logger
}

// NewOpenAITranslator builds a Translator backed by the OpenAI Go SDK.
func NewOpenAITranslator(_ context.Context, opts OpenAIOptions) (*OpenAITranslator, error) {
	clientOpts := []option.RequestOption{option.WithAPIKey(opts.APIKey)}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}
	cli := openai.NewClient(clientOpts...)

	model := opts.Model
	if model == "" {
		model = "gpt-4o-mini"
	}
	temp := opts.Temperature
	if temp == 0 {
		temp = 0.2
	}
	maxTok := int64(opts.MaxTokens)
	if maxTok == 0 {
		maxTok = 4096
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &OpenAITranslator{
		client:      cli,
		model:       model,
		temperature: temp,
		maxTokens:   maxTok,
		log:         log,
	}, nil
}

// openaiTurn tracks conversation state across one Stream + zero or more
// Resume calls. After a tool call the assistant message (with tool_calls)
// is appended, then Resume appends a `tool` role message with the result
// JSON, and we re-open the stream.
type openaiTurn struct {
	t        *OpenAITranslator
	system   string
	messages []openai.ChatCompletionMessageParamUnion
	tools    []openai.ChatCompletionToolParam
}

// Stream implements Translator.
func (o *OpenAITranslator) Stream(ctx context.Context, in TurnInput) (<-chan AssistantEvent, ResumeFunc, error) {
	state := &openaiTurn{
		t:        o,
		system:   in.SystemPrompt,
		messages: openaiHistoryToMessages(in.SystemPrompt, in.History, in.UserText),
		tools:    toOpenAITools(in.Tools),
	}
	out := make(chan AssistantEvent, 16)
	go state.run(ctx, out)
	resume := func(rctx context.Context, r ToolResult) (<-chan AssistantEvent, error) {
		state.messages = append(state.messages, openaiToolResultMessage(r))
		next := make(chan AssistantEvent, 16)
		go state.run(rctx, next)
		return next, nil
	}
	return out, resume, nil
}

func (s *openaiTurn) run(ctx context.Context, out chan<- AssistantEvent) {
	defer close(out)

	params := openai.ChatCompletionNewParams{
		Model:       shared.ChatModel(s.t.model),
		Messages:    s.messages,
		Temperature: openai.Float(s.t.temperature),
		MaxTokens:   openai.Int(s.t.maxTokens),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}
	if len(s.tools) > 0 {
		params.Tools = s.tools
	}

	stream := s.t.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	// Aggregate tool-call deltas across chunks: the SDK emits one tool_call
	// per output, but its `name` arrives on the first chunk and `arguments`
	// streams in fragments keyed by Index.
	type pendingCall struct {
		id   string
		name string
		args []byte
	}
	pending := map[int64]*pendingCall{}

	var (
		lastUsage    openai.CompletionUsage
		haveUsage    bool
		finishReason string
		callOrder    []int64 // preserve insertion order so we resolve the first complete call
	)

	for stream.Next() {
		chunk := stream.Current()
		if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			lastUsage = chunk.Usage
			haveUsage = true
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				select {
				case out <- AssistantEvent{Text: choice.Delta.Content}:
				case <-ctx.Done():
					return
				}
			}
			for _, tcDelta := range choice.Delta.ToolCalls {
				p, ok := pending[tcDelta.Index]
				if !ok {
					p = &pendingCall{}
					pending[tcDelta.Index] = p
					callOrder = append(callOrder, tcDelta.Index)
				}
				if tcDelta.ID != "" {
					p.id = tcDelta.ID
				}
				if tcDelta.Function.Name != "" {
					p.name = tcDelta.Function.Name
				}
				if tcDelta.Function.Arguments != "" {
					p.args = append(p.args, tcDelta.Function.Arguments...)
				}
			}
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}
	if err := stream.Err(); err != nil {
		select {
		case out <- AssistantEvent{Err: err}:
		case <-ctx.Done():
		}
		return
	}

	// One tool call per turn — matches Gemini behavior. If the model
	// emitted multiple, we take the first; the rest are dropped, same as
	// the Gemini path. (Phase 4 handles one call per turn.)
	if finishReason == "tool_calls" && len(callOrder) > 0 {
		p := pending[callOrder[0]]
		callID := p.id
		if callID == "" {
			callID = event.NewID()
		}
		var args map[string]any
		if len(p.args) > 0 {
			_ = json.Unmarshal(p.args, &args) // tolerate empty / malformed; dispatch will validate
		}
		// Mirror the assistant tool-call into running history so the
		// next stage's `tool` message has a matching tool_call_id.
		s.messages = append(s.messages, openaiAssistantToolCallMessage(callID, p.name, string(p.args)))

		if haveUsage {
			select {
			case out <- AssistantEvent{Usage: openaiUsage(lastUsage)}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- AssistantEvent{ToolCall: &ToolCall{ID: callID, Tool: p.name, Args: args}}:
		case <-ctx.Done():
		}
		return
	}

	// Terminal stage.
	if finishReason == "" {
		finishReason = "stop"
	}
	final := &FinalMessage{FinishReason: finishReason}
	if haveUsage {
		final.Usage = openaiUsageValue(lastUsage)
	}
	select {
	case out <- AssistantEvent{FinalMessage: final}:
	case <-ctx.Done():
	}
}

func openaiUsageValue(u openai.CompletionUsage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens,
		CandidatesTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

func openaiUsage(u openai.CompletionUsage) *Usage {
	v := openaiUsageValue(u)
	return &v
}

// openaiHistoryToMessages converts the orchestrator's persisted Turn slice
// plus a system prompt and the new user message into the SDK's message slice.
// Tool legs are rebuilt by the Resume path, so missing tool history just means
// the model loses earlier context — safe degradation, matching the Gemini path.
func openaiHistoryToMessages(systemPrompt string, hist []history.Turn, userText string) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(hist)+2)
	if systemPrompt != "" {
		out = append(out, openai.SystemMessage(systemPrompt))
	}
	for _, t := range hist {
		var raw map[string]any
		if err := json.Unmarshal(t.Parts, &raw); err != nil {
			continue
		}
		text, _ := raw["text"].(string)
		if text == "" {
			continue
		}
		switch t.Role {
		case history.RoleUser:
			out = append(out, openai.UserMessage(text))
		case history.RoleAssistant:
			out = append(out, openai.AssistantMessage(text))
		}
	}
	out = append(out, openai.UserMessage(userText))
	return out
}

func openaiAssistantToolCallMessage(callID, name, argsJSON string) openai.ChatCompletionMessageParamUnion {
	return openai.ChatCompletionMessageParamUnion{
		OfAssistant: &openai.ChatCompletionAssistantMessageParam{
			ToolCalls: []openai.ChatCompletionMessageToolCallParam{{
				ID: callID,
				Function: openai.ChatCompletionMessageToolCallFunctionParam{
					Name:      name,
					Arguments: argsJSON,
				},
			}},
		},
	}
}

func openaiToolResultMessage(r ToolResult) openai.ChatCompletionMessageParamUnion {
	body, err := json.Marshal(r.Output)
	if err != nil {
		body = []byte(`{}`)
	}
	return openai.ToolMessage(string(body), r.CallID)
}
