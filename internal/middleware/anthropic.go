//go:build anthropic

package middleware

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/mattcheramie/nomaddev/internal/history"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicTranslator is the Anthropic Messages API–backed Translator. Built
// only when the `anthropic` build tag is set so the default orchestrator
// binary doesn't pull in the SDK.
type AnthropicTranslator struct {
	client      anthropic.Client
	model       string
	temperature float64
	maxTokens   int64
	log         *slog.Logger
}

// AnthropicOptions is the constructor input for NewAnthropicTranslator.
type AnthropicOptions struct {
	APIKey      string
	Model       string  // default "claude-sonnet-4-5"
	Temperature float64 // default 0.2
	MaxTokens   int     // default 4096
	Logger      *slog.Logger
}

// NewAnthropicTranslator builds a Translator backed by the Anthropic SDK.
func NewAnthropicTranslator(_ context.Context, opts AnthropicOptions) (*AnthropicTranslator, error) {
	cli := anthropic.NewClient(option.WithAPIKey(opts.APIKey))

	model := opts.Model
	if model == "" {
		model = anthropic.ModelClaudeSonnet4_5
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
	return &AnthropicTranslator{
		client:      cli,
		model:       model,
		temperature: temp,
		maxTokens:   maxTok,
		log:         log,
	}, nil
}

// anthropicTurn tracks conversation state across one Stream + zero or more
// Resume calls. After a tool_use the assistant turn is appended; Resume
// appends a user turn with a tool_result block and re-opens the stream.
type anthropicTurn struct {
	t        *AnthropicTranslator
	system   string
	tools    []anthropic.ToolUnionParam
	messages []anthropic.MessageParam
}

// Stream implements Translator.
func (a *AnthropicTranslator) Stream(ctx context.Context, in TurnInput) (<-chan AssistantEvent, ResumeFunc, error) {
	state := &anthropicTurn{
		t:        a,
		system:   in.SystemPrompt,
		tools:    toAnthropicTools(in.Tools),
		messages: anthropicHistoryToMessages(in.History, in.UserText),
	}
	out := make(chan AssistantEvent, 16)
	go state.run(ctx, out)
	resume := func(rctx context.Context, r ToolResult) (<-chan AssistantEvent, error) {
		body, err := json.Marshal(r.Output)
		if err != nil {
			body = []byte(`{}`)
		}
		state.messages = append(state.messages, anthropic.NewUserMessage(
			anthropic.NewToolResultBlock(r.CallID, string(body), r.Error != ""),
		))
		next := make(chan AssistantEvent, 16)
		go state.run(rctx, next)
		return next, nil
	}
	return out, resume, nil
}

func (s *anthropicTurn) run(ctx context.Context, out chan<- AssistantEvent) {
	defer close(out)

	params := anthropic.MessageNewParams{
		Model:       s.t.model,
		Messages:    s.messages,
		MaxTokens:   s.t.maxTokens,
		Temperature: anthropic.Float(s.t.temperature),
	}
	if s.system != "" {
		params.System = []anthropic.TextBlockParam{{Text: s.system}}
	}
	if len(s.tools) > 0 {
		params.Tools = s.tools
	}

	stream := s.t.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	// Track in-flight content blocks by index. The Messages API streams a
	// content_block_start (with type+id+name for tool_use), then a series
	// of content_block_delta events carrying text or partial JSON, then
	// content_block_stop. message_delta carries the stop_reason and usage.
	type block struct {
		kind       string // "text" | "tool_use" | "other"
		toolID     string
		toolName   string
		toolArgsRaw []byte
	}
	blocks := map[int64]*block{}

	var (
		stopReason   string
		usage        Usage
		haveUsage    bool
		toolCallID   string
		toolCallName string
		toolCallArgs []byte
	)

	for stream.Next() {
		ev := stream.Current()
		switch v := ev.AsAny().(type) {
		case anthropic.MessageStartEvent:
			// Initial input tokens land here. We'll overwrite/extend at
			// message_delta and message_stop, but record what we have.
			usage = Usage{
				PromptTokens: v.Message.Usage.InputTokens,
				TotalTokens:  v.Message.Usage.InputTokens + v.Message.Usage.OutputTokens,
			}
			haveUsage = true
		case anthropic.ContentBlockStartEvent:
			b := &block{kind: v.ContentBlock.Type}
			if v.ContentBlock.Type == "tool_use" {
				b.toolID = v.ContentBlock.ID
				b.toolName = v.ContentBlock.Name
			}
			blocks[v.Index] = b
		case anthropic.ContentBlockDeltaEvent:
			b, ok := blocks[v.Index]
			if !ok {
				continue
			}
			switch v.Delta.Type {
			case "text_delta":
				if v.Delta.Text != "" {
					select {
					case out <- AssistantEvent{Text: v.Delta.Text}:
					case <-ctx.Done():
						return
					}
				}
			case "input_json_delta":
				if v.Delta.PartialJSON != "" {
					b.toolArgsRaw = append(b.toolArgsRaw, v.Delta.PartialJSON...)
				}
			}
		case anthropic.ContentBlockStopEvent:
			b, ok := blocks[v.Index]
			if !ok {
				continue
			}
			// First completed tool_use wins — Phase 4 dispatches one per
			// turn, matching the Gemini path.
			if b.kind == "tool_use" && toolCallID == "" {
				toolCallID = b.toolID
				toolCallName = b.toolName
				toolCallArgs = b.toolArgsRaw
			}
		case anthropic.MessageDeltaEvent:
			stopReason = string(v.Delta.StopReason)
			// usage on message_delta is cumulative output tokens
			if v.Usage.OutputTokens > 0 {
				usage.CandidatesTokens = v.Usage.OutputTokens
				usage.TotalTokens = usage.PromptTokens + v.Usage.OutputTokens
				haveUsage = true
			}
		case anthropic.MessageStopEvent:
			// nothing more — fall through to post-loop handling
		}
	}
	if err := stream.Err(); err != nil {
		select {
		case out <- AssistantEvent{Err: err}:
		case <-ctx.Done():
		}
		return
	}

	if stopReason == "tool_use" && toolCallID != "" {
		var args map[string]any
		if len(toolCallArgs) > 0 {
			_ = json.Unmarshal(toolCallArgs, &args)
		}
		// Mirror the assistant tool_use into running history so the
		// following user turn (with the tool_result block) matches it.
		s.messages = append(s.messages, anthropic.NewAssistantMessage(
			anthropic.NewToolUseBlock(toolCallID, args, toolCallName),
		))

		if haveUsage {
			u := usage
			select {
			case out <- AssistantEvent{Usage: &u}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- AssistantEvent{ToolCall: &ToolCall{ID: toolCallID, Tool: toolCallName, Args: args}}:
		case <-ctx.Done():
		}
		return
	}

	if stopReason == "" {
		stopReason = "stop"
	}
	final := &FinalMessage{FinishReason: stopReason}
	if haveUsage {
		final.Usage = usage
	}
	select {
	case out <- AssistantEvent{FinalMessage: final}:
	case <-ctx.Done():
	}
}

// anthropicHistoryToMessages converts the orchestrator's persisted Turn
// slice plus the new user message into the SDK's MessageParam slice. Same
// "extract text field" degradation as the Gemini path — tool legs are
// rebuilt by Resume.
func anthropicHistoryToMessages(hist []history.Turn, userText string) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(hist)+1)
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
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(text)))
		case history.RoleAssistant:
			out = append(out, anthropic.NewAssistantMessage(anthropic.NewTextBlock(text)))
		}
	}
	out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(userText)))
	return out
}
