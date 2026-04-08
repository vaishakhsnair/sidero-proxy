package source

import (
	"context"
	"testing"
)

type fakeRunner struct {
	name string
	args []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.name = name
	f.args = append([]string(nil), args...)
	return nil
}

func TestEnsureLocalSubnet(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager := NewRouteManager(runner)
	if err := manager.EnsureLocalSubnet(context.Background(), "10.1.0.0/16"); err != nil {
		t.Fatalf("EnsureLocalSubnet() error = %v", err)
	}
	if runner.name != "ip" {
		t.Fatalf("name = %q, want ip", runner.name)
	}
	want := []string{"route", "replace", "local", "10.1.0.0/16", "dev", "lo"}
	if len(runner.args) != len(want) {
		t.Fatalf("args = %v, want %v", runner.args, want)
	}
	for i := range want {
		if runner.args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q", i, runner.args[i], want[i])
		}
	}
}
