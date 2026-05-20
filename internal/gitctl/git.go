package gitctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a handle to one git working copy rooted at an absolute path.
type Repo struct {
	root   string // absolute path to the repo
	git    string // resolved absolute path to the git binary
	logger *slog.Logger
}

// Sentinel errors.
var (
	ErrNotARepo     = errors.New("gitctl: workspace is not a git repository")
	ErrGitMissing   = errors.New("gitctl: git binary not found on PATH")
	ErrMergeConflict = errors.New("gitctl: merge produced conflicts")
)

// worktreesDir is the directory name, relative to the repo root, under which
// gitctl creates per-task worktrees.
const worktreesDir = ".nomaddev-worktrees"

// Open validates that root contains a .git entry and resolves the git binary
// via exec.LookPath. Returns ErrGitMissing if git is absent, ErrNotARepo if
// root has no .git. logger may be nil (a discard logger is used then).
func Open(root string, logger *slog.Logger) (*Repo, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("gitctl: resolve root %q: %w", root, err)
	}

	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, ErrGitMissing
	}

	// A .git entry is either a directory (normal repo) or a file (worktree
	// or submodule). Either one is acceptable.
	if _, statErr := os.Stat(filepath.Join(abs, ".git")); statErr != nil {
		return nil, ErrNotARepo
	}

	return &Repo{root: abs, git: gitBin, logger: logger}, nil
}

// Root returns the absolute path the repo is rooted at.
func (r *Repo) Root() string { return r.root }

// run executes a git subcommand. dir is the working directory for the command;
// when empty r.root is used. The hook-disabling and identity -c flags are
// prepended to every invocation, and the environment is sanitised so system
// and global config cannot influence behaviour.
func (r *Repo) run(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error) {
	if dir == "" {
		dir = r.root
	}

	hardened := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "user.name=nomaddev",
		"-c", "user.email=nomaddev@localhost",
	}
	fullArgs := append(hardened, args...)

	cmd := exec.CommandContext(ctx, r.git, fullArgs...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ALLOW_PROTOCOL=file",
	)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout = strings.TrimRight(outBuf.String(), " \t\r\n")
	stderr = strings.TrimRight(errBuf.String(), " \t\r\n")

	if runErr != nil {
		sub := "git"
		if len(args) > 0 {
			sub = args[0]
		}
		r.logger.Debug("gitctl: git command failed",
			"subcommand", sub, "dir", dir, "stderr", stderr, "err", runErr)
		return stdout, stderr, fmt.Errorf("gitctl: git %s: %w: %s", sub, runErr, stderr)
	}
	return stdout, stderr, nil
}

// CurrentBranch returns the checked-out branch name of the primary worktree.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, _, err := r.run(ctx, r.root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return out, nil
}

// HeadSHA returns the full commit SHA that ref points at.
func (r *Repo) HeadSHA(ctx context.Context, ref string) (string, error) {
	out, _, err := r.run(ctx, r.root, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return out, nil
}

// Worktree describes one created worktree.
type Worktree struct {
	ID     string // task id
	Path   string // absolute path
	Branch string // the temp branch checked out there
}

// validID rejects ids that would be unsafe as a path segment. The caller is
// expected to pass a safe id; this is defence in depth.
func validID(id string) error {
	if id == "" {
		return errors.New("gitctl: worktree id must not be empty")
	}
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return fmt.Errorf("gitctl: unsafe worktree id %q", id)
	}
	return nil
}

// AddWorktree creates a worktree at <root>/.nomaddev-worktrees/<id> with a
// fresh branch based on baseRef.
func (r *Repo) AddWorktree(ctx context.Context, id, branch, baseRef string) (*Worktree, error) {
	if err := validID(id); err != nil {
		return nil, err
	}

	parent := filepath.Join(r.root, worktreesDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("gitctl: create worktrees dir: %w", err)
	}

	path := filepath.Join(parent, id)
	if _, _, err := r.run(ctx, r.root,
		"worktree", "add", "-b", branch, path, baseRef); err != nil {
		return nil, err
	}

	r.logger.Info("gitctl: worktree created", "id", id, "branch", branch, "path", path)
	return &Worktree{ID: id, Path: path, Branch: branch}, nil
}

// RemoveWorktree force-removes the worktree dir and prunes git's record, then,
// unless keepBranch, deletes the branch. It is idempotent: a missing worktree
// or branch is not an error.
func (r *Repo) RemoveWorktree(ctx context.Context, wt *Worktree, keepBranch bool) error {
	if wt == nil {
		return errors.New("gitctl: nil worktree")
	}

	if _, stderr, err := r.run(ctx, r.root, "worktree", "remove", "--force", wt.Path); err != nil {
		// A worktree that is already gone is not an error for an idempotent
		// remove. git reports this with "is not a working tree" / "No such".
		if !isMissingWorktree(stderr) {
			return err
		}
	}

	// Prune any stale administrative record left behind.
	if _, _, err := r.run(ctx, r.root, "worktree", "prune"); err != nil {
		return err
	}

	if !keepBranch && wt.Branch != "" {
		if _, stderr, err := r.run(ctx, r.root, "branch", "-D", wt.Branch); err != nil {
			if !isMissingBranch(stderr) {
				return err
			}
		}
	}

	r.logger.Info("gitctl: worktree removed", "id", wt.ID, "branch", wt.Branch, "keepBranch", keepBranch)
	return nil
}

func isMissingWorktree(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "is not a working tree") ||
		strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "not a valid path")
}

func isMissingBranch(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "not found") || strings.Contains(s, "couldn't look up")
}

// CommitAll stages every change in the worktree and commits with msg. A clean
// tree is not an error: it returns (false, nil). Otherwise it commits and
// returns (true, nil).
func (r *Repo) CommitAll(ctx context.Context, wt *Worktree, msg string) (committed bool, err error) {
	if wt == nil {
		return false, errors.New("gitctl: nil worktree")
	}

	if _, _, err := r.run(ctx, wt.Path, "add", "-A"); err != nil {
		return false, err
	}

	status, _, err := r.run(ctx, wt.Path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil
	}

	if _, _, err := r.run(ctx, wt.Path, "commit", "-m", msg); err != nil {
		return false, err
	}

	r.logger.Info("gitctl: committed worktree changes", "id", wt.ID, "branch", wt.Branch)
	return true, nil
}

// splitLines splits git output on newlines and drops empty entries.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ChangedFiles returns the set of files that differ between baseRef and the
// worktree's branch tip.
func (r *Repo) ChangedFiles(ctx context.Context, wt *Worktree, baseRef string) ([]string, error) {
	if wt == nil {
		return nil, errors.New("gitctl: nil worktree")
	}
	out, _, err := r.run(ctx, wt.Path, "diff", "--name-only", baseRef+".."+wt.Branch)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// MergeResult reports the outcome of Merge.
type MergeResult struct {
	MergedSHA     string
	Conflicted    bool
	ConflictPaths []string
}

// Merge merges branch into the repo's current (primary) branch with a non
// fast-forward merge commit. On conflict it aborts the merge, leaving the
// primary branch byte-identical, and returns ErrMergeConflict. It never
// force-merges.
func (r *Repo) Merge(ctx context.Context, branch, msg string) (MergeResult, error) {
	var res MergeResult

	_, _, mergeErr := r.run(ctx, r.root, "merge", "--no-ff", "--no-edit", "-m", msg, branch)
	if mergeErr != nil {
		// Identify unmerged paths before aborting.
		unmerged, _, diffErr := r.run(ctx, r.root, "diff", "--name-only", "--diff-filter=U")
		conflictPaths := splitLines(unmerged)

		if len(conflictPaths) > 0 {
			// Conflict: restore the primary branch to its pre-merge state.
			if _, _, abortErr := r.run(ctx, r.root, "merge", "--abort"); abortErr != nil {
				return MergeResult{Conflicted: true, ConflictPaths: conflictPaths},
					fmt.Errorf("%w (and abort failed: %v)", ErrMergeConflict, abortErr)
			}
			r.logger.Warn("gitctl: merge conflict aborted", "branch", branch, "paths", conflictPaths)
			res.Conflicted = true
			res.ConflictPaths = conflictPaths
			return res, ErrMergeConflict
		}

		// Non-zero exit that is not a content conflict (e.g. bad ref). Try to
		// abort defensively so we never leave a half-merged tree, then return
		// the original failure.
		_, _, _ = r.run(ctx, r.root, "merge", "--abort")
		if diffErr != nil {
			return res, mergeErr
		}
		return res, mergeErr
	}

	sha, _, err := r.run(ctx, r.root, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	res.MergedSHA = sha
	r.logger.Info("gitctl: merge succeeded", "branch", branch, "sha", sha)
	return res, nil
}
