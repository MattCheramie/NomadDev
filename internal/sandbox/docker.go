//go:build docker

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// searchSyntaxStdoutCap bounds how much of sg's stdout the runner will
// buffer before it stops appending. ast-grep can dump megabytes of
// matches on a permissive pattern; the reshaper truncates at the
// MaxResultBytes envelope cap further down, but this internal ceiling
// keeps the runner from RAM-blowing on the way there.
const searchSyntaxStdoutCap = 16 * 1024 * 1024

// DockerRunner runs each Exec in a fresh, single-shot container. It uses the
// Docker engine API directly (no docker CLI shellout). One container per
// request keeps the threat surface minimal: a brand-new pid namespace, a
// fresh tmpfs at /tmp, and an AutoRemove lifecycle.
type DockerRunner struct {
	cli            *client.Client
	image          string
	expectedDigest string // non-empty when image was pinned via @sha256:...
	workspaceDir   string
	perSessionWS   bool // bind-mount <workspaceDir>/<sid>/ when ExecRequest.SessionID is set
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
	// RequireDigest, when true, makes NewDockerRunner return
	// ErrImageDigestRequired if Image has no `@sha256:...` suffix. Use
	// this in production to refuse to boot on a tag-only ref that
	// would be vulnerable to a registry-tag race.
	RequireDigest bool
	// PerSessionWorkspace, when true, scopes the bind-mounted /work
	// volume to a per-SID subdirectory of WorkspaceDir so two
	// sessions can't see each other's files. The subdir is created
	// on first use with mode 0o700. ExecRequest.SessionID drives
	// the path; empty SessionID falls back to the shared root.
	// Default false for back-compat — existing single-tenant
	// deploys keep the shared workspace.
	PerSessionWorkspace bool
	Logger              *slog.Logger
}

// NewDockerRunner constructs a DockerRunner and probes the daemon for runsc.
func NewDockerRunner(ctx context.Context, opts DockerRunnerOptions) (*DockerRunner, error) {
	_, _, digest, err := ParseImageRef(opts.Image)
	if err != nil {
		return nil, fmt.Errorf("sandbox: parse image %q: %w", opts.Image, err)
	}
	if opts.RequireDigest && digest == "" {
		return nil, fmt.Errorf("%w: %q", ErrImageDigestRequired, opts.Image)
	}
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
		expectedDigest: digest,
		workspaceDir:   opts.WorkspaceDir,
		perSessionWS:   opts.PerSessionWorkspace,
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
	if digest != "" {
		r.log.Info("sandbox: image pinned by digest",
			"image", opts.Image, "digest", digest)
	} else {
		r.log.Warn("sandbox: image not pinned by digest — tag-race risk; set NOMADDEV_SANDBOX_REQUIRE_DIGEST=true to enforce",
			"image", opts.Image)
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

// Exec implements Runner. It dispatches by req.Tool: execute_script runs a
// shell script and streams stdout/stderr chunks; search_syntax invokes
// `sg` (ast-grep) inside the container, buffers its JSON output, and
// emits a single reshaped envelope chunk.
func (r *DockerRunner) Exec(ctx context.Context, req ExecRequest) (<-chan ExecChunk, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.defaultTimeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	switch req.Tool {
	case ToolExecuteScript:
		shell, _ := req.Args["shell"].(string)
		if shell == "" {
			shell = "bash"
		}
		script, _ := req.Args["script"].(string)
		if script == "" {
			return nil, fmt.Errorf("%w: missing or empty 'script' arg", ErrBadRequest)
		}
		out := make(chan ExecChunk, 32)
		go r.runOne(ctx, req, shell, script, timeout, out)
		return out, nil
	case ToolSearchSyntax:
		argv, softMax, err := buildSearchSyntaxCmd(req.Args)
		if err != nil {
			return nil, err
		}
		out := make(chan ExecChunk, 8)
		go r.runSearchSyntax(ctx, req, argv, softMax, timeout, out)
		return out, nil
	}
	return nil, fmt.Errorf("%w: unknown tool %q", ErrBadRequest, req.Tool)
}

func (r *DockerRunner) runOne(
	parentCtx context.Context, req ExecRequest, shell, script string,
	timeout time.Duration, out chan<- ExecChunk,
) {
	defer close(out)

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	// Phase 11.3: per-run span. Noop when tracing is disabled.
	// Attributes capture the tool, session, shell, and timeout —
	// enough to correlate with the wsserver dispatch span and the
	// Prometheus latency histogram.
	tracer := otel.Tracer("nomaddev/sandbox/docker")
	ctx, span := tracer.Start(ctx, "sandbox.exec",
		trace.WithAttributes(
			attribute.String("sandbox.tool", req.Tool),
			attribute.String("sandbox.session_id", req.SessionID),
			attribute.String("sandbox.shell", shell),
			attribute.Int64("sandbox.timeout_ms", timeout.Milliseconds()),
		),
	)
	defer span.End()

	// 1. Pull the image if it isn't already cached locally. Inspecting first
	//    keeps a tight per-exec timeout (e.g. TestDocker_TimeoutKills's 500ms)
	//    from racing a registry round-trip when the image is already present —
	//    a problem operators hit in CI where the workflow pre-pulls images.
	if _, _, inspectErr := r.cli.ImageInspectWithRaw(ctx, r.image); inspectErr != nil {
		pullReader, err := r.cli.ImagePull(ctx, r.image, types.ImagePullOptions{})
		if err != nil {
			out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: fmt.Errorf("%w: %v", ErrImagePull, err)}
			return
		}
		_, _ = io.Copy(io.Discard, pullReader)
		_ = pullReader.Close()
	}

	// 1a. If the operator pinned the image by digest, verify that what
	//     ended up local actually matches. Docker validates digest-on-pull,
	//     but the cache path bypasses that and a malicious `docker tag`
	//     on the host could otherwise smuggle a different manifest under
	//     the configured name.
	if r.expectedDigest != "" {
		inspect, _, inspectErr := r.cli.ImageInspectWithRaw(ctx, r.image)
		if inspectErr != nil {
			out <- ExecChunk{Stream: StreamExit, ExitCode: -1,
				Err: fmt.Errorf("sandbox: post-pull inspect: %w", inspectErr)}
			return
		}
		if !MatchesRepoDigest(inspect.RepoDigests, r.expectedDigest) {
			r.log.Error("sandbox: image digest mismatch — refusing to run",
				"image", r.image,
				"expected", r.expectedDigest,
				"got_repo_digests", inspect.RepoDigests)
			out <- ExecChunk{Stream: StreamExit, ExitCode: -1,
				Err: fmt.Errorf("%w: image %q has RepoDigests %v, expected %s",
					ErrImageDigestMismatch, r.image, inspect.RepoDigests, r.expectedDigest)}
			return
		}
	}

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
		mountSrc := r.workspaceDir
		if r.perSessionWS && req.SessionID != "" {
			// Each SID gets its own subdir. Created at 0o700 so the
			// orchestrator user owns it; the container runs as a
			// different uid (or namespace-remapped uid when the
			// daemon is configured for that — see docs/sandbox.md).
			mountSrc = filepath.Join(r.workspaceDir, sanitizeSID(req.SessionID))
			if err := os.MkdirAll(mountSrc, 0o700); err != nil {
				out <- ExecChunk{Stream: StreamExit, ExitCode: -1,
					Err: fmt.Errorf("per-session workspace mkdir: %w", err)}
				return
			}
		}
		hostCfg.Mounts = append(hostCfg.Mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: mountSrc,
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

// runSearchSyntax runs `sg` inside an ephemeral container, buffers the
// captured stdout/stderr in memory (bounded by searchSyntaxStdoutCap),
// then emits a single JSON envelope chunk via reshapeMatches. The
// container plumbing mirrors runOne — kept as a parallel method rather
// than factored out so each tool's I/O policy stays at the leaf of the
// dispatch tree.
func (r *DockerRunner) runSearchSyntax(
	parentCtx context.Context, req ExecRequest, argv []string, softMaxMatches int,
	timeout time.Duration, out chan<- ExecChunk,
) {
	defer close(out)

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	tracer := otel.Tracer("nomaddev/sandbox/docker")
	ctx, span := tracer.Start(ctx, "sandbox.exec",
		trace.WithAttributes(
			attribute.String("sandbox.tool", req.Tool),
			attribute.String("sandbox.session_id", req.SessionID),
			attribute.Int64("sandbox.timeout_ms", timeout.Milliseconds()),
			attribute.Int("sandbox.search_syntax.max_matches", softMaxMatches),
		),
	)
	defer span.End()

	// Pull image if needed, with digest enforcement — same policy as runOne.
	if _, _, inspectErr := r.cli.ImageInspectWithRaw(ctx, r.image); inspectErr != nil {
		pullReader, err := r.cli.ImagePull(ctx, r.image, types.ImagePullOptions{})
		if err != nil {
			out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: fmt.Errorf("%w: %v", ErrImagePull, err)}
			return
		}
		_, _ = io.Copy(io.Discard, pullReader)
		_ = pullReader.Close()
	}
	if r.expectedDigest != "" {
		inspect, _, inspectErr := r.cli.ImageInspectWithRaw(ctx, r.image)
		if inspectErr != nil {
			out <- ExecChunk{Stream: StreamExit, ExitCode: -1,
				Err: fmt.Errorf("sandbox: post-pull inspect: %w", inspectErr)}
			return
		}
		if !MatchesRepoDigest(inspect.RepoDigests, r.expectedDigest) {
			out <- ExecChunk{Stream: StreamExit, ExitCode: -1,
				Err: fmt.Errorf("%w: image %q has RepoDigests %v, expected %s",
					ErrImageDigestMismatch, r.image, inspect.RepoDigests, r.expectedDigest)}
			return
		}
	}

	workingDir := "/work"
	if rel := cleanWorkdir(req.WorkingDir); rel != "" {
		workingDir = filepath.Join("/work", rel)
	}
	cfg := &container.Config{
		Image:        r.image,
		Cmd:          strslice.StrSlice(argv),
		WorkingDir:   workingDir,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
	}
	pidsLimit := r.limits.PidsLimit
	hostCfg := &container.HostConfig{
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
		mountSrc := r.workspaceDir
		if r.perSessionWS && req.SessionID != "" {
			mountSrc = filepath.Join(r.workspaceDir, sanitizeSID(req.SessionID))
			if err := os.MkdirAll(mountSrc, 0o700); err != nil {
				out <- ExecChunk{Stream: StreamExit, ExitCode: -1,
					Err: fmt.Errorf("per-session workspace mkdir: %w", err)}
				return
			}
		}
		hostCfg.Mounts = append(hostCfg.Mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: mountSrc,
			Target: "/work",
		})
	}

	create, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: fmt.Errorf("container create: %w", err)}
		return
	}
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

	stdoutBuf := &boundedBuffer{cap: searchSyntaxStdoutCap}
	stderrBuf := &boundedBuffer{cap: searchSyntaxStdoutCap}
	copyDone := make(chan struct{})
	go func() {
		_, _ = stdcopy.StdCopy(stdoutBuf, stderrBuf, attach.Reader)
		close(copyDone)
	}()

	statusCh, errCh := r.cli.ContainerWait(ctx, create.ID, container.WaitConditionNotRunning)
	var (
		exitCode int
		exitErr  error
	)
	select {
	case <-ctx.Done():
		_ = r.cli.ContainerKill(context.Background(), create.ID, "KILL")
		<-copyDone
		exitCode = -1
		exitErr = ctx.Err()
		if errors.Is(parentCtx.Err(), context.Canceled) {
			exitErr = ErrCanceled
		}
	case e := <-errCh:
		<-copyDone
		exitCode = -1
		exitErr = e
	case s := <-statusCh:
		<-copyDone
		exitCode = int(s.StatusCode)
		if s.Error != nil {
			exitErr = errors.New(s.Error.Message)
		}
		if s.StatusCode == 137 {
			if inspect, ierr := r.cli.ContainerInspect(context.Background(), create.ID); ierr == nil {
				if inspect.State != nil && inspect.State.OOMKilled {
					exitErr = ErrOOM
				}
			}
		}
	}

	// On transport / lifecycle errors, skip the reshape and surface the
	// raw error so the wsserver maps it to the right SandboxErr* code.
	if exitErr != nil {
		out <- ExecChunk{Stream: StreamExit, ExitCode: exitCode, Err: exitErr}
		return
	}

	envelope, err := reshapeMatches(stdoutBuf.Bytes(), stderrBuf.Bytes(), softMaxMatches, req.MaxResultBytes)
	if err != nil {
		out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: fmt.Errorf("reshape sg output: %w", err)}
		return
	}

	// Chunk the envelope onto stdout at chunkSliceBytes — same shape as
	// execute_script so the wsserver's emitChunk path needs no special
	// handling. Then exit with sg's status code so a non-zero exit (e.g.
	// "language not supported") still propagates.
	for off := 0; off < len(envelope); off += chunkSliceBytes {
		end := off + chunkSliceBytes
		if end > len(envelope) {
			end = len(envelope)
		}
		select {
		case out <- ExecChunk{Stream: StreamStdout, Data: append([]byte(nil), envelope[off:end]...)}:
		case <-ctx.Done():
			out <- ExecChunk{Stream: StreamExit, ExitCode: -1, Err: ctx.Err()}
			return
		}
	}
	out <- ExecChunk{Stream: StreamExit, ExitCode: exitCode}
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
