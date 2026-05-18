//go:build docker

package sandbox

import "context"

func newDockerRunner(ctx context.Context, c FactoryConfig) (Runner, error) {
	return NewDockerRunner(ctx, DockerRunnerOptions{
		Image:               c.Image,
		WorkspaceDir:        c.WorkspaceDir,
		DefaultTimeout:      c.DefaultTimeout,
		Limits:              c.Limits,
		ReadonlyRoot:        c.ReadonlyRoot,
		Network:             c.Network,
		PreferRunsc:         c.PreferRunsc,
		RequireDigest:       c.RequireDigest,
		PerSessionWorkspace: c.PerSessionWorkspace,
		Logger:              c.Logger,
	})
}
