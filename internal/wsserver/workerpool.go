package wsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mattcheramie/nomaddev/internal/event"
	"github.com/mattcheramie/nomaddev/internal/githubmcp"
	"github.com/mattcheramie/nomaddev/internal/gitctl"
	"github.com/mattcheramie/nomaddev/internal/history"
	"github.com/mattcheramie/nomaddev/internal/hub"
	"github.com/mattcheramie/nomaddev/internal/metrics"
	"github.com/mattcheramie/nomaddev/internal/middleware"
	"github.com/mattcheramie/nomaddev/internal/sandbox"
	"github.com/mattcheramie/nomaddev/internal/session"
)

// worktreePrefix is the directory, relative to the workspace root, under which
// gitctl materializes per-task worktrees. Must match gitctl's own constant.
const worktreePrefix = ".nomaddev-worktrees"

// runWorkerPool executes a dispatch_worker_pool tool call. It is invoked from
// runToolCall after the single launch approval. It creates one git worktree
// per sub-task, runs each headless sub-dispatcher in parallel under a
// concurrency cap, then merges the disjoint branches back into the primary
// branch — and returns the aggregate as a ToolResult, matching runToolCall's
// 5-tuple so the caller can hand it straight to the translator's resume.
func (s *Server) runWorkerPool(
	ctx context.Context, intentID, pendingCmdID string, call middleware.ToolCall,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) (middleware.ToolResult, []byte, int, string, bool) {
	started := time.Now()

	// fail emits the terminal command.result, persists the tool turns, and
	// returns the structural-failure 5-tuple. ok stays true: a bad worker-pool
	// request is a normal tool-result error the translator can react to, not
	// a turn-ending disconnect.
	fail := func(code, msg string) (middleware.ToolResult, []byte, int, string, bool) {
		s.emitResult(sess, client, pendingCmdID, started, -1, code, msg)
		output := map[string]any{"error_message": msg}
		_ = s.appendToolTurns(ctx, sess.SID, call, output, code, msg)
		metrics.WorkerPoolDispatchesTotal.WithLabelValues("failed").Inc()
		return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Output: output, Error: code},
			nil, -1, msg, true
	}

	if s.workerPoolSem == nil {
		return fail(event.SandboxErrBadRequest,
			"dispatch_worker_pool is not enabled; set NOMADDEV_WORKER_POOL_ENABLED=true")
	}
	// Per-session workspaces would scope fsops to <workspace>/<sid>, but the
	// git repo and worktrees live at the shared workspace root — the two
	// layouts are incompatible, so refuse cleanly rather than silently
	// resolving paths into the wrong tree.
	if s.cfg.Sandbox.PerSessionWorkspace {
		return fail(event.SandboxErrBadRequest,
			"dispatch_worker_pool requires NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE=false")
	}

	args, err := middleware.ParseWorkerPoolArgs(call.Args, s.mw.Config.WorkerPoolMaxTasks)
	if err != nil {
		return fail(event.SandboxErrBadRequest, err.Error())
	}

	repo, err := gitctl.Open(s.cfg.Sandbox.WorkspaceDir, logger)
	if err != nil {
		return fail(event.SandboxErrBadRequest, "worker pool: "+err.Error())
	}
	baseSHA, err := repo.HeadSHA(ctx, "HEAD")
	if err != nil {
		return fail(event.SandboxErrInternal, "worker pool: read HEAD: "+err.Error())
	}
	primaryRef, err := repo.CurrentBranch(ctx)
	if err != nil {
		return fail(event.SandboxErrInternal, "worker pool: read current branch: "+err.Error())
	}

	// Fork the conversation context once: every sub-dispatcher is seeded with
	// this same windowed history snapshot.
	parentHistory, err := s.mw.History.LoadWindow(ctx, sess.SID, s.mw.Config.WindowTurns)
	if err != nil {
		return fail(event.SandboxErrInternal, "worker pool: load history: "+err.Error())
	}
	baseSystemPrompt := s.mw.Config.SystemPrompt

	results := make([]middleware.WorkerTaskResult, len(args.Tasks))

	// Create one worktree per task off the captured base SHA. On any failure
	// roll back the worktrees created so far and abort the whole pool.
	worktrees := make([]*gitctl.Worktree, 0, len(args.Tasks))
	for _, task := range args.Tasks {
		wtID := intentID + "-" + task.ID
		branch := "nomaddev/wp-" + intentID + "-" + task.ID
		wt, addErr := repo.AddWorktree(ctx, wtID, branch, baseSHA)
		if addErr != nil {
			rb := context.WithoutCancel(ctx)
			for _, done := range worktrees {
				_ = repo.RemoveWorktree(rb, done, false)
			}
			return fail(event.SandboxErrInternal,
				"worker pool: create worktree for "+task.ID+": "+addErr.Error())
		}
		worktrees = append(worktrees, wt)
	}
	// Every worktree is cleaned up no matter how this function exits. Branches
	// are kept only for tasks the operator may want to inspect (failures,
	// scope violations, the by-construction-impossible merge conflict).
	defer func() {
		rb := context.WithoutCancel(ctx)
		for i, wt := range worktrees {
			if rmErr := repo.RemoveWorktree(rb, wt, workerKeepBranch(results[i])); rmErr != nil && logger != nil {
				logger.Warn("worker pool: worktree cleanup failed", "id", wt.ID, "err", rmErr)
			}
		}
	}()

	// Run the headless sub-dispatchers in parallel under the server-wide
	// concurrency cap.
	var wg sync.WaitGroup
	for i := range args.Tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task := args.Tasks[i]
			wt := worktrees[i]
			select {
			case s.workerPoolSem <- struct{}{}:
			case <-ctx.Done():
				results[i] = middleware.WorkerTaskResult{
					TaskID: task.ID, Branch: wt.Branch,
					Status: middleware.WorkerStatusCanceled, MergeStatus: "not_attempted",
					Error: event.SandboxErrCanceled,
				}
				return
			}
			defer func() { <-s.workerPoolSem }()

			// A per-sub-dispatcher timeout keeps one hung worker from pinning
			// a semaphore slot for the rest of the pool.
			taskCtx, taskCancel := context.WithTimeout(ctx, s.mw.Config.WorkerPoolTaskTimeout)
			defer taskCancel()

			s.emitWorkerUpdate(sess, client, intentID, task.ID, "started", wt.Branch, "", "", "")
			results[i] = s.dispatchOneTask(taskCtx, parentHistory, baseSystemPrompt, task, wt, sess, client, logger)
			s.emitWorkerUpdate(sess, client, intentID, task.ID, "finished", wt.Branch,
				results[i].Status, results[i].Summary, results[i].Error)
		}(i)
	}
	wg.Wait()

	// Sequential commit + scope-enforcement + merge-back. Concurrent merges
	// into one branch are unsafe, so this phase is single-threaded. The
	// worktree is committed first so the scope check sees the real change
	// set (an uncommitted worktree shows an empty branch diff). Because the
	// declared scopes are disjoint, merges never conflict by construction;
	// the ErrMergeConflict branch is kept as defense in depth.
	for i := range results {
		r := &results[i]
		wt := worktrees[i]
		if r.Status != middleware.WorkerStatusSuccess {
			r.MergeStatus = "skipped"
			continue
		}
		committed, commitErr := repo.CommitAll(ctx, wt, "nomaddev worker-pool task "+args.Tasks[i].ID)
		if commitErr != nil {
			r.Status = middleware.WorkerStatusFailed
			r.Error = "commit failed: " + commitErr.Error()
			r.MergeStatus = "skipped"
			continue
		}
		if !committed {
			// The sub-dispatcher reported success but made no file changes.
			r.MergeStatus = "skipped"
			continue
		}
		changed, cErr := repo.ChangedFiles(ctx, wt, baseSHA)
		if cErr != nil {
			r.Status = middleware.WorkerStatusFailed
			r.Error = "scope check failed: " + cErr.Error()
			r.MergeStatus = "skipped"
			continue
		}
		if bad := outOfScope(changed, args.Tasks[i].Paths); len(bad) > 0 {
			r.Status = middleware.WorkerStatusScopeViolation
			r.Error = "modified files outside the declared scope: " + strings.Join(bad, ", ")
			r.MergeStatus = "skipped"
			s.emitWorkerUpdate(sess, client, intentID, r.TaskID, "scope_violation", wt.Branch, r.Status, "", r.Error)
			continue
		}
		mr, mErr := repo.Merge(ctx, wt.Branch, "Merge worker-pool task "+args.Tasks[i].ID)
		if mErr != nil {
			r.MergeStatus = "conflict"
			r.Error = mErr.Error()
			continue
		}
		r.MergeStatus = "merged"
		r.MergedSHA = mr.MergedSHA
		s.emitWorkerUpdate(sess, client, intentID, r.TaskID, "merged", wt.Branch, r.Status, "", "")
	}

	// Record metrics and fold sub-dispatcher token spend into the LLM
	// counters so a pool's cost is visible on the same dashboards.
	for _, r := range results {
		metrics.WorkerPoolTasksTotal.WithLabelValues(r.Status).Inc()
		var sink middleware.Usage
		accumulateUsage(&sink, r.Usage, s.mw.Config.Provider, s.mw.Config.Model)
	}
	metrics.WorkerPoolDispatchesTotal.WithLabelValues(poolOutcome(results)).Inc()

	aggregate := middleware.WorkerPoolResult{
		BaseSHA: baseSHA, PrimaryRef: primaryRef, Tasks: results,
	}
	summary := renderWorkerPoolSummary(aggregate)
	output := map[string]any{
		"worker_pool": aggregate,
		"summary":     summary,
	}
	s.emitResult(sess, client, pendingCmdID, started, 0, "", "")
	_ = s.appendToolTurns(ctx, sess.SID, call, output, "", "")
	return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Output: output, Error: ""},
		nil, 0, "", true
}

// dispatchOneTask runs a single headless sub-dispatcher: a forked turn loop
// seeded with the parent conversation history plus the sub-task prompt,
// confined to wt's worktree. It drives the translator to a terminal frame and
// reports the per-task outcome. It never emits assistant.* envelopes — the
// pool aggregates results instead — but each mutating tool call still goes
// through the human-approval gate inside runWorkerToolCall.
func (s *Server) dispatchOneTask(
	ctx context.Context, parentHistory []history.Turn, baseSystemPrompt string,
	task middleware.SubTask, wt *gitctl.Worktree,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) middleware.WorkerTaskResult {
	taskStarted := time.Now()
	defer func() { metrics.WorkerPoolTaskSeconds.Observe(time.Since(taskStarted).Seconds()) }()

	res := middleware.WorkerTaskResult{
		TaskID: task.ID, Branch: wt.Branch,
		Status: middleware.WorkerStatusFailed, MergeStatus: "not_attempted",
	}

	in := middleware.TurnInput{
		SID:          sess.SID,
		UserText:     task.Prompt,
		History:      parentHistory,
		SystemPrompt: workerSystemPrompt(baseSystemPrompt, task),
		Tools:        middleware.SubDispatcherTools(s.mw.AvailableToolsFor(middleware.ModeNormal)),
		Mode:         middleware.ModeNormal,
		Model:        s.effectiveModel(sess.SID),
	}
	eventsCh, resume, err := s.mw.Translator.Stream(ctx, in)
	if err != nil {
		res.Error = "translator stream: " + err.Error()
		return res
	}

	workerIntentID := event.NewID()
	var summary strings.Builder
	var usage middleware.Usage

	for {
		var toolCall *middleware.ToolCall
		var streamErr bool
		final := false
		for ev := range eventsCh {
			switch {
			case ev.Err != nil:
				streamErr = true
			case ev.Text != "":
				summary.WriteString(ev.Text)
			case ev.Usage != nil:
				usage.PromptTokens += ev.Usage.PromptTokens
				usage.CandidatesTokens += ev.Usage.CandidatesTokens
				usage.TotalTokens += ev.Usage.TotalTokens
			case ev.ToolCall != nil:
				tc := *ev.ToolCall
				toolCall = &tc
			case ev.FinalMessage != nil:
				usage.PromptTokens += ev.FinalMessage.Usage.PromptTokens
				usage.CandidatesTokens += ev.FinalMessage.Usage.CandidatesTokens
				usage.TotalTokens += ev.FinalMessage.Usage.TotalTokens
				final = true
			}
		}
		res.Usage = usage

		if streamErr {
			res.Error = "translator reported a fatal turn error"
			return res
		}
		if ctx.Err() != nil {
			res.Status = middleware.WorkerStatusCanceled
			res.Error = event.SandboxErrCanceled
			return res
		}
		if final {
			res.Status = middleware.WorkerStatusSuccess
			res.Summary = strings.TrimSpace(summary.String())
			return res
		}
		if toolCall == nil {
			res.Error = "translator closed without a final message or tool call"
			return res
		}

		toolResult := s.runWorkerToolCall(ctx, workerIntentID, *toolCall, wt, task, sess, client, logger)
		next, rErr := resume(ctx, toolResult)
		if rErr != nil {
			res.Error = "translator resume: " + rErr.Error()
			res.Summary = strings.TrimSpace(summary.String())
			return res
		}
		eventsCh = next
	}
}

// runWorkerToolCall executes one tool call from a headless sub-dispatcher,
// confined to wt's worktree and task's declared scope. It still runs the full
// human-approval round-trip for mutating tools (the operator approves each
// task's edits) but, unlike runToolCall, does not persist tool turns to the
// shared session history — sub-dispatcher turns are ephemeral; only the pool
// aggregate is persisted.
func (s *Server) runWorkerToolCall(
	ctx context.Context, workerIntentID string, call middleware.ToolCall,
	wt *gitctl.Worktree, task middleware.SubTask,
	sess *session.Session, client *hub.Client, logger *slog.Logger,
) middleware.ToolResult {
	cmdEnv, _ := event.NewReply(event.EventCommandRequest, workerIntentID, event.CommandRequestPayload{
		Tool: call.Tool,
		Args: event.RedactArgs(call.Args),
	})
	s.bufferAndSend(sess, client, cmdEnv)

	toolErr := func(code, msg string) middleware.ToolResult {
		s.emitResult(sess, client, cmdEnv.ID, time.Now(), -1, code, msg)
		return middleware.ToolResult{
			CallID: call.ID, Tool: call.Tool,
			Output: map[string]any{"error_message": msg}, Error: code,
		}
	}

	if vErr := middleware.Validate(call.Tool, call.Args); vErr != nil {
		return toolErr(event.SandboxErrBadRequest, vErr.Error())
	}

	// Confine the call to the task's worktree and declared file scope.
	scopedArgs, workingDir, scopeErr := scopeWorkerCall(call.Tool, call.Args, wt.ID, task)
	if scopeErr != nil {
		return toolErr(event.SandboxErrBadRequest, scopeErr.Error())
	}
	scopedCall := middleware.ToolCall{ID: call.ID, Tool: call.Tool, Args: scopedArgs}

	approved, code, msg, _ := s.requestApproval(ctx, cmdEnv.ID, scopedCall, sess, client)
	if !approved {
		return toolErr(code, msg)
	}

	started := time.Now()
	dispatchCtx := githubmcp.WithUserSub(ctx, client.Sub)
	ch, err := s.mw.Dispatcher.Dispatch(dispatchCtx, scopedCall, middleware.DispatchOptions{
		WorkingDir:     workingDir,
		Timeout:        s.mw.Config.DefaultTimeout,
		SandboxLimits:  s.mw.Config.SandboxLimits,
		SessionID:      client.SID,
		MaxResultBytes: s.mw.Config.MaxResultBytes,
		Mode:           middleware.ModeNormal,
	})
	if err != nil {
		dCode := event.SandboxErrInternal
		if errors.Is(err, sandbox.ErrBadRequest) {
			dCode = event.SandboxErrBadRequest
		}
		return toolErr(dCode, err.Error())
	}

	seq := map[string]int{event.StreamStdout: 0, event.StreamStderr: 0}
	var stdoutBuf, stderrBuf []byte
	exitCode := 0
	exitErrCode := ""
	exitMsg := ""
	for chunk := range ch {
		switch chunk.Stream {
		case sandbox.StreamStdout:
			stdoutBuf = append(stdoutBuf, chunk.Data...)
			s.emitChunk(sess, client, cmdEnv.ID, chunk, seq)
		case sandbox.StreamStderr:
			stderrBuf = append(stderrBuf, chunk.Data...)
			s.emitChunk(sess, client, cmdEnv.ID, chunk, seq)
		case sandbox.StreamExit:
			exitCode, exitErrCode, exitMsg = classifyExit(chunk)
			s.emitResult(sess, client, cmdEnv.ID, started, exitCode, exitErrCode, exitMsg)
		}
	}
	if logger != nil && exitErrCode != "" {
		logger.Debug("worker pool: tool call failed",
			"task", task.ID, "tool", call.Tool, "error", exitErrCode, "msg", exitMsg)
	}

	output := map[string]any{"exit_code": exitCode}
	if len(stdoutBuf) > 0 {
		output["stdout"] = string(stdoutBuf)
	}
	if len(stderrBuf) > 0 {
		output["stderr"] = string(stderrBuf)
	}
	return middleware.ToolResult{CallID: call.ID, Tool: call.Tool, Output: output, Error: exitErrCode}
}

// scopeWorkerCall rewrites a tool call so it operates inside one sub-task's
// worktree. fsops path arguments are prefixed into .nomaddev-worktrees/<id>/;
// sandbox tools instead get a working directory. For write tools it also
// rejects targets outside the task's declared scope. It returns the (possibly
// rewritten) args and the working directory for sandbox dispatch.
func scopeWorkerCall(tool string, args map[string]any, wtID string, task middleware.SubTask) (map[string]any, string, error) {
	prefix := worktreePrefix + "/" + wtID
	rewrite := func(key string, enforceScope bool) (map[string]any, error) {
		raw, _ := args[key].(string)
		cleaned, err := middleware.CleanWorkspacePath(raw)
		if err != nil {
			return nil, err
		}
		if enforceScope && !middleware.PathInScope(cleaned, task.Paths) {
			return nil, fmt.Errorf("%s %q is outside this sub-task's declared scope", key, raw)
		}
		m := make(map[string]any, len(args))
		for k, v := range args {
			m[k] = v
		}
		m[key] = prefix + "/" + cleaned
		return m, nil
	}

	switch tool {
	case middleware.ToolReadFile, middleware.ToolListDir,
		middleware.ToolPinFile, middleware.ToolUnpinFile:
		m, err := rewrite("path", false)
		return m, "", err
	case middleware.ToolWritePatch:
		m, err := rewrite("path", true)
		return m, "", err
	case middleware.ToolApplyCodePatch:
		m, err := rewrite("file_path", true)
		return m, "", err
	case middleware.ToolExecuteScript, middleware.ToolSearchSyntax:
		// Sandbox tools run with the worktree as their working directory;
		// their path arguments are already relative to it.
		return args, prefix, nil
	default:
		// github_* and any other non-workspace tool: nothing to confine.
		return args, "", nil
	}
}

// emitWorkerUpdate sends one worker.update lifecycle envelope, correlated to
// the parent pool's intent id.
func (s *Server) emitWorkerUpdate(
	sess *session.Session, client *hub.Client,
	poolID, taskID, phase, branch, status, summary, errMsg string,
) {
	env, err := event.NewReply(event.EventWorkerUpdate, poolID, event.WorkerUpdatePayload{
		PoolID:  poolID,
		TaskID:  taskID,
		Phase:   phase,
		Branch:  branch,
		Status:  status,
		Summary: truncateStr(oneLine(summary), 2000),
		Error:   errMsg,
	})
	if err != nil {
		return
	}
	s.bufferAndSend(sess, client, env)
}

// workerSystemPrompt builds a sub-dispatcher's system prompt: the base prompt
// plus a steering block that pins the worker to its sub-task and declared
// scope.
func workerSystemPrompt(base string, task middleware.SubTask) string {
	steer := "You are a NomadDev worker-pool sub-dispatcher running headlessly inside an " +
		"isolated git worktree. Complete ONLY this sub-task. You may modify only these " +
		"paths: " + strings.Join(task.Paths, ", ") + ". Editing any file outside that set " +
		"will cause your work to be rejected. Do not assume changes made by other workers " +
		"are visible. When finished, reply with a concise summary of what you changed."
	if base == "" {
		return steer
	}
	return base + "\n\n" + steer
}

// outOfScope returns the changed files that fall outside the declared scope.
func outOfScope(changed, scope []string) []string {
	var bad []string
	for _, f := range changed {
		if !middleware.PathInScope(f, scope) {
			bad = append(bad, f)
		}
	}
	return bad
}

// workerKeepBranch reports whether a task's temp branch should survive
// cleanup. Successfully-merged tasks are deleted; everything else is kept so
// the operator can inspect or recover it.
func workerKeepBranch(r middleware.WorkerTaskResult) bool {
	if r.Status != middleware.WorkerStatusSuccess {
		return true
	}
	return r.MergeStatus == "conflict"
}

// poolOutcome classifies the aggregate result for the dispatch metric.
func poolOutcome(rs []middleware.WorkerTaskResult) string {
	ok, bad := 0, 0
	for _, r := range rs {
		if r.Status == middleware.WorkerStatusSuccess {
			ok++
		} else {
			bad++
		}
	}
	switch {
	case bad == 0:
		return "ok"
	case ok == 0:
		return "failed"
	default:
		return "partial"
	}
}

// renderWorkerPoolSummary renders a human-readable digest of the pool result
// for the translator's tool-result and the wire payload.
func renderWorkerPoolSummary(r middleware.WorkerPoolResult) string {
	merged := 0
	for _, t := range r.Tasks {
		if t.MergeStatus == "merged" {
			merged++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "worker pool: %d sub-task(s) on %s (base %s); %d merged, %d not merged.\n",
		len(r.Tasks), r.PrimaryRef, shortSHA(r.BaseSHA), merged, len(r.Tasks)-merged)
	for _, t := range r.Tasks {
		fmt.Fprintf(&b, "  [%s] status=%s merge=%s", t.TaskID, t.Status, t.MergeStatus)
		if t.MergedSHA != "" {
			fmt.Fprintf(&b, " (%s)", shortSHA(t.MergedSHA))
		}
		if t.Error != "" {
			fmt.Fprintf(&b, " — %s", t.Error)
		}
		b.WriteString("\n")
		if t.Summary != "" {
			fmt.Fprintf(&b, "      %s\n", truncateStr(oneLine(t.Summary), 300))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// shortSHA truncates a git SHA for display.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// oneLine collapses whitespace runs (including newlines) into single spaces.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateStr caps s at n bytes, appending an ellipsis when it had to cut.
func truncateStr(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
