package machines

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usestring/gate-inbox/extension"
)

func TestLaunchGuardOnlyChecksExplicitMirrorRole(t *testing.T) {
	e := New()
	e.conf = extension.NewConfig(nil).WithDataDir(t.TempDir())
	dir, _ := e.conf.DataDir()
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{bad json`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LaunchEnv(context.Background(), extension.Launch{Session: extension.SessionInfo{Tool: "agent", Role: mirrorRole}}); err == nil {
		t.Fatal("allowed a mirror to change tools")
	}
	for _, launch := range []extension.Launch{
		{From: "cd", Session: extension.SessionInfo{Tool: "agent"}},
		{From: "cd", Session: extension.SessionInfo{Tool: toolName, Role: mirrorRole}},
	} {
		if _, err := e.LaunchEnv(context.Background(), launch); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMirrorLaunchAndRestoreRequireTarget(t *testing.T) {
	dir := t.TempDir()
	e := New()
	e.conf = extension.NewConfig(nil).WithDataDir(dir)
	e.reg = newRegistry(machineStore(t, dir, mini))
	ctx := context.Background()
	launch := extension.Launch{Session: extension.SessionInfo{ID: "a1", Tool: toolName}}
	if _, err := e.LaunchEnv(ctx, launch); err == nil || !strings.Contains(err.Error(), "Shift+n") {
		t.Fatal(err)
	}
	launch.Session.Role = mirrorRole
	launch.Reason = extension.LaunchRelaunch
	if _, err := e.LaunchEnv(ctx, launch); err == nil || !strings.Contains(err.Error(), "no saved target") {
		t.Fatal(err)
	}
	saved := newState()
	saved.Dismissed["mini/b2"] = "a1"
	if err := writeJSON(filepath.Join(dir, stateFile), saved); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LaunchEnv(ctx, launch); err != nil {
		t.Fatal(err)
	}
}
