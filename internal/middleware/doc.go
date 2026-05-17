// Package middleware translates free-text user intent into typed tool calls
// (Phase 4). The default build ships a MockTranslator for tests and
// scripted smoke flows; the real Gemini-backed Translator lives behind the
// `gemini` build tag so the orchestrator can still ship without the SDK.
//
// The handler in internal/wsserver/middleware.go drives a translator stream
// for one user.intent envelope: text tokens flow back as assistant.chunk,
// tool calls go through an approval gate then a dispatcher (sandbox for
// execute_script, fsops for read_file/list_dir/write_patch), and the
// stream terminates with one assistant.message.
package middleware
