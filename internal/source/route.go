package source

import (
	"context"
	"fmt"
	"os/exec"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v failed: %w: %s", name, args, err, string(out))
	}
	return nil
}

type RouteManager struct {
	runner CommandRunner
}

func NewRouteManager(runner CommandRunner) *RouteManager {
	return &RouteManager{runner: runner}
}

func (m *RouteManager) EnsureLocalSubnet(ctx context.Context, subnet string) error {
	return m.runner.Run(ctx, "ip", "route", "replace", "local", subnet, "dev", "lo")
}
