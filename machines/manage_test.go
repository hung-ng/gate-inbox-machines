package machines

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

// farMachine answers every command the extension sends a machine, the way
// a machine running this build would.
type farMachine struct {
	mu    sync.Mutex
	calls []string
	// sshErr fails every command, as ssh does when it can't log in.
	sshErr error
	snap   snapshot
	// hidden is a gate-inbox only the login shell finds, at loginPath.
	hidden    bool
	loginPath string
}

func healthyFar() *farMachine {
	socket := ""
	return &farMachine{snap: snapshot{Version: wireVersion, BoardRunning: boolPtr(true), About: &about{
		Hostname: "Box-1.local", OS: "linux", GateInbox: "/home/me/go/bin/gate-inbox",
		Tmux: "/usr/bin/tmux", TmuxSocket: &socket, Version: Version,
	}}}
}

func (f *farMachine) run(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := argv[len(argv)-1]
	f.calls = append(f.calls, cmd)
	if f.sshErr != nil {
		return nil, f.sshErr
	}
	login := strings.HasPrefix(cmd, `exec "$SHELL" -l -c`)
	found := !f.hidden || login || (f.loginPath != "" && strings.Contains(cmd, f.loginPath+" "))
	switch {
	case strings.Contains(cmd, "command -v"):
		if f.loginPath == "" {
			return []byte("hello from .zprofile\n"), nil
		}
		return []byte("hello from .zprofile\n" + f.loginPath + "\n"), nil
	case strings.Contains(cmd, "machines sessions"):
		if !found {
			return nil, exitStatus(commandNotFound)
		}
		data, err := json.Marshal(f.snap)
		if login {
			data = append([]byte("hello from .zprofile\n"), data...)
		}
		return data, err
	case strings.Contains(cmd, "--version"), strings.Contains(cmd, "machines --help"):
		if !found {
			return nil, exitStatus(commandNotFound)
		}
	}
	return nil, nil
}

func (f *farMachine) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sshErr = err
}

// newManaged is an extension with a data dir, config.toml's machines base,
// and far answering for every machine.
func newManaged(t *testing.T, far *farMachine, base ...host) *Extension {
	t.Helper()
	dir := t.TempDir()
	e := New()
	e.conf = extension.NewConfig(nil).WithDataDir(dir)
	for i := range base {
		base[i].Source = sourceConfig
	}
	e.cfg = config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none", Hosts: base}
	e.reg = newRegistry(base, dir)
	e.run = far.run
	return e
}

func TestAboutCleanDropsWhatItCantUse(t *testing.T) {
	bad := "no/slash"
	a := about{Hostname: "two words", OS: "Darwin!", GateInbox: "bin/gate-inbox", Tmux: "/usr/bin/tmux\nrm", TmuxSocket: &bad, Version: "0.2.0"}.clean()
	if a.Hostname != "" || a.OS != "" || a.GateInbox != "" || a.Tmux != "" || a.TmuxSocket != nil || a.Version != "0.2.0" {
		t.Fatalf("%+v", a)
	}
}

func TestOperatorSettingsWinOverWhatAMachineSays(t *testing.T) {
	socket := "agents"
	said := about{GateInbox: "/far/gate-inbox", Tmux: "/far/tmux", TmuxSocket: &socket}
	h, _ := resolveHost("box", hostSettings{Tmux: "/mine/tmux"})
	h = h.withAbout(said)
	if h.Tmux != "/mine/tmux" || h.GateInbox != "/far/gate-inbox" || h.TmuxSocket != "agents" {
		t.Fatalf("%+v", h)
	}
	h, _ = resolveHost("box", hostSettings{TmuxSocket: "mine"})
	if h = h.withAbout(said); h.TmuxSocket != "mine" {
		t.Fatal(h.TmuxSocket)
	}
	empty := ""
	h, _ = resolveHost("box", hostSettings{})
	if h = h.withAbout(about{TmuxSocket: &empty}); h.TmuxSocket != "" || h.GateInbox != "gate-inbox" {
		t.Fatalf("%+v", h)
	}
}

func TestSuggestName(t *testing.T) {
	none := func(string) bool { return false }
	for _, tc := range []struct{ hostname, destination, want string }{
		{"hung-mini", "x", "hung-mini"},
		{"Box-1.local", "", "box-1"},
		{"", "me@Box.Example.com", "box"},
		{"", "10.0.0.7", "m-10"},
		{"poll", "poll.example", "machine"},
		{"", "", "machine"},
	} {
		if got := suggestName(tc.hostname, tc.destination, none); got != tc.want {
			t.Errorf("suggestName(%q, %q) = %q, want %q", tc.hostname, tc.destination, got, tc.want)
		}
	}
	taken := func(n string) bool { return n == "box" || n == "box-2" }
	if got := suggestName("box", "", taken); got != "box-3" {
		t.Fatal(got)
	}
}

func TestBoardSocketReadsTheBoardsOwnSetting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(socketEnv, "")
	if socket, ok := boardSocket(dir); !ok || socket != "" {
		t.Fatal(socket, ok)
	}
	t.Setenv(socketEnv, "from-env")
	if socket, _ := boardSocket(dir); socket != "from-env" {
		t.Fatal(socket)
	}
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("poll_interval = \"2s\"\ntmux_socket = \"agents\"\n[tools.x]\ncommand = \"x\"\n"), 0o600)
	if socket, _ := boardSocket(dir); socket != "agents" {
		t.Fatal(socket)
	}
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("tmux_socket = [\n"), 0o600)
	if _, ok := boardSocket(dir); ok {
		t.Fatal("a config that does not parse can't say")
	}
}

func TestDescribeSelf(t *testing.T) {
	a := describeSelf(t.TempDir(), "sh")
	if !strings.HasPrefix(a.Tmux, "/") || !strings.HasPrefix(a.GateInbox, "/") || a.Version != Version || a.OS == "" || a.TmuxSocket == nil {
		t.Fatalf("%+v", a)
	}
	if !a.clean().equal(a) {
		t.Fatalf("this machine's own description does not survive clean: %+v", a)
	}
}

func TestSnapshotWithAboutAndAGreeting(t *testing.T) {
	far := healthyFar()
	data, _ := json.Marshal(far.snap)
	snap, err := parseSnapshotAfterGreeting(append([]byte("Last login: today\nwelcome {not json\n"), data...))
	if err != nil || snap.About == nil || snap.About.Hostname != "Box-1.local" {
		t.Fatal(snap, err)
	}
	if _, err := parseSnapshotAfterGreeting([]byte("only a greeting\n")); err == nil {
		t.Fatal("a greeting alone is not a reply")
	}
}

func TestAddNamesTheMachineAfterItsHostnameAndLearnsItsPaths(t *testing.T) {
	far := healthyFar()
	e := newManaged(t, far, mini)
	var out bytes.Buffer
	h, err := e.addMachine(context.Background(), addOptions{Destination: "me@box.example"}, &out, nil)
	if err != nil {
		t.Fatal(err, out.String())
	}
	if h.Name != "box-1" || h.Source != sourceAdded || h.GateInbox != "/home/me/go/bin/gate-inbox" || h.Tmux != "/usr/bin/tmux" {
		t.Fatalf("%+v\n%s", h, out.String())
	}
	if !strings.Contains(out.String(), "it reports hostname Box-1.local") {
		t.Fatal(out.String())
	}
	// Another process of this build reads the same files.
	other := newRegistry([]host{mini}, e.reg.dir)
	if got, ok := other.host("box-1"); !ok || got.Tmux != "/usr/bin/tmux" {
		t.Fatalf("%+v", got)
	}
	for _, opts := range []addOptions{
		{Destination: "me@box.example"},
		{Destination: "other", Name: "mini"},
		{Destination: "other", Name: "poll"},
		{Destination: "-oProxyCommand=x"},
		{Destination: "two words"},
	} {
		if _, err := e.addMachine(context.Background(), opts, &out, nil); err == nil {
			t.Errorf("%+v was added", opts)
		}
	}
}

func TestAddStopsAtAKeyItNeedsATerminalFor(t *testing.T) {
	far := healthyFar()
	far.sshErr = errors.New("exit status 255: me@box: Permission denied (publickey).")
	e := newManaged(t, far)
	var out bytes.Buffer
	_, err := e.addMachine(context.Background(), addOptions{Destination: "me@box"}, &out, nil)
	var needs errNeedsTerminal
	if !errors.As(err, &needs) || needs.Fix != fixKey {
		t.Fatal(err)
	}
	if len(e.machines().all()) != 0 {
		t.Fatal("added a machine it can't log in to")
	}
	var ran [][]string
	term := &terminal{in: bufioReader("\n"), out: &out, run: func(_ context.Context, argv []string) (int, error) {
		ran = append(ran, argv)
		far.setErr(nil)
		return 0, nil
	}}
	h, err := e.addMachine(context.Background(), addOptions{Destination: "me@box"}, &out, term)
	if err != nil || h.Name != "box-1" {
		t.Fatal(err, out.String())
	}
	if len(ran) != 1 || strings.Join(ran[0], " ") != "ssh-copy-id me@box" {
		t.Fatal(ran)
	}
}

func TestAddAcceptsAHostKeyAtATerminal(t *testing.T) {
	far := healthyFar()
	far.sshErr = errors.New("exit status 255: Host key verification failed.")
	e := newManaged(t, far)
	var ran []string
	var out bytes.Buffer
	term := &terminal{in: bufioReader("y\n"), out: &out, run: func(_ context.Context, argv []string) (int, error) {
		ran = argv
		far.setErr(nil)
		return 0, nil
	}}
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &out, term); err != nil {
		t.Fatal(err, out.String())
	}
	if strings.Join(ran, " ") != "ssh -o BatchMode=no -o ControlPath=none -- box true" {
		t.Fatal(ran)
	}
}

func TestAddRefusesABrokenMachineUnlessForced(t *testing.T) {
	far := healthyFar()
	far.hidden = true
	e := newManaged(t, far)
	var out bytes.Buffer
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &out, nil); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatal(err, out.String())
	}
	h, err := e.addMachine(context.Background(), addOptions{Destination: "box", Force: true}, &out, nil)
	if err != nil || h.Name != "box" {
		t.Fatal(err)
	}
}

func TestAddFindsGateInboxThroughTheLoginShell(t *testing.T) {
	far := healthyFar()
	far.hidden, far.loginPath = true, "/home/me/go/bin/gate-inbox"
	e := newManaged(t, far)
	var out bytes.Buffer
	h, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &out, nil)
	if err != nil {
		t.Fatal(err, out.String())
	}
	if !strings.Contains(out.String(), "gate-inbox OK (found through your login shell at /home/me/go/bin/gate-inbox)") || h.GateInbox != far.loginPath {
		t.Fatal(out.String(), h)
	}
}

func TestAddKeepsAMachineWhoseBoardIsStopped(t *testing.T) {
	far := healthyFar()
	far.snap.BoardRunning = boolPtr(false)
	e := newManaged(t, far)
	var out bytes.Buffer
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &out, nil); err != nil {
		t.Fatal(err, out.String())
	}
	if !strings.Contains(out.String(), "board running FAIL") {
		t.Fatal(out.String())
	}
}

func TestCheckWarnsWhenTheBuildsDiffer(t *testing.T) {
	far := healthyFar()
	far.snap.About.Version = "0.1.9"
	h := mini
	e := newManaged(t, far, h)
	var out bytes.Buffer
	if err := e.runCheck(context.Background(), nil, &out); err != nil {
		t.Fatal("a version warning failed the check:", err, out.String())
	}
	if !strings.Contains(out.String(), "version WARN (that machine runs machines 0.1.9 and this one runs "+Version+")") {
		t.Fatal(out.String())
	}
	if got, _ := e.machines().host("mini"); got.About == nil || got.Tmux != "/usr/bin/tmux" {
		t.Fatal("check did not keep what the machine said", got)
	}
}

func TestRemovePauseAndRename(t *testing.T) {
	far := healthyFar()
	e := newManaged(t, far, mini)
	var out bytes.Buffer
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box", Name: "box"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.pauseMachine("mini", true); err != nil {
		t.Fatal(err)
	}
	if active := e.machines().active(); len(active) != 1 || active[0].Name != "box" {
		t.Fatal(active)
	}
	if err := e.pauseMachine("nobody", true); err == nil {
		t.Fatal("paused a machine that isn't there")
	}
	if err := e.renameMachine("mini", "mini2"); err == nil {
		t.Fatal("renamed a machine config.toml names")
	}
	if err := e.renameMachine("box", "mini"); err == nil {
		t.Fatal("renamed onto a taken name")
	}
	if err := e.renameMachine("box", "big"); err != nil {
		t.Fatal(err)
	}
	if h, ok := e.machines().host("big"); !ok || h.About == nil || h.Tmux != "/usr/bin/tmux" {
		t.Fatal("the new name lost what the machine said", h)
	}
	if err := e.removeMachine("mini"); err == nil || !strings.Contains(err.Error(), "config.toml") {
		t.Fatal(err)
	}
	if err := e.removeMachine("big"); err != nil {
		t.Fatal(err)
	}
	if all := e.machines().all(); len(all) != 1 || all[0].Name != "mini" || !all[0].Paused {
		t.Fatal(all)
	}
}

func TestRegistryFollowsOtherProcesses(t *testing.T) {
	dir := t.TempDir()
	board := newRegistry(nil, dir)
	shell := newRegistry(nil, dir)
	if err := shell.change(func(s *storeFile, _ []host) error {
		s.Machines["box"] = hostSettings{Destination: "me@box"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !board.reload() || len(board.all()) != 1 {
		t.Fatal(board.all())
	}
	if board.reload() {
		t.Fatal("an unchanged file read as a change")
	}
	board.learn("box", "me@box", about{Hostname: "box"})
	shell.learn("other", "other", about{Hostname: "other"})
	board.reload()
	if h, _ := board.host("box"); h.About == nil || h.About.Hostname != "box" {
		t.Fatal("one process's learning overwrote another's", h)
	}
	// What a machine said under a destination it no longer has is ignored.
	shell.change(func(s *storeFile, _ []host) error {
		s.Machines["box"] = hostSettings{Destination: "me@elsewhere"}
		return nil
	})
	board.reload()
	if h, _ := board.host("box"); h.About != nil {
		t.Fatal(h)
	}
	// A file broken by hand keeps the last good list.
	os.WriteFile(filepath.Join(dir, storeFileName), []byte("{"), 0o600)
	board.reload()
	if len(board.all()) != 1 || board.issue() == "" {
		t.Fatal(board.all(), board.issue())
	}
}

func TestConfigWinsANameClash(t *testing.T) {
	dir := t.TempDir()
	base := []host{{Name: "mini", Destination: "from-config", GateInbox: "gate-inbox", Tmux: "tmux", Source: sourceConfig}}
	os.WriteFile(filepath.Join(dir, storeFileName), []byte(`{"machines":{"mini":{"destination":"from-store"},"Bad Name":{"destination":"x"}}}`), 0o600)
	r := newRegistry(base, dir)
	if all := r.all(); len(all) != 1 || all[0].Destination != "from-config" {
		t.Fatal(all)
	}
	if !strings.Contains(r.issue(), "Bad Name") {
		t.Fatal(r.issue())
	}
}

// A machine whose board keeps its sessions on a named tmux server says so;
// the first poll asked about tmux's default server, so the board asks again
// before mirroring anything.
func TestPollLearnsTheBoardsSocketAndAsksAgain(t *testing.T) {
	socket := "agents"
	var asked []string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		cmd := argv[len(argv)-1]
		asked = append(asked, cmd)
		onSocket := strings.Contains(cmd, "--tmux-socket agents")
		snap := snapshot{Version: wireVersion, BoardRunning: boolPtr(true), Sessions: []machineSession{{ID: "a1", Status: "waiting", Running: true, Attachable: onSocket}},
			About: &about{Tmux: "/opt/homebrew/bin/tmux", TmuxSocket: &socket, GateInbox: "/far/gate-inbox", Version: Version}}
		return json.Marshal(snap)
	}
	b := newFakeBoard()
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none", Hosts: []host{mini}}
	m := newMirrors(cfg, b, nil, quiet(), run, t.TempDir())
	m.pass(context.Background())
	if len(asked) != 2 || !strings.Contains(asked[1], "--tmux /opt/homebrew/bin/tmux --tmux-socket agents") {
		t.Fatal(asked)
	}
	if len(b.launches) != 1 {
		t.Fatal("the session on the board's socket was not mirrored", b.launches)
	}
	h, _ := m.reg.host("mini")
	if argv := attachArgv(cfg, h, "a1"); !strings.Contains(argv[len(argv)-1], "exec /opt/homebrew/bin/tmux -u -L agents attach-session") {
		t.Fatal(argv)
	}
	m.pass(context.Background())
	if len(asked) != 3 {
		t.Fatal("asked again once it knew", asked)
	}
}

func TestPollHostTriesTheLoginShellOnce(t *testing.T) {
	far := healthyFar()
	far.hidden = true
	h, _ := resolveHost("box", hostSettings{Destination: "box"})
	snap, err := pollHost(context.Background(), far.run, config{SSH: []string{"ssh"}, ControlPath: "none"}, h)
	if err != nil || snap.About == nil {
		t.Fatal(snap, err)
	}
	if len(far.calls) != 2 || !strings.HasPrefix(far.calls[1], `exec "$SHELL" -l -c`) {
		t.Fatal(far.calls)
	}
	far.calls = nil
	h, _ = resolveHost("box", hostSettings{Destination: "box", GateInbox: "/set/by/me"})
	if _, err := pollHost(context.Background(), far.run, config{SSH: []string{"ssh"}, ControlPath: "none"}, h); err == nil || len(far.calls) != 1 {
		t.Fatal("a path the operator set must not be second-guessed", far.calls)
	}
}

func TestPassPutsAwayAPausedMachinesMirrorsAndClosesFinishedSetupRows(t *testing.T) {
	dir := t.TempDir()
	b := newFakeBoard()
	b.sessions["s1"] = extension.SessionInfo{ID: "s1", Tool: toolName, Role: setupRole, Running: false}
	b.sessions["s2"] = extension.SessionInfo{ID: "s2", Tool: toolName, Role: setupRole, Running: true}
	replies := hosts{"mini": snapshot{Version: wireVersion, BoardRunning: boolPtr(true), Sessions: []machineSession{live("a1", "waiting")}}}
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none", Hosts: []host{mini}}
	m := newMirrors(cfg, b, nil, quiet(), replies.run, dir)
	m.pass(context.Background())
	if len(b.launches) != 1 || !slices.Contains(b.archives, "s1") || slices.Contains(b.archives, "s2") {
		t.Fatal(b.launches, b.archives)
	}
	m.reg.change(func(s *storeFile, _ []host) error {
		s.Paused = append(s.Paused, "mini")
		return nil
	})
	m.pass(context.Background())
	if !slices.Contains(b.archives, "l1") {
		t.Fatal("a paused machine kept its mirror", b.archives)
	}
	if len(m.state.Dismissed) != 0 {
		t.Fatal("a paused machine's mirrors are not the operator's dismissals", m.state)
	}
}

func TestSSHConfigHosts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh", "conf.d"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(`# mine
Host devbox box2 *.wild !neg
  HostName 10.0.0.2
Host=eq-host
host "two words" quoted
Match host x
Include conf.d/*
Include ~/.ssh/config
`), 0o600)
	os.WriteFile(filepath.Join(home, ".ssh", "conf.d", "work"), []byte("Host work-1\nHost devbox\n"), 0o600)
	got := sshConfigHosts(filepath.Join(home, ".ssh", "config"))
	if strings.Join(got, ",") != "devbox,box2,eq-host,quoted,work-1" {
		t.Fatal(got)
	}
	if sshConfigHosts(filepath.Join(home, "missing")) != nil {
		t.Fatal("a missing file has hosts")
	}
}

func TestDiscoverCommand(t *testing.T) {
	got, err := discoverCommand(context.Background(), []string{"sh", "-c", `printf 'a.example.ts.net.\n# note\n\nb extra words\n-bad\na.example.ts.net\n'`})
	if err != nil || strings.Join(got, ",") != "a.example.ts.net,b" {
		t.Fatal(got, err)
	}
	if _, err := discoverCommand(context.Background(), []string{"sh", "-c", "echo nope >&2; exit 3"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatal(err)
	}
	if got, err := discoverCommand(context.Background(), nil); got != nil || err != nil {
		t.Fatal(got, err)
	}
}

func TestProbeSummary(t *testing.T) {
	for _, tc := range []struct {
		snap  snapshot
		err   error
		want  string
		ready bool
	}{
		{snapshot{BoardRunning: boolPtr(true)}, nil, "ready, board running", true},
		{snapshot{BoardRunning: boolPtr(false)}, nil, "ready, board stopped", true},
		{snapshot{}, exitStatus(commandNotFound), "no gate-inbox found there", false},
		{snapshot{}, errors.New("Permission denied (publickey)"), "needs your SSH key", false},
		{snapshot{}, errors.New("Host key verification failed."), "host key not accepted yet", false},
		{snapshot{}, errors.New("the machine's reply is not a session list: x"), "runs a gate-inbox without this extension", false},
		{snapshot{}, errors.New("Connection timed out"), "can't connect", false},
	} {
		if got, ready := probeSummary(tc.snap, tc.err); got != tc.want || ready != tc.ready {
			t.Errorf("%v: %q %v", tc.err, got, ready)
		}
	}
}

// start-board's command must reach tmux word for word through each login
// shell, home and all.
func TestStartBoardThroughShells(t *testing.T) {
	for _, shell := range []string{"zsh", "bash", "sh"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skip(shell)
			}
			home := t.TempDir()
			tmux := filepath.Join(home, "tmux")
			if err := os.WriteFile(tmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			h := host{Destination: "box", Tmux: tmux, GateInbox: "/far/bin/gate-inbox", Home: "~/gi home"}
			argv := startBoardArgv(config{SSH: []string{"ssh"}, ControlPath: "none"}, h)
			cmd := exec.Command(path, "-c", argv[len(argv)-1])
			cmd.Env = []string{"HOME=" + home, "SHELL=" + path, "PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(out))
			}
			want := "new-session\n-d\n-s\ngate-inbox-board\n--\nenv\nGATE_INBOX_HOME=" + home + "/gi home\n/far/bin/gate-inbox\n"
			if string(out) != want {
				t.Fatalf("%q\nwant %q", out, want)
			}
		})
	}
}

func TestStartBoardSkipsARunningBoard(t *testing.T) {
	far := healthyFar()
	e := newManaged(t, far, mini)
	msg, err := e.startBoard(context.Background(), mini)
	if err != nil || !strings.Contains(msg, "already running") {
		t.Fatal(msg, err)
	}
	far.snap.BoardRunning = boolPtr(false)
	far.snap.About.OS = "darwin"
	msg, err = e.startBoard(context.Background(), mini)
	if err != nil || !strings.Contains(msg, "tmux session gate-inbox-board") || !strings.Contains(msg, "keychain") {
		t.Fatal(msg, err)
	}
	if last := far.calls[len(far.calls)-1]; !strings.Contains(last, "new-session") || !strings.Contains(last, "/home/me/go/bin/gate-inbox") {
		t.Fatal(last)
	}
}

func bufioReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

// screenUI is the board's UI as the machines screen sees it.
type screenUI struct {
	extension.UIHost
	mu       sync.Mutex
	views    []extension.View
	screens  []string
	notes    []string
	refresh  int
	closed   int
	handlers []*screenHandle
}

type screenHandle struct {
	ui   *screenUI
	view extension.View
}

func (h *screenHandle) Refresh() {
	h.ui.mu.Lock()
	h.ui.refresh++
	h.ui.mu.Unlock()
}

func (h *screenHandle) Close() {
	h.ui.mu.Lock()
	h.ui.closed++
	h.ui.mu.Unlock()
	if c, ok := h.view.(extension.Closer); ok {
		c.Closed(extension.CloseHandle)
	}
}

func (u *screenUI) Open(screen string, v extension.View) extension.ViewHandle {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.views = append(u.views, v)
	u.screens = append(u.screens, screen)
	return &screenHandle{ui: u, view: v}
}

func (u *screenUI) Notify(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.notes = append(u.notes, text)
}

func (u *screenUI) last() (string, extension.View) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.screens[len(u.screens)-1], u.views[len(u.views)-1]
}

func text(lines []extension.Line) string {
	var b strings.Builder
	for _, line := range lines {
		for _, span := range line {
			b.WriteString(span.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMachinesScreen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Host devbox mini\n"), 0o600)
	far := healthyFar()
	e := newManaged(t, far, mini)
	u := &screenUI{}
	e.ui = u
	e.status.set(map[string]machineState{"mini": {BoardRunning: true, Mirrored: 2, NotMirrored: 1}})
	e.openMachines(context.Background())
	screen, view := u.last()
	v := view.(*machinesView)
	if screen != machinesScreen {
		t.Fatal(screen)
	}
	waitFor(t, "suggestions", func() bool { return strings.Contains(text(v.Render(120, 40)), "devbox") })
	body := text(v.Render(120, 40))
	if !strings.Contains(body, "› mini  online · 2 sessions · 1 not mirrored  mini · config.toml") {
		t.Fatal(body)
	}
	if strings.Count(body, "mini") != 2 {
		t.Fatal("a machine on the board is offered again:\n" + body)
	}
	if !strings.Contains(body, "devbox  not checked") {
		t.Fatal(body)
	}
	// Space pauses mini; the screen reads the store again.
	v.Key(extension.ViewKey{Action: actionPause, Key: "space"})
	if h, _ := e.machines().host("mini"); !h.Paused || !strings.Contains(text(v.Render(120, 40)), "mini  paused") {
		t.Fatal(text(v.Render(120, 40)))
	}
	// Enter on a suggestion opens the add form with it filled in.
	v.Key(extension.ViewKey{Key: "down"})
	v.Key(extension.ViewKey{Key: "enter"})
	screen, view = u.last()
	if screen != addScreen || view.(*addForm).Fields()[0].Value != "devbox" {
		t.Fatal(screen)
	}
	v.Closed(extension.CloseReplaced)
}

func TestMachinesScreenRemovesOnTheSecondPress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	far := healthyFar()
	e := newManaged(t, far)
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box", Name: "box"}, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	u := &screenUI{}
	e.ui = u
	e.openMachines(context.Background())
	_, view := u.last()
	v := view.(*machinesView)
	v.Key(extension.ViewKey{Action: actionRemove, Key: "d"})
	if len(e.machines().all()) != 1 || !strings.Contains(text(v.Render(120, 40)), "Press d again to remove box") {
		t.Fatal(text(v.Render(120, 40)))
	}
	v.Key(extension.ViewKey{Action: actionRemove, Key: "d"})
	if len(e.machines().all()) != 0 {
		t.Fatal("not removed")
	}
	if !strings.Contains(text(v.Render(120, 40)), "No machines yet") {
		t.Fatal(text(v.Render(120, 40)))
	}
}

func TestAddFormAddsOrOpensASetupRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	far := healthyFar()
	far.sshErr = errors.New("exit status 255: Permission denied (publickey).")
	e := newManaged(t, far)
	b := newFakeBoard()
	e.board = b
	u := &screenUI{}
	e.ui = u
	e.openAdd(context.Background(), "")
	_, view := u.last()
	f := view.(*addForm)
	values := map[string]string{"destination": "me@box", "name": "", "group": ""}
	f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: values})
	waitFor(t, "the key problem", func() bool { return strings.Contains(text(f.Render(120, 40)), "ctrl+f") })
	if !f.Key(extension.ViewKey{Action: actionFix, Key: "ctrl+f", Values: values}) {
		t.Fatal("the fix did not close the form")
	}
	if len(b.launches) != 1 {
		t.Fatal(b.launches)
	}
	req := b.launches[0]
	if req.Role != setupRoleName || req.Tool != toolName || strings.Join(req.Args, " ") != "--add -- me@box" || req.Name != "add-box" {
		t.Fatalf("%+v", req)
	}
	if !slices.Contains(b.pins, "l1=waiting") {
		t.Fatal(b.pins)
	}
	// The form handed the operator a row; closing it leaves them there.
	f.Closed(extension.CloseReturned)
	if screen, _ := u.last(); screen != addScreen {
		t.Fatal("reopened the machines screen over the setup row")
	}

	far.setErr(nil)
	e.openAdd(context.Background(), "")
	_, view = u.last()
	f = view.(*addForm)
	f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: values})
	waitFor(t, "the add", func() bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		return len(u.notes) > 1
	})
	if u.notes[len(u.notes)-1] != "Added box-1 (me@box)" {
		t.Fatal(u.notes)
	}
	waitFor(t, "the machines screen", func() bool { s, _ := u.last(); return s == machinesScreen })
}

// A setup row's pane runs `machines add` through the mirror tool's
// command, `machines attach`.
func TestAttachRunsASetupPane(t *testing.T) {
	far := healthyFar()
	e := newManaged(t, far)
	stdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = stdin })
	w.WriteString("\n")
	w.Close()
	stdout := os.Stdout
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdout = devnull
	t.Setenv("GATE_INBOX_SESSION_ID", "abcd")
	err := e.runAttach(context.Background(), []string{"", setupFlag, "--name", "box", "--", "me@box"}, nil)
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := e.machines().host("box"); !ok || h.Destination != "me@box" {
		t.Fatal(e.machines().all())
	}
	if !setupDone(e.machines().dir, "abcd") {
		t.Fatal("the pane did not mark its row done")
	}
}

func TestWrapLinesBreaksLongNotesAtSpaces(t *testing.T) {
	lines := wrapLines([]extension.Line{
		{{Text: "short"}},
		{{Text: "one two three four five six", Tone: extension.ToneBad}},
		{{Text: "a"}, {Text: "two spans are left alone however long they get"}},
	}, 10)
	got := strings.TrimSpace(text(lines))
	if got != "short\none two\nthree four\nfive six\natwo spans are left alone however long they get" {
		t.Fatalf("%q", got)
	}
	if lines[2][0].Tone != extension.ToneBad {
		t.Fatal("a wrapped line lost its tone")
	}
}

func TestPreviewSaysWhyAStoppedBoardsMirrorHasNone(t *testing.T) {
	var routes previewRoutes
	obs := seen(mini, live("a1", "waiting"))
	obs.Snapshot.BoardRunning = boolPtr(false)
	routes.update(withMirrors("mini/a1", "l1"), []observation{obs})
	if _, ok := routes.lookup("l1"); ok {
		t.Fatal("a stopped board's mirror has a preview route")
	}
	if err := routes.unavailable("l1"); err == nil || err.Error() != "no preview until mini's board is running" {
		t.Fatal(err)
	}
	if err := routes.unavailable("l9"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatal(err)
	}
}

// A setup pane's command ends on a shell, so the row still reads as
// running; the pane marks itself done and the next pass archives it.
func TestPassArchivesASetupRowThatMarkedItselfDone(t *testing.T) {
	dir := t.TempDir()
	b := newFakeBoard()
	b.sessions["ab12"] = extension.SessionInfo{ID: "ab12", Tool: toolName, Role: setupRole, Running: true}
	m := newMirrors(config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none"}, b, nil, quiet(), hosts{}.run, dir)
	m.pass(context.Background())
	if len(b.archives) != 0 {
		t.Fatal("archived a setup row still in use")
	}
	if err := markSetupDone(dir, "ab12"); err != nil {
		t.Fatal(err)
	}
	m.pass(context.Background())
	if !slices.Contains(b.archives, "ab12") || setupDone(dir, "ab12") {
		t.Fatal(b.archives)
	}
	if markSetupDone(dir, "../x") == nil {
		t.Fatal("a path that is not a session id was written")
	}
}

func TestTmuxToUseFallsBackFromAMissingPath(t *testing.T) {
	for in, want := range map[string]string{"/no/such/tmux": "tmux", "/bin/sh": "/bin/sh", "tmux": "tmux", "~/bin/tmux": "~/bin/tmux"} {
		if got := tmuxToUse(in); got != want {
			t.Errorf("tmuxToUse(%q) = %q, want %q", in, got, want)
		}
	}
}

// A gate-inbox that moved since the machine last said where it was is
// found again through the login shell, by its bare name.
func TestPollHostFindsAMovedGateInbox(t *testing.T) {
	far := healthyFar()
	far.hidden = true
	h, _ := resolveHost("box", hostSettings{Destination: "box"})
	h = h.withAbout(about{GateInbox: "/old/gate-inbox"})
	snap, err := pollHost(context.Background(), far.run, config{SSH: []string{"ssh"}, ControlPath: "none"}, h)
	if err != nil || snap.About == nil {
		t.Fatal(err, far.calls)
	}
	if len(far.calls) != 2 || strings.Contains(far.calls[1], "/old/gate-inbox") {
		t.Fatal(far.calls)
	}
}

func TestNWithNoMachinesOpensTheMachinesScreen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	e := newManaged(t, healthyFar())
	u := &screenUI{}
	e.ui = u
	keys := e.machineUI().Keys
	if err := keys[0].Run(context.Background(), extension.Press{}); err != nil {
		t.Fatal(err)
	}
	if screen, _ := u.last(); screen != machinesScreen {
		t.Fatal(screen)
	}
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	keys[0].Run(context.Background(), extension.Press{})
	if screen, view := u.last(); screen != spawnScreen || view.(*spawnForm).machine != "box-1" {
		t.Fatal(screen)
	}
}
