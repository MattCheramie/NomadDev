//go:build gemini

package middleware

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/history"

	"google.golang.org/genai"
)

// GeminiTranslator is the Google GenAI-backed Translator. It is built only
// when the `gemini` build tag is set so the default orchestrator binary
// doesn't pull in the SDK (and its grpc / protobuf transitives).
type GeminiTranslator struct {
	client      *genai.Client
	model       string
	temperature float32
	maxTokens   int32
	log         *slog.Logger
}

// GeminiOptions is the constructor input for NewGeminiTranslator.
type GeminiOptions struct {
	APIKey      string
	Model       string  // default "gemini-2.0-flash"
	Temperature float64 // default 0.2
	MaxTokens   int     // default 4096
	Logger      *slog.Logger
}

// NewGeminiTranslator builds a Translator backed by the Google GenAI SDK.
func NewGeminiTranslator(ctx context.Context, opts GeminiOptions) (*GeminiTranslator, error) {
	cli, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  opts.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	model := opts.Model
	if model == "" {
		model = "gemini-2.0-flash"
	}
	temp := float32(opts.Temperature)
	if opts.Temperature == 0 {
		temp = 0.2
	}
	maxTok := int32(opts.MaxTokens)
	if maxTok == 0 {
		maxTok = 4096
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &GeminiTranslator{
		client:      cli,
		model:       model,
		temperature: temp,
		maxTokens:   maxTok,
		log:         log,
	}, nil
}

// geminiTurn tracks the conversation state across one Stream + zero or more
// Resume calls. A FunctionCall ends a stage; Resume appends a
// FunctionResponse to `contents` and re-opens the stream.
type geminiTurn struct {
	t        *GeminiTranslator
	cfg      *genai.GenerateContentConfig
	contents []*genai.Content
}

// Stream implements Translator.
func (g *GeminiTranslator) Stream(ctx context.Context, in TurnInput) (<-chan AssistantEvent, ResumeFunc, error) {
	state := &geminiTurn{
		t:        g,
		cfg:      g.configFor(in),
		contents: historyToContents(in.History, in.UserText),
	}
	out := make(chan AssistantEvent, 16)
	go state.run(ctx, out)
	resume := func(rctx context.Context, r ToolResult) (<-chan AssistantEvent, error) {
		state.contents = append(state.contents, toolResponseContent(r))
		next := make(chan AssistantEvent, 16)
		go state.run(rctx, next)
		return next, nil
	}
	return out, resume, nil
}

func (g *GeminiTranslator) configFor(in TurnInput) *genai.GenerateContentConfig {
	cfg := &genai.GenerateContentConfig{
		Temperature:     &g.temperature,
		MaxOutputTokens: g.maxTokens,
		Tools:           toGeminiTools(in.Tools),
	}
	if in.SystemPrompt != "" {
		cfg.SystemInstruction = &genai.Content{
			Role:  "system",
			Parts: []*genai.Part{{Text: in.SystemPrompt}},
		}
	}
	return cfg
}

func (s *geminiTurn) run(ctx context.Context, out chan<- AssistantEvent) {
	defer close(out)
	stream := s.t.client.Models.GenerateContentStream(ctx, s.t.model, s.contents, s.cfg)
	for resp, err := range stream {
		if err != nil {
			out <- AssistantEvent{Err: err}
			return
		}
		if resp == nil || len(resp.Candidates) == 0 {
			continue
		}
		cand := resp.Candidates[0]
		if cand.Content != nil {
			for _, part := range cand.Content.Parts {
				switch {
				case part.Text != "":
					select {
					case out <- AssistantEvent{Text: part.Text}:
					case <-ctx.Done():
						return
					}
				case part.FunctionCall != nil:
					callID := part.FunctionCall.ID
					if callID == "" {
						callID = event.NewID()
					}
					// Mirror the assistant tool-call into the running contents
					// so the eventual Resume appends a matching FunctionResponse.
					s.contents = append(s.contents, &genai.Content{
						Role:  "model",
						Parts: []*genai.Part{{FunctionCall: part.FunctionCall}},
					})
					select {
					case out <- AssistantEvent{ToolCall: &ToolCall{
						ID:   callID,
						Tool: part.FunctionCall.Name,
						Args: part.FunctionCall.Args,
					}}:
					case <-ctx.Done():
						return
					}
					return // suspend; handler will Resume.
				}
			}
		}
		if cand.FinishReason != "" {
			select {
			case out <- AssistantEvent{FinalMessage: &FinalMessage{
				FinishReason: string(cand.FinishReason),
			}}:
			case <-ctx.Done():
			}
			return
		}
	}
	// Stream ended without an explicit FinishReason. Synthesize a terminal
	// frame so the handler always closes the turn cleanly.
	select {
	case out <- AssistantEvent{FinalMessage: &FinalMessage{FinishReason: "stop"}}:
	case <-ctx.Done():
	}
}

// historyToContents converts the orchestrator's persisted Turn slice plus the
// new user message into a Gemini Content slice. Each persisted Turn is
// expected to carry a JSON-serialized {"text": "..."} for user/assistant
// roles or a {"tool", "args", "output"} blob for tool_call / tool_result.
func historyToContents(hist []history.Turn, userText string) []*genai.Content {
	out := make([]*genai.Content, 0, len(hist)+1)
	for _, t := range hist {
		c := turnToContent(t)
		if c != nil {
			out = append(out, c)
		}
	}
	out = append(out, &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: userText}},
	})
	return out
}

func turnToContent(t history.Turn) *genai.Content {
	// The minimum supported payload is {"text": "..."}; tool turns get a more
	// elaborate shape. For now, treat any JSON we can extract a "text" field
	// from as a plain message. Tool legs are rebuilt by the Resume path, so
	// missing tool history just means the model loses earlier context — safe
	// degradation.
	var raw map[string]any
	if err := json.Unmarshal(t.Parts, &raw); err != nil {
		return nil
	}
	if text, ok := raw["text"].(string); ok && text != "" {
		role := "user"
		if t.Role == history.RoleAssistant {
			role = "model"
		}
		return &genai.Content{Role: role, Parts: []*genai.Part{{Text: text}}}
	}
	return nil
}

func toolResponseContent(r ToolResult) *genai.Content {
	return &genai.Content{
		Role: "function",
		Parts: []*genai.Part{
			{
				FunctionResponse: &genai.FunctionResponse{
					Name:     r.Tool,
					Response: r.Output,
				},
			},
		},
	}
}

