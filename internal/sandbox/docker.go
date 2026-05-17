//go:build docker

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerRunner runs each Exec in a fresh, single-shot container. It uses the
// Docker engine API directly (no docker CLI shellout). One container per
// request keeps the threat surface minimal: a brand-new pid namespace, a
// fresh tmpfs at /tmp, and an AutoRemove lifecycle.
type DockerRunner struct {
	cli            *client.Client
	image          string
	workspaceDir   string
	defaultTimeout time.Duration
	limits         ResourceLimits
	readonlyRoot   bool
	network        string
	runtime        string // "" = engine default; "runsc" when gVisor is available and preferred.
	log            *slog.Logger
}

// DockerRunnerOptions is the constructor input for NewDockerRunner.
type DockerRunnerOptions struct {
	Image          string
	WorkspaceDir   string
	DefaultTimeout time.Duration
	Limits         ResourceLimits
	ReadonlyRoot   bool
	Network        string
	PreferRunsc    bool
	Logger         *slog.Logger
}

// NewDockerRunner constructs a DockerRunner and probes the daemon for runsc.
func NewDockerRunner(ctx context.Context, opts DockerRunnerOptions) (*DockerRunner, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("sandbox: docker client: %w", err)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	r := &DockerRunner{
		cli:            cli,
		image:          opts.Image,
		workspaceDir:   opts.WorkspaceDir,
		defaultTimeout: opts.DefaultTimeout,
		limits:         opts.Limits,
		readonlyRoot:   opts.ReadonlyRoot,
		network:        opts.Network,
		log:            log,
	}
	if r.network == "" {
		r.network = "none"
	}
	if opts.PreferRunsc {
		if r.daemonHasRuntime(ctx, "runsc") {
			r.runtime = "runsc"
		} else {
			r.log.Warn("sandbox: gVisor (runsc) not advertised by Docker daemon; using default runtime")
		}
	}
	return r, nil
}

func (r *DockerRunner) daemonHasRuntime(ctx context.Context, name string) bool {
	info, err := r.cli.Info(ctx)
	if err != nil {
		return false
	}
	_, ok := info.Runtimes[name]
	return ok
}

// Exec implements Runner.
func (r *DockerRunner) Exec(ctx context.Context, req ExecRequest) (<-chan ExecChunk, error) {
	if req.Tool != ToolExecuteScript {
		return nil, fmt.Errorf("%w: unknown tool %q", ErrBadRequest, req.Tool)
	}
	shell, _ := req.Args["shell"].(string)
	if shell == "" {
		shell = "bash"
	}
	script, _ := req.Args["script"].(string)
	if script == "" {
		return nil, fmt.Errorf("%w: missing or empty 'script' arg", ErrBadRequest)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.defaultTimeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	out := make(chan ExecChunk, 32)
	go r.runOne(ctx, req, shell, script, timeout, out)
	return out, nil
}

func (r *DockerRunner) runOne(
	parentCtx context.Context, req ExecRequest, shell, script string,
	timeout time.Duration, out chan<- ExecChunk,
) {
	defer close(out)

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	// 1. Pull the image. Reader MUST be drained for the pull to complete.
	pullReader, err := r.cli.ImagePull(ctx, r.image, types.ImagePullOptions{})
	if err != nil {
		out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: fmt.Errorf("%w: %v", ErrImagePull, err)}
		return
	}
	_, _ = io.Copy(io.Discard, pullReader)
	_ = pullReader.Close()

	// 2. Build container config.
	workingDir := "/work"
	if rel := cleanWorkdir(req.WorkingDir); rel != "" {
		workingDir = filepath.Join("/work", rel)
	}
	cfg := &container.Config{
		Image:        r.image,
		Cmd:          strslice.StrSlice{shell, "-c", script},
		WorkingDir:   workingDir,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
	}
	pidsLimit := r.limits.PidsLimit
	hostCfg := &container.HostConfig{
		// AutoRemove is intentionally off: we need the container around long
		// enough to ContainerInspect after exit so we can read State.OOMKilled
		// reliably. The deferred ContainerRemove(force: true) below handles
		// cleanup on every path.
		AutoRemove:     false,
		NetworkMode:    container.NetworkMode(r.network),
		ReadonlyRootfs: r.readonlyRoot,
		Runtime:        r.runtime,
		Resources: container.Resources{
			Memory:    r.limits.MemoryBytes,
			NanoCPUs:  r.limits.CPUNanos,
			PidsLimit: &pidsLimit,
		},
		Mounts: []mount.Mount{
			{Type: mount.TypeTmpfs, Target: "/tmp", TmpfsOptions: &mount.TmpfsOptions{SizeBytes: 64 << 20}},
		},
	}
	if r.workspaceDir != "" {
		hostCfg.Mounts = append(hostCfg.Mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: r.workspaceDir,
			Target: "/work",
		})
	}

	create, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: fmt.Errorf("container create: %w", err)}
		return
	}
	// Belt and braces in case start fails before AutoRemove engages.
	defer func() {
		_ = r.cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
	}()

	attach, err := r.cli.ContainerAttach(ctx, create.ID, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: fmt.Errorf("container attach: %w", err)}
		return
	}
	defer attach.Close()

	if err := r.cli.ContainerStart(ctx, create.ID, container.StartOptions{}); err != nil {
		out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: fmt.Errorf("container start: %w", err)}
		return
	}

	// 3. Fan out the multiplexed stdout/stderr stream onto our channel.
	stdoutW := &chunkWriter{stream: StreamStdout, out: out, ctx: ctx}
	stderrW := &chunkWriter{stream: StreamStderr, out: out, ctx: ctx}
	copyDone := make(chan struct{})
	go func() {
		_, _ = stdcopy.StdCopy(stdoutW, stderrW, attach.Reader)
		close(copyDone)
	}()

	// 4. Wait for either the container to exit, ctx to fire, or copy to end.
	statusCh, errCh := r.cli.ContainerWait(ctx, create.ID, container.WaitConditionNotRunning)
	var exit ExecChunk
	select {
	case <-ctx.Done():
		_ = r.cli.ContainerKill(context.Background(), create.ID, "KILL")
		// Wait for copy to drain so we don't lose trailing output.
		<-copyDone
		exit = ExecChunk{Stream: StreamExit, ExitCode: -1, Err: ctx.Err()}
		// Distinguish timeout from caller cancel.
		if errors.Is(parentCtx.Err(), context.Canceled) {
			exit.Err = ErrCanceled
		}
	case e := <-errCh:
		<-copyDone
		exit = ExecChunk{Stream: StreamExit, ExitCode: -1, Err: e}
	case s := <-statusCh:
		<-copyDone
		exit = ExecChunk{Stream: StreamExit, ExitCode: int(s.StatusCode)}
		if s.Error != nil {
			exit.Err = errors.New(s.Error.Message)
		}
		// OOM check: inspect before the AutoRemove path takes the container away.
		if s.StatusCode == 137 {
			if inspect, ierr := r.cli.ContainerInspect(context.Background(), create.ID); ierr == nil {
				if inspect.State != nil && inspect.State.OOMKilled {
					exit.Err = ErrOOM
				}
			}
		}
	}
	out <- exit
}

// chunkWriter is the io.Writer half of the stdcopy fan-out. It slices p into
// pieces of at most chunkSliceBytes and forwards each as one ExecChunk.
type chunkWriter struct {
	stream string
	out    chan<- ExecChunk
	ctx    context.Context
}

const chunkSliceBytes = 16 * 1024

func (w *chunkWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := len(p)
		if n > chunkSliceBytes {
			n = chunkSliceBytes
		}
		buf := append([]byte(nil), p[:n]...)
		select {
		case w.out <- ExecChunk{Stream: w.stream, Data: buf}:
		case <-w.ctx.Done():
			return 0, w.ctx.Err()
		}
		p = p[n:]
	}
	return total, nil
}

// cleanWorkdir is a path-safe join helper: rejects absolute paths and any
// component that climbs above the bind-mount root.
func cleanWorkdir(rel string) string {
	if rel == "" {
		return ""
	}
	c := filepath.Clean("/" + rel) // forces leading slash for normalization
	c = strings.TrimPrefix(c, "/")
	if c == "" || strings.HasPrefix(c, "..") {
		return ""
	}
	return c
}
