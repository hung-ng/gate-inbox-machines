package machines

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usestring/gate-inbox/extension"
)

func TestLoginSpawnThroughShells(t *testing.T) {
	for _, shell := range []string{"zsh", "bash", "sh"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skip(shell)
			}
			home := t.TempDir()
			bin := filepath.Join(home, "bin")
			os.Mkdir(bin, 0700)
			program := filepath.Join(bin, "gate-inbox")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			h := host{GateInbox: program, Tmux: "tmux", Destination: "box"}
			task := "it's $(touch /should-not-exist) =x"
			a := spawnArgv(config{SSH: []string{"ssh"}, ControlPath: "none"}, h, "agent", task, "/two words")
			cmd := exec.Command(path, "-c", a[len(a)-1])
			cmd.Env = []string{"HOME=" + home, "SHELL=" + path, "PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(out))
			}
			if !strings.Contains(string(out), "--prompt\n"+task+"\n--directory\n/two words\n--json") {
				t.Fatal(string(out))
			}
		})
	}
}

func TestMachinePathPrecedence(t *testing.T) {
	h := host{Tmux: "/chosen/tmux/tmux", GateInbox: "~/chosen/gate-inbox"}
	cmd := exec.Command("sh", "-c", machineCommand(h, "sh", "-c", `printf '%s' "$PATH"`))
	cmd.Env = []string{"HOME=/far/home", "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "/chosen/tmux:/far/home/chosen:/usr/bin:/bin:/far/home/go/bin:/far/home/.local/bin:/opt/homebrew/bin:/usr/local/bin"
	if string(out) != want {
		t.Fatalf("%s want %s", out, want)
	}
}

func TestMirrorMigrationGuards(t *testing.T) {
	e := New()
	e.conf = extension.NewConfig(nil).WithDataDir(t.TempDir())
	dir, _ := e.conf.DataDir()
	os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"mirrors":{"a1/ab":"cd"}}`), 0600)
	for _, l := range []extension.Launch{
		{From: "cd", Session: extension.SessionInfo{Tool: "agent"}},
		{Session: extension.SessionInfo{Tool: "agent", Role: mirrorRole}},
	} {
		if _, err := e.LaunchEnv(context.Background(), l); err == nil {
			t.Fatal("allowed mirror to launch agent", l)
		}
	}
	if _, err := e.LaunchEnv(context.Background(), extension.Launch{From: "cd", Session: extension.SessionInfo{Tool: toolName, Role: mirrorRole}}); err != nil {
		t.Fatal(err)
	}
}

func TestTwoMachinesKeepIndependentStatus(t *testing.T) {
	prev := withMirrors("mini/a1", "aa", "box/b2", "bb")
	local := rows(mirrorRow("aa", true), mirrorRow("bb", true))
	acts, next := plan(prev, []host{mini, box}, []observation{unreachable(mini), seen(box, live("b2", "waiting"))}, local, map[string]string{"aa": "working", "bb": "working"})
	got := strings.Join(describe(acts), ";")
	if got != "pin bb waiting" || !sameState(prev, next) {
		t.Fatal(got, next)
	}
}
