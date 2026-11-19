package machines

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usestring/gate-inbox/extension"
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
	if err != nil || strings.Join(tools, ",") != "agent,missing,terminal" {
		t.Fatal(tools, err)
	}
}

func TestInstalledToolsChecksConfiguredCommands(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "custom-agent")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("[tools.agent]\ncommand = %q\n", binary+" --example")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	tools, err := installedTools(context.Background(), toolsHost{dir: dir})
	if err != nil || strings.Join(tools, ",") != "agent,terminal" {
		t.Fatal(tools, err)
	}
}

func TestSpawnArgvAndForm(t *testing.T) {
	cfg := testConfig()
	cfg.ControlPath = "none"
	h := mini
	h.Home = "/some board"
	h.Tmux = "/opt/bin/tmux"
	argv := spawnArgv(cfg, h, spawnOptions{tool: "agent", prompt: "it's $(id)", directory: "/a path"})
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
	e.run = func(_ context.Context, args []string) ([]byte, error) {
		if args[len(args)-2] == "mini" {
			return []byte("hello from zsh\n[\"agent\"]\n"), nil
		}
		return nil, errors.New("older remote helper has no tools command")
	}
	f := &spawnForm{e: e, ctx: context.Background(), names: []string{"mini", "box"}}
	if len(f.Fields()) != 1 {
		t.Fatal(f.Fields())
	}
	if f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: map[string]string{"machine": "mini"}}) {
		t.Fatal("submitted machine choice")
	}
	waitFor(t, "installed tools", func() bool { return len(f.Fields()) == 7 && len(f.Fields()[1].Choices) > 0 })
	fields := f.Fields()
	if len(fields) != 7 || len(fields[1].Choices) != 1 || fields[6].Placeholder != "remote default" {
		t.Fatal(fields)
	}
	f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: map[string]string{"machine": "box"}})
	waitFor(t, "older helper fallback", func() bool {
		return strings.Contains(text(f.Render(80, 10)), "Could not check installed CLIs")
	})
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
		calls := make(chan []string, 2)
		e.run = func(_ context.Context, a []string) ([]byte, error) {
			if strings.Contains(a[len(a)-1], "machines tools --json") {
				return []byte(`["agent"]`), nil
			}
			calls <- a
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
		waitFor(t, "installed tools", func() bool { return len(f.Fields()) == 7 && len(f.Fields()[1].Choices) > 0 })
		select {
		case <-calls:
			t.Fatal("opening form ran ssh")
		default:
		}
		if len(f.Fields()) != 7 || f.machine != "mini" {
			t.Fatal(f.Fields())
		}
		values := map[string]string{"machine": "mini", "cli": "agent", "task": "hello", "directory": "/work", "name": "named", "model": "chosen", "group": "remote"}
		if f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: values}) {
			t.Fatal("form closed before launch completed")
		}
		select {
		case a := <-calls:
			if !strings.Contains(a[len(a)-1], "--tool agent --model chosen --name named --prompt hello --group remote --directory /work --json") {
				t.Fatal(a)
			}
		case <-time.After(time.Second):
			t.Fatal("no spawn")
		}
		if fail {
			waitFor(t, "inline launch failure", func() bool {
				return strings.Contains(text(f.Render(80, 10)), "CLI agent is not installed")
			})
		} else {
			select {
			case note := <-u.notifications:
				if note != "Started on mini · waiting for mirror" {
					t.Fatal(note)
				}
			case <-time.After(time.Second):
				t.Fatal("no notification")
			}
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

func TestSpawnFormShowsWhenMachineHasNoInstalledTools(t *testing.T) {
	e := New()
	e.reg = newRegistry(machineStore(t, t.TempDir(), mini))
	u := &spawnUI{notifications: make(chan string, 1)}
	e.ui = u
	e.run = func(_ context.Context, args []string) ([]byte, error) {
		if !strings.Contains(args[len(args)-1], "machines tools --json") {
			t.Errorf("unexpected command: %v", args)
		}
		return []byte("[]\n"), nil
	}
	e.openSpawn(context.Background(), "mini")
	f := u.opened.(*spawnForm)
	waitFor(t, "the empty picker", func() bool {
		return strings.Contains(text(f.Render(80, 10)), "No agent CLI is installed")
	})
	if len(f.Fields()) != 1 || f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: map[string]string{"machine": "mini", "cli": "claude"}}) {
		t.Fatal("an empty picker allowed a spawn")
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
			a := spawnArgv(config{SSH: []string{"ssh"}, ControlPath: "none"}, h, spawnOptions{tool: "agent", prompt: task, directory: "~/two words"})
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

func TestRemoteTerminalUsesWorkerHelper(t *testing.T) {
	argv := spawnArgv(testConfig(), mini, spawnOptions{tool: remoteTerminal, directory: "~/work"})
	remote := machineCommand(mini, mini.GateInbox, "machines", "terminal", "--tmux", mini.Tmux)
	want := `exec "$SHELL" -l -c ` + shellWord(remote+` --directory "$HOME"/work --json`)
	if command := argv[len(argv)-1]; command != want {
		t.Fatalf("%s\nwant %s", command, want)
	}
}
