// Package gitctl is a host-side git control-plane helper: a thin wrapper
// around the `git` binary, invoked via os/exec on the orchestrator host.
//
// Running git on the host is a deliberately new privilege. The rest of
// NomadDev executes untrusted work exclusively inside Docker sandboxes; this
// package is the one place where a process touches the host filesystem and
// runs a host binary on behalf of a task. It exists so the orchestrator can
// manage worktrees, commits, and merges for the operator-pre-cloned repo
// without handing repo write access to the sandbox.
//
// gitctl operates on the repo the operator has already cloned at the sandbox
// workspace directory. It never clones, fetches, or pushes over the network;
// callers point it at an existing working copy via Open.
//
// Hook safety. Every git invocation is run with -c core.hooksPath=/dev/null.
// Git hooks (pre-commit, post-checkout, etc.) are repo-supplied, and the repo
// content is attacker-influenced. A hook fired on the host would be arbitrary
// code execution outside the sandbox, so gitctl unconditionally disables all
// repo-supplied hooks. It likewise neutralises system and global git config
// (GIT_CONFIG_NOSYSTEM, GIT_CONFIG_GLOBAL=/dev/null), disables terminal
// prompts, and restricts transports so a malicious .gitmodules or config
// cannot redirect git into unexpected behaviour.
package gitctl
