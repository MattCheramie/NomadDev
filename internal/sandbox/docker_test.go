//go:build docker

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// daemonReachable pings the local Docker daemon. Each tagged test calls
// requireDaemon(t) up front so the suite gracefully skips on machines
// without Docker rather than failing loudly.
func daemonReachable(ctx context.Context) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.Ping(ctx)
	return err
}

func requireDaemon(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := daemonReachable(ctx); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
}

func newDocker(t *testing.T) *DockerRunner {
	t.Helper()
	workdir := t.TempDir()
	r, err := NewDockerRunner(context.Background(), DockerRunnerOptions{
		Image:          "alpine:3.20",
		WorkspaceDir:   workdir,
		DefaultTimeout: 15 * time.Second,
		Limits: ResourceLimits{
			MemoryBytes: 64 << 20,
			CPUNanos:    500_000_000,
			PidsLimit:   64,
		},
		ReadonlyRoot: true,
		Network:      "none",
	})
	if err != nil {
		t.Fatalf("NewDockerRunner: %v", err)
	}
	return r
}

func collect(t *testing.T, ch <-chan ExecChunk) (stdout, stderr bytes.Buffer, exit ExecChunk) {
	t.Helper()
	for c := range ch {
		switch c.Stream {
		case StreamStdout:
			stdout.Write(c.Data)
		case StreamStderr:
			stderr.Write(c.Data)
		case StreamExit:
			exit = c
		}
	}
	return
}

func TestDocker_EchoHello(t *testing.T) {
	requireDaemon(t)
	r := newDocker(t)
	ch, err := r.Exec(context.Background(), ExecRequest{
		Tool: ToolExecuteScript,
		Args: map[string]any{"shell": "sh", "script": "echo hello-from-sandbox"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	stdout, _, exit := collect(t, ch)
	if exit.ExitCode != 0 || exit.Err != nil {
		t.Fatalf("exit = %+v", exit)
	}
	if got := stdout.String(); got != "hello-from-sandbox\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestDocker_NonZeroExit(t *testing.T) {
	requireDaemon(t)
	r := newDocker(t)
	ch, _ := r.Exec(context.Background(), ExecRequest{
		Tool: ToolExecuteScript,
		Args: map[string]any{"shell": "sh", "script": "exit 7"},
	})
	_, _, exit := collect(t, ch)
	if exit.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", exit.ExitCode)
	}
}

func TestDocker_TimeoutKills(t *testing.T) {
	requireDaemon(t)
	r := newDocker(t)
	ch, _ := r.Exec(context.Background(), ExecRequest{
		Tool:    ToolExecuteScript,
		Args:    map[string]any{"shell": "sh", "script": "sleep 30"},
		Timeout: 500 * time.Millisecond,
	})
	_, _, exit := collect(t, ch)
	if exit.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1 on timeout", exit.ExitCode)
	}
	if !errors.Is(exit.Err, context.DeadlineExceeded) {
		t.Fatalf("exit err = %v, want context.DeadlineExceeded", exit.Err)
	}
}

func TestDocker_BindMountIsolation(t *testing.T) {
	requireDaemon(t)
	r := newDocker(t)
	// Drop a sentinel file into the bind-mount directory; the container
	// should be able to read it at /work/sentinel.
	path := filepath.Join(r.workspaceDir, "sentinel")
	if err := os.WriteFile(path, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	ch, _ := r.Exec(context.Background(), ExecRequest{
		Tool: ToolExecuteScript,
		Args: map[string]any{"shell": "sh", "script": "cat /work/sentinel"},
	})
	stdout, _, exit := collect(t, ch)
	if exit.ExitCode != 0 {
		t.Fatalf("exit = %+v stdout=%q", exit, stdout.String())
	}
	if stdout.String() != "ok" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestDocker_NoNetwork(t *testing.T) {
	requireDaemon(t)
	r := newDocker(t)
	// With NetworkMode=none, DNS resolution fails — we want a non-zero exit
	// from something that requires the network. `wget` is in busybox/alpine.
	ch, _ := r.Exec(context.Background(), ExecRequest{
		Tool:    ToolExecuteScript,
		Args:    map[string]any{"shell": "sh", "script": "wget -T 2 -q -O - http://example.com >/dev/null 2>&1"},
		Timeout: 10 * time.Second,
	})
	_, _, exit := collect(t, ch)
	if exit.ExitCode == 0 {
		t.Fatalf("expected non-zero exit with NetworkMode=none, got 0")
	}
}

func TestDocker_OOMKill(t *testing.T) {
	requireDaemon(t)
	// Tight 16 MiB cap; the script asks dd for a 256 MiB buffer in one
	// allocation. The cgroup OOM killer should fire and Docker should set
	// State.OOMKilled, which the runner maps to ErrOOM.
	workdir := t.TempDir()
	r, err := NewDockerRunner(context.Background(), DockerRunnerOptions{
		Image:          "alpine:3.20",
		WorkspaceDir:   workdir,
		DefaultTimeout: 15 * time.Second,
		Limits: ResourceLimits{
			MemoryBytes: 16 << 20,
			CPUNanos:    500_000_000,
			PidsLimit:   64,
		},
		ReadonlyRoot: true,
		Network:      "none",
	})
	if err != nil {
		t.Fatalf("NewDockerRunner: %v", err)
	}
	ch, _ := r.Exec(context.Background(), ExecRequest{
		Tool:    ToolExecuteScript,
		Args:    map[string]any{"shell": "sh", "script": "dd if=/dev/zero of=/dev/null bs=256M count=1"},
		Timeout: 10 * time.Second,
	})
	_, _, exit := collect(t, ch)
	if !errors.Is(exit.Err, ErrOOM) {
		t.Fatalf("expected ErrOOM, got exit_code=%d err=%v", exit.ExitCode, exit.Err)
	}
}
