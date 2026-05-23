package state

import (
	"encoding/json"

	"github.com/mattcheramie/nomaddev/internal/event"
)

// Ingest routes one inbound envelope into the store. The reducer mirrors
// mobile/src/state/store.ts so the native app and the React Native SPA stay
// in lockstep behaviorally — adding a new event type means updating both.
//
// Ingest is safe to call from the wireclient.Session callback goroutine; it
// uses Store.Update which is concurrency-safe.
func Ingest(store *Store, env event.Envelope) {
	store.Update(func(st *State) {
		st.LastEventID = env.ID
		switch env.Type {
		case event.EventHello:
			applyHello(st, env)
		case event.EventAssistantChunk:
			applyAssistantChunk(st, env)
		case event.EventAssistantMessage:
			applyAssistantMessage(st, env)
		case event.EventError:
			applyError(st, env)
		case event.EventAck:
			applyAck(st, env)
		}
	})
}

func applyHello(st *State, env event.Envelope) {
	var p event.HelloPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	st.SessionID = p.SessionID
	if p.Provider != "" {
		st.Provider = p.Provider
	}
	if p.Model != "" {
		st.Model = p.Model
	}
	if len(p.AvailableModels) > 0 {
		st.AvailableModels = append([]string(nil), p.AvailableModels...)
	}
	st.LastError = ""
}

func applyAssistantChunk(st *State, env event.Envelope) {
	turn := findTurn(st, env.CorrelationID)
	if turn == nil {
		return
	}
	var p event.AssistantChunkPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	turn.AssistantText += p.Text
}

func applyAssistantMessage(st *State, env event.Envelope) {
	turn := findTurn(st, env.CorrelationID)
	if turn == nil {
		return
	}
	var p event.AssistantMessagePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	if p.Text != "" {
		// AssistantMessagePayload.Text is the final, authoritative text
		// once streaming completes — replace any accumulated chunks so
		// post-stream rewrites (rare but possible) don't double up.
		turn.AssistantText = p.Text
	}
	if p.FinishReason == "error" || p.Error != "" {
		turn.Error = p.Error
		if turn.Error == "" {
			turn.Error = "assistant returned an error frame"
		}
	}
	turn.Finished = true
	if p.Usage != nil {
		st.SessionTokens.Prompt += p.Usage.PromptTokens
		st.SessionTokens.Candidates += p.Usage.CandidatesTokens
		st.SessionTokens.Total += p.Usage.TotalTokens
		st.SessionTokens.CostUSD += p.Usage.CostUSD
	}
}

func applyError(st *State, env event.Envelope) {
	var p event.ErrorPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	st.LastError = p.Message
}

func applyAck(st *State, env event.Envelope) {
	var p event.AckPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	if p.Action == event.UserCommandSetModel && p.Error == "" && p.Model != "" {
		st.Model = p.Model
	}
}

// findTurn returns a pointer to the turn whose IntentID equals correlationID,
// or nil if no such turn is currently tracked. The pointer is valid only for
// the duration of the Update callback that called us.
func findTurn(st *State, correlationID string) *Turn {
	if correlationID == "" {
		return nil
	}
	for i := range st.Turns {
		if st.Turns[i].IntentID == correlationID {
			return &st.Turns[i]
		}
	}
	return nil
}
