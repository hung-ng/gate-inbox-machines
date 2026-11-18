package machines

import (
	"context"
	"errors"
	"github.com/usestring/gate-inbox/extension"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type toolsHost struct {
	extension.Host
	dir string
}

func (h toolsHost) ConfigDir() string { return h.dir }

func (h toolsHost) Tools(context.Context) ([]extension.ToolInfo, error) {
	return []extension.ToolInfo{{Name: "agent"}, {Name: "missing"}, {Name: "terminal", Shell: true}, {Name: "machines"}, {Name: "mirror"}}, nil
}

func TestAvailableToolsIncludesCommandsOutsidePATH(t *testing.T) {
	dir := t.TempDir()
	// No config.toml exists: the extension only uses Host.Tools.
	tools, err := availableTools(context.Background(), toolsHost{dir: dir})
	if err != nil || strings.Join(tools, ",") != "agent,missing" {
		t.Fatal(tools, err)
	}
}

func TestSpawnArgvAndForm(t *testing.T) {
	cfg := testConfig()
	cfg.ControlPath = "none"
	h := mini
	h.Home = "/some board"
	h.Tmux = "/opt/bin/tmux"
	argv := spawnArgv(cfg, h, "agent", "it's $(id)", "/a path")
	command := argv[len(argv)-1]
	want := `exec "$SHELL" -l -c ` + shellWord(machineCommand(h, h.GateInbox, "spawn", "--nest=false", "--tool", "agent", "--prompt", "it's $(id)", "--directory", "/a path", "--json"))
	if command != want {
		t.Fatalf("%s\nwant %s", command, want)
	}
	if strings.Contains(strings.Join(argv, " "), " -t ") {
		t.Fatal("interactive spawn")
	}
	e := New()
	e.reg = newRegistry(machineStore(t, t.TempDir(), mini, box))
	f := &spawnForm{e: e, names: []string{"mini", "box"}, known: map[string][]string{"mini": {"agent"}}}
	if len(f.Fields()) != 1 {
		t.Fatal(f.Fields())
	}
	if f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: map[string]string{"machine": "mini"}}) {
		t.Fatal("submitted machine choice")
	}
	fields := f.Fields()
	if len(fields) != 4 || len(fields[1].Choices) != 1 || fields[3].Placeholder != "that board's default" {
		t.Fatal(fields)
	}
	f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: map[string]string{"machine": "box"}})
	if len(f.Fields()[1].Choices) != 0 {
		t.Fatal("unknown tools must allow typing")
	}
	keys := e.machineUI().Keys
	if keys[0].Action != "new_on_machine" || keys[0].Keys[0] != "N" || keys[1].Keys[0] != "O" || keys[2].Screen != spawnScreen {
		t.Fatal(keys)
	}
}

type spawnUI struct {
	extension.UIHost
	notifications chan string
	opened        extension.View
	screen        string
}

func (u *spawnUI) Notify(s string) { u.notifications <- s }

func (u *spawnUI) Open(screen string, v extension.View) extension.ViewHandle {
	u.screen = screen
	u.opened = v
	return nil
}

func TestFormOpensSpawnsNotifiesAndWakesPoll(t *testing.T) {
	for _, fail := range []bool{false, true} {
		e := New()
		e.cfg = config{SSH: []string{"ssh"}, ControlPath: "none"}
		e.reg = newRegistry(machineStore(t, t.TempDir(), mini))
		u := &spawnUI{notifications: make(chan string, 1)}
		e.ui = u
		e.tools.set("mini", []string{"agent"})
		calls := make(chan []string, 2)
		e.run = func(_ context.Context, a []string) ([]byte, error) {
			calls <- a
			if strings.Contains(a[len(a)-1], "machines sessions") {
				return []byte(`{"version":1,"board_running":true,"tools":["agent"],"sessions":[]}`), nil
			}
			if fail {
				return nil, errors.New("CLI agent is not installed")
			}
			return []byte(`{"id":"a1"}`), nil
		}
		if err := e.machineUI().Keys[0].Run(context.Background(), extension.Press{}); err != nil {
			t.Fatal(err)
		}
		if u.screen != spawnScreen {
			t.Fatal(u.screen)
		}
		f := u.opened.(*spawnForm)
		select {
		case <-calls:
			t.Fatal("opening form ran ssh")
		default:
		}
		if len(f.Fields()) != 4 || f.machine != "mini" {
			t.Fatal(f.Fields())
		}
		values := map[string]string{"machine": "mini", "cli": "agent", "task": "hello", "directory": "/work"}
		if !f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: values}) {
			t.Fatal("form did not submit")
		}
		select {
		case a := <-calls:
			if !strings.Contains(a[len(a)-1], "--tool agent --prompt hello --directory /work --json") {
				t.Fatal(a)
			}
		case <-time.After(time.Second):
			t.Fatal("no spawn")
		}
		select {
		case note := <-u.notifications:
			if fail != strings.HasPrefix(note, "Could not") || (fail && !strings.Contains(note, "CLI agent is not installed")) {
				t.Fatal(note)
			}
		case <-time.After(time.Second):
			t.Fatal("no notification")
		}
		select {
		case name := <-e.wake:
			if name != "mini" {
				t.Fatal(name)
			}
		case <-time.After(time.Second):
			t.Fatal("poll not woken")
		}
	}
}

func TestCachedToolsSurviveFailureAndAreCopied(t *testing.T) {
	e := New()
	e.cfg = config{Poll: defaultPoll, SSH: []string{"ssh"}}
	replies := hosts{"mini": snapshot{Version: wireVersion, Tools: []string{"agent"}, Sessions: []machineSession{}}}
	m := newMirrors(e.cfg, newFakeBoard(), nil, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	m.tools = &e.tools
	m.observe(context.Background(), m.reg.active())
	replies["mini"] = context.DeadlineExceeded
	m.observe(context.Background(), m.reg.active())
	copy := e.tools.copy()
	if strings.Join(copy["mini"], ",") != "agent" {
		t.Fatal(copy)
	}
	copy["mini"][0] = "changed"
	if e.tools.copy()["mini"][0] != "agent" {
		t.Fatal("shared slice")
	}
}

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
			task := "~/it's $(touch /should-not-exist) =x"
			a := spawnArgv(config{SSH: []string{"ssh"}, ControlPath: "none"}, h, "agent", task, "~/two words")
			cmd := exec.Command(path, "-c", a[len(a)-1])
			cmd.Env = []string{"HOME=" + home, "SHELL=" + path, "PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(out))
			}
			if !strings.Contains(string(out), "--prompt\n"+task+"\n--directory\n"+home+"/two words\n--json") {
				t.Fatal(string(out))
			}
		})
	}
}
