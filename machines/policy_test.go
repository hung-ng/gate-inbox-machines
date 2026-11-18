package machines

import (
	"context"
	"github.com/usestring/gate-inbox/extension"
	"os"
	"path/filepath"
	"testing"
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
