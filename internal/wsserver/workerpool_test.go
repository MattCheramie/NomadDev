package wsserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/fsops"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/middleware"
)

// poolScriptTranslator is a deterministic, concurrency-safe Translator for the
// worker-pool tests. It routes each Stream call to a per-prompt script keyed
// on TurnInput.UserText, so the parent turn and every headless sub-dispatcher
// turn run independently — unlike middleware.MockTranslator, whose single
// shared stage counter cannot survive concurrent sub-dispatchers.
type poolScriptTranslator struct {
	scripts map[string][][]middleware.AssistantEvent
}

func (t *poolScriptTranslator) Stream(
	ctx context.Context, in middleware.TurnInput,
) (<-chan middleware.AssistantEvent, middleware.ResumeFunc, error) {
	stages := t.scripts[in.UserText]
	idx := 0
	emit := func() <-chan middleware.AssistantEvent {
		cur := idx
		idx++
		out := make(chan middleware.AssistantEvent, 8)
		go func() {
			defer close(out)
			if cur >= len(stages) {
				out <- middleware.AssistantEvent{
					FinalMessage: &middleware.FinalMessage{FinishReason: "stop"},
				}
				return
			}
			for _, ev := range stages[cur] {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}()
		return out
	}
	resume := func(context.Context, middleware.ToolResult) (<-chan middleware.AssistantEvent, error) {
		return emit(), nil
	}
	return emit(), resume, nil
}

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func runGitSetup(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Neutralize host git config so the seed commit does not inherit global
	// signing/identity settings — matches how gitctl isolates its own runs.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newWorkerPoolRepo creates a temp git repo seeded with a.txt / b.txt and
// returns its absolute path.
func newWorkerPoolRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitSetup(t, dir, "init")
	runGitSetup(t, dir, "config", "user.name", "tester")
	runGitSetup(t, dir, "config", "user.email", "tester@example.com")
	for name, body := range map[string]string{
		"a.txt":    "orig a\n",
		"b.txt":    "orig b\n",
		"seed.txt": "seed\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	runGitSetup(t, dir, "add", "-A")
	runGitSetup(t, dir, "commit", "-m", "seed")
	return dir
}

// buildWorkerPoolMW assembles a middleware.Service with the worker pool
// enabled and approval required for both the pool launch and write_patch.
func buildWorkerPoolMW(t *testing.T, tr middleware.Translator, repoDir string, enabled bool) *middleware.Service {
	t.Helper()
	fsEngine, err := fsops.New(repoDir)
	if err != nil {
		t.Fatalf("fsops.New: %v", err)
	}
	required := []string{middleware.ToolWritePatch}
	if enabled {
		required = append(required, middleware.ToolDispatchWorkerPool)
	}
	return &middleware.Service{
		Translator: tr,
		Dispatcher: middleware.NewCompositeDispatcher(nil, fsEngine),
		Approver:   middleware.NewPolicyApprover(required, false, 5*time.Second),
		History:    history.NewMemoryStore(),
		Tools:      append(middleware.DefaultTools(), middleware.WorkerPoolSpec()),
		Config: middleware.RuntimeConfig{
			DefaultTimeout:          2 * time.Second,
			WindowTurns:             10,
			WorkerPoolEnabled:       enabled,
			WorkerPoolMaxConcurrent: 2,
			WorkerPoolMaxTasks:      8,
			WorkerPoolTaskTimeout:   30 * time.Second,
		},
	}
}

func assertFileBody(t *testing.T, dir, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// TestWorkerPool_TwoTasksMergeBack drives a 2-task dispatch_worker_pool end to
// end: each headless sub-dispatcher edits a disjoint file in its own worktree,
// every edit is human-approved, and both branches merge back into the primary
// branch.
func TestWorkerPool_TwoTasksMergeBack(t *testing.T) {
	if testing.Short() || !gitAvailable() {
		t.Skip("requires git; skipped under -short")
	}
	repoDir := newWorkerPoolRepo(t)

	tr := &poolScriptTranslator{scripts: map[string][][]middleware.AssistantEvent{
		"migrate everything": {
			{{ToolCall: &middleware.ToolCall{
				ID: "pool", Tool: middleware.ToolDispatchWorkerPool,
				Args: map[string]any{"tasks": []any{
					map[string]any{"id": "alpha", "prompt": "WORKER_A", "paths": []any{"a.txt"}},
					map[string]any{"id": "beta", "prompt": "WORKER_B", "paths": []any{"b.txt"}},
				}},
			}}},
			{{FinalMessage: &middleware.FinalMessage{Text: "pool complete", FinishReason: "stop"}}},
		},
		"WORKER_A": {
			{{ToolCall: &middleware.ToolCall{
				ID: "wa", Tool: middleware.ToolWritePatch,
				Args: map[string]any{"path": "a.txt", "content": "migrated a\n"},
			}}},
			{{Text: "edited a.txt"}, {FinalMessage: &middleware.FinalMessage{FinishReason: "stop"}}},
		},
		"WORKER_B": {
			{{ToolCall: &middleware.ToolCall{
				ID: "wb", Tool: middleware.ToolWritePatch,
				Args: map[string]any{"path": "b.txt", "content": "migrated b\n"},
			}}},
			{{Text: "edited b.txt"}, {FinalMessage: &middleware.FinalMessage{FinishReason: "stop"}}},
		},
	}}

	mw := buildWorkerPoolMW(t, tr, repoDir, true)
	ts, srv, _, issuer := newTestServerFull(t, testOpts{Middleware: mw, ApprovalTimeout: 5 * time.Second})
	srv.cfg.Sandbox.WorkspaceDir = repoDir

	tok, _ := issuer.Sign("matt", "sess-wp", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "migrate everything"})
	writeEnv(t, c, intent)

	merged := map[string]bool{}
	approvalReqs := 0
	var finalMsg *event.AssistantMessagePayload
	for i := 0; i < 80 && finalMsg == nil; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventToolApprovalRequest:
			approvalReqs++
			g, _ := event.NewReply(event.EventToolApprovalGranted, env.ID, event.ToolApprovalGrantedPayload{})
			writeEnv(t, c, g)
		case event.EventWorkerUpdate:
			var p event.WorkerUpdatePayload
			_ = env.UnmarshalPayload(&p)
			if p.PoolID != intent.ID {
				t.Errorf("worker.update pool_id = %q, want %q", p.PoolID, intent.ID)
			}
			if p.Phase == "merged" {
				merged[p.TaskID] = true
			}
		case event.EventAssistantMessage:
			if env.CorrelationID == intent.ID {
				var p event.AssistantMessagePayload
				_ = env.UnmarshalPayload(&p)
				finalMsg = &p
			}
		}
	}

	if finalMsg == nil {
		t.Fatal("never saw the terminal assistant.message")
	}
	if finalMsg.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", finalMsg.FinishReason)
	}
	// One approval for the pool launch + one per sub-task edit.
	if approvalReqs != 3 {
		t.Errorf("approval requests = %d, want 3 (1 launch + 2 edits)", approvalReqs)
	}
	if !merged["alpha"] || !merged["beta"] {
		t.Errorf("merged tasks = %v, want alpha+beta", merged)
	}
	// The primary branch carries both migrations.
	assertFileBody(t, repoDir, "a.txt", "migrated a\n")
	assertFileBody(t, repoDir, "b.txt", "migrated b\n")
	// Worktrees are cleaned up.
	if entries, _ := os.ReadDir(filepath.Join(repoDir, ".nomaddev-worktrees")); len(entries) != 0 {
		t.Errorf(".nomaddev-worktrees not cleaned up: %v", entries)
	}
}

// TestWorkerPool_DisabledRejected verifies that a dispatch_worker_pool call
// fails cleanly when the feature is not enabled.
func TestWorkerPool_DisabledRejected(t *testing.T) {
	if testing.Short() || !gitAvailable() {
		t.Skip("requires git; skipped under -short")
	}
	repoDir := newWorkerPoolRepo(t)
	tr := &poolScriptTranslator{scripts: map[string][][]middleware.AssistantEvent{
		"go": {
			{{ToolCall: &middleware.ToolCall{
				ID: "pool", Tool: middleware.ToolDispatchWorkerPool,
				Args: map[string]any{"tasks": []any{
					map[string]any{"prompt": "x", "paths": []any{"a.txt"}},
				}},
			}}},
			{{FinalMessage: &middleware.FinalMessage{Text: "done", FinishReason: "stop"}}},
		},
	}}
	mw := buildWorkerPoolMW(t, tr, repoDir, false)
	ts, srv, _, issuer := newTestServerFull(t, testOpts{Middleware: mw})
	srv.cfg.Sandbox.WorkspaceDir = repoDir

	tok, _ := issuer.Sign("matt", "sess-wp-off", nil)
	c, _, err := dialWithAuthHeader(t, ts, tok)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = readEnv(t, c) // hello

	intent, _ := event.NewEnvelope(event.EventUserIntent, event.UserIntentPayload{Text: "go"})
	writeEnv(t, c, intent)

	var result *event.CommandResultPayload
	var sawMsg bool
	for i := 0; i < 20 && !sawMsg; i++ {
		env := readEnv(t, c)
		switch env.Type {
		case event.EventCommandResult:
			var p event.CommandResultPayload
			_ = env.UnmarshalPayload(&p)
			result = &p
		case event.EventAssistantMessage:
			sawMsg = true
		}
	}
	if result == nil || result.Error != event.SandboxErrBadRequest {
		t.Fatalf("command.result = %+v, want error %q", result, event.SandboxErrBadRequest)
	}
}
