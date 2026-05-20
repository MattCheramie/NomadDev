package gitctl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitAvailable reports whether a git binary is on PATH.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// requireGit skips the test unless git is installed and we are not running in
// -short mode. Every test that touches a real repo calls this first.
func requireGit(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping git integration test in -short mode")
	}
	if !gitAvailable() {
		t.Skip("git not installed")
	}
}

// runGit runs a raw git command in dir and fails the test on error. It is used
// only by test helpers to build fixtures, not to exercise the package itself.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@localhost",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@localhost",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// writeFile writes content to path under dir, creating parents as needed.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// newTestRepo creates a fresh temp git repo with one initial commit and
// returns an opened *Repo handle.
func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()

	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "user.email", "test@localhost")

	writeFile(t, dir, "README.md", "initial\n")
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial commit")

	repo, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return repo
}

func TestOpen_NotARepo(t *testing.T) {
	requireGit(t)

	dir := t.TempDir() // plain temp dir, no git init
	_, err := Open(dir, nil)
	if !errors.Is(err, ErrNotARepo) {
		t.Fatalf("Open on non-repo: err = %v, want ErrNotARepo", err)
	}
}

func TestOpen_ValidRepo(t *testing.T) {
	requireGit(t)

	repo := newTestRepo(t)
	branch, err := repo.CurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "main" {
		t.Fatalf("CurrentBranch = %q, want main", branch)
	}
}

func TestAddRemoveWorktree_RoundTrip(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	tests := []struct {
		name       string
		keepBranch bool
	}{
		{name: "delete branch", keepBranch: false},
		{name: "keep branch", keepBranch: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestRepo(t)

			id := "task-1"
			branch := "nomaddev/task-1"
			wt, err := repo.AddWorktree(ctx, id, branch, "HEAD")
			if err != nil {
				t.Fatalf("AddWorktree: %v", err)
			}

			if _, statErr := os.Stat(wt.Path); statErr != nil {
				t.Fatalf("worktree dir missing after add: %v", statErr)
			}
			if wt.Branch != branch {
				t.Fatalf("wt.Branch = %q, want %q", wt.Branch, branch)
			}

			if err := repo.RemoveWorktree(ctx, wt, tc.keepBranch); err != nil {
				t.Fatalf("RemoveWorktree: %v", err)
			}

			if _, statErr := os.Stat(wt.Path); !os.IsNotExist(statErr) {
				t.Fatalf("worktree dir still exists after remove: %v", statErr)
			}

			// Probe the branch via the package's run helper.
			_, _, branchErr := repo.run(ctx, repo.root, "rev-parse", "--verify", branch)
			if tc.keepBranch && branchErr != nil {
				t.Fatalf("branch should have been kept, but is gone: %v", branchErr)
			}
			if !tc.keepBranch && branchErr == nil {
				t.Fatalf("branch should have been deleted, but still exists")
			}
		})
	}
}

func TestRemoveWorktree_Idempotent(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	repo := newTestRepo(t)
	wt, err := repo.AddWorktree(ctx, "task-x", "nomaddev/task-x", "HEAD")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := repo.RemoveWorktree(ctx, wt, false); err != nil {
		t.Fatalf("first RemoveWorktree: %v", err)
	}
	// Removing again must not error.
	if err := repo.RemoveWorktree(ctx, wt, false); err != nil {
		t.Fatalf("second RemoveWorktree should be idempotent, got: %v", err)
	}
}

func TestAddWorktree_RejectsUnsafeID(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := newTestRepo(t)

	for _, id := range []string{"", "a/b", "..", "../escape", "x/../y"} {
		if _, err := repo.AddWorktree(ctx, id, "nomaddev/b", "HEAD"); err == nil {
			t.Errorf("AddWorktree(%q) should have been rejected", id)
		}
	}
}

func TestCommitAll_CleanAndDirty(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	repo := newTestRepo(t)
	wt, err := repo.AddWorktree(ctx, "task-2", "nomaddev/task-2", "HEAD")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	// Clean worktree: nothing staged.
	committed, err := repo.CommitAll(ctx, wt, "no-op")
	if err != nil {
		t.Fatalf("CommitAll on clean tree: %v", err)
	}
	if committed {
		t.Fatalf("CommitAll on clean tree: committed = true, want false")
	}

	// Dirty worktree: a new file appears.
	writeFile(t, wt.Path, "new.txt", "hello\n")
	committed, err = repo.CommitAll(ctx, wt, "add new.txt")
	if err != nil {
		t.Fatalf("CommitAll on dirty tree: %v", err)
	}
	if !committed {
		t.Fatalf("CommitAll on dirty tree: committed = false, want true")
	}
}

func TestChangedFiles(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	repo := newTestRepo(t)
	wt, err := repo.AddWorktree(ctx, "task-3", "nomaddev/task-3", "HEAD")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	writeFile(t, wt.Path, "feature.go", "package main\n")
	committed, err := repo.CommitAll(ctx, wt, "add feature.go")
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	if !committed {
		t.Fatalf("expected a commit")
	}

	files, err := repo.ChangedFiles(ctx, wt, "main")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(files) != 1 || files[0] != "feature.go" {
		t.Fatalf("ChangedFiles = %v, want [feature.go]", files)
	}
}

func TestMerge_NewFileSucceeds(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	repo := newTestRepo(t)
	wt, err := repo.AddWorktree(ctx, "task-merge", "nomaddev/task-merge", "HEAD")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	writeFile(t, wt.Path, "added.txt", "from worktree\n")
	if _, err := repo.CommitAll(ctx, wt, "add added.txt"); err != nil {
		t.Fatalf("CommitAll: %v", err)
	}

	res, err := repo.Merge(ctx, wt.Branch, "merge task-merge")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Conflicted {
		t.Fatalf("Merge reported conflict on a clean merge")
	}
	if res.MergedSHA == "" {
		t.Fatalf("Merge returned empty MergedSHA")
	}

	// The new file must now exist in the primary worktree.
	if _, statErr := os.Stat(filepath.Join(repo.root, "added.txt")); statErr != nil {
		t.Fatalf("merged file missing from primary worktree: %v", statErr)
	}
}

func TestMerge_ConflictAbortsAndPreservesHead(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	repo := newTestRepo(t)

	// Seed a file on main so both branches edit the same line.
	writeFile(t, repo.root, "shared.txt", "original line\n")
	runGit(t, repo.root, "add", "shared.txt")
	runGit(t, repo.root, "commit", "-m", "add shared.txt")

	wtA, err := repo.AddWorktree(ctx, "branch-a", "nomaddev/branch-a", "HEAD")
	if err != nil {
		t.Fatalf("AddWorktree A: %v", err)
	}
	wtB, err := repo.AddWorktree(ctx, "branch-b", "nomaddev/branch-b", "HEAD")
	if err != nil {
		t.Fatalf("AddWorktree B: %v", err)
	}

	// Both branches edit the SAME line.
	writeFile(t, wtA.Path, "shared.txt", "edit from A\n")
	if _, err := repo.CommitAll(ctx, wtA, "A edits shared.txt"); err != nil {
		t.Fatalf("CommitAll A: %v", err)
	}
	writeFile(t, wtB.Path, "shared.txt", "edit from B\n")
	if _, err := repo.CommitAll(ctx, wtB, "B edits shared.txt"); err != nil {
		t.Fatalf("CommitAll B: %v", err)
	}

	// First merge succeeds.
	if _, err := repo.Merge(ctx, wtA.Branch, "merge A"); err != nil {
		t.Fatalf("Merge A should succeed: %v", err)
	}

	headBefore, err := repo.HeadSHA(ctx, "HEAD")
	if err != nil {
		t.Fatalf("HeadSHA before: %v", err)
	}

	// Second merge conflicts.
	res, err := repo.Merge(ctx, wtB.Branch, "merge B")
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("Merge B: err = %v, want ErrMergeConflict", err)
	}
	if !res.Conflicted {
		t.Fatalf("Merge B: Conflicted = false, want true")
	}
	if len(res.ConflictPaths) == 0 {
		t.Fatalf("Merge B: ConflictPaths empty, want shared.txt")
	}
	foundShared := false
	for _, p := range res.ConflictPaths {
		if p == "shared.txt" {
			foundShared = true
		}
	}
	if !foundShared {
		t.Fatalf("Merge B: ConflictPaths = %v, want to contain shared.txt", res.ConflictPaths)
	}

	// HEAD must be byte-identical after the aborted merge.
	headAfter, err := repo.HeadSHA(ctx, "HEAD")
	if err != nil {
		t.Fatalf("HeadSHA after: %v", err)
	}
	if headBefore != headAfter {
		t.Fatalf("HEAD changed after failed merge: before=%s after=%s", headBefore, headAfter)
	}
}

// TestCommitAll_IgnoresRepoHooks installs a pre-commit hook that would create a
// sentinel file. Because gitctl forces core.hooksPath=/dev/null, the hook must
// never fire and the sentinel must not appear.
func TestCommitAll_IgnoresRepoHooks(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	repo := newTestRepo(t)
	wt, err := repo.AddWorktree(ctx, "task-hook", "nomaddev/task-hook", "HEAD")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	// Hooks for a linked worktree live in the primary repo's .git/hooks.
	hooksDir := filepath.Join(repo.root, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	sentinel := filepath.Join(t.TempDir(), "hook-fired.sentinel")
	hookScript := "#!/bin/sh\ntouch " + sentinel + "\n"
	hookPath := filepath.Join(hooksDir, "pre-commit")
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	writeFile(t, wt.Path, "hooked.txt", "content\n")
	committed, err := repo.CommitAll(ctx, wt, "commit with hook present")
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	if !committed {
		t.Fatalf("expected a commit")
	}

	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatalf("pre-commit hook fired: sentinel %s exists despite core.hooksPath=/dev/null", sentinel)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on sentinel: %v", statErr)
	}
}
