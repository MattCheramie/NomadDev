// Command sandbox is a thin CLI around internal/sandbox.Runner. It is useful
// for reproducing a tool invocation outside the orchestrator — most often to
// confirm an image pulls cleanly or to debug a script before wiring it into
// a `command.request` envelope.
//
// Build the mock-only version with: go build ./cmd/sandbox
// Build with real Docker support: go build -tags docker ./cmd/sandbox
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/mattcheramie/nomaddev/internal/sandbox"
)

func main() {
	runtime := flag.String("runtime", "mock", "runner runtime: mock | docker | none")
	image := flag.String("image", "alpine:3.20", "container image (docker runtime only)")
	workdir := flag.String("workspace", os.TempDir(), "host workspace bind-mounted at /work")
	tool := flag.String("tool", "execute_script", "tool name")
	shell := flag.String("shell", "bash", "shell for execute_script")
	script := flag.String("script", "", "script body; '-' reads stdin")
	timeout := flag.Duration("timeout", 30*time.Second, "exec wall-clock timeout")
	memBytes := flag.Int64("memory", 256<<20, "container memory limit in bytes")
	cpuNanos := flag.Int64("cpu-nanos", 1_000_000_000, "container CPU limit in nano-CPUs")
	pidsLimit := flag.Int64("pids-limit", 256, "container pids cgroup limit")
	preferRunsc := flag.Bool("prefer-runsc", true, "use gVisor (runsc) if the daemon advertises it")
	network := flag.String("network", "none", "container network mode")
	flag.Parse()

	if err := run(*runtime, *image, *workdir, *tool, *shell, *script, *timeout,
		*memBytes, *cpuNanos, *pidsLimit, *preferRunsc, *network); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		os.Exit(1)
	}
}

func run(runtime, image, workdir, tool, shell, scriptArg string, timeout time.Duration,
	memBytes, cpuNanos, pidsLimit int64, preferRunsc bool, network string) error {
	body := scriptArg
	if body == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		body = string(b)
	}
	if body == "" {
		return errors.New("-script is required (or use -script - to read stdin)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	runner, err := sandbox.NewRunner(ctx, sandbox.FactoryConfig{
		Runtime:        runtime,
		Image:          image,
		WorkspaceDir:   workdir,
		DefaultTimeout: timeout,
		Limits: sandbox.ResourceLimits{
			MemoryBytes: memBytes,
			CPUNanos:    cpuNanos,
			PidsLimit:   pidsLimit,
		},
		ReadonlyRoot: true,
		Network:      network,
		PreferRunsc:  preferRunsc,
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	if runner == nil {
		return errors.New("runner is nil (runtime=none); pick mock or docker")
	}

	req := sandbox.ExecRequest{
		Tool:    tool,
		Args:    map[string]any{"shell": shell, "script": body},
		Timeout: timeout,
	}
	ch, err := runner.Exec(ctx, req)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	enc := json.NewEncoder(os.Stdout)
	for c := range ch {
		entry := map[string]any{
			"stream":    c.Stream,
			"data":      string(c.Data),
			"exit_code": c.ExitCode,
		}
		if c.Err != nil {
			entry["err"] = c.Err.Error()
		}
		_ = enc.Encode(entry)
	}
	return nil
}
