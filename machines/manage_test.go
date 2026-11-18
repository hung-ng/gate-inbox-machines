package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
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
	// targets is what `ssh -G` resolves each destination to.
	targets map[string]string
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
	if slices.Contains(argv, "-G") {
		if target, ok := f.targets[argv[len(argv)-1]]; ok {
			return []byte(target), nil
		}
		return nil, errors.New("no ssh config")
	}
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

// machineStore writes the registry fixture before the board reads it.
func machineStore(t *testing.T, dir string, machines ...host) string {
	t.Helper()
	store := storeFile{Machines: map[string]hostSettings{}}
	for _, h := range machines {
		store.Machines[h.Name] = hostSettings{Destination: h.Destination, GateInbox: h.Settings.GateInbox, Home: h.Home, Group: h.Group}
	}
	if err := writeJSON(filepath.Join(dir, storeFileName), store); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newManaged(t *testing.T, far *farMachine, machines ...host) *Extension {
	t.Helper()
	dir := machineStore(t, t.TempDir(), machines...)
	e := New()
	if err := e.Configure(extension.NewConfig(map[string]any{"control_path": "none"}).WithDataDir(dir)); err != nil {
		t.Fatal(err)
	}
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

func TestMachineReportsTmuxAndOperatorSetsGateInbox(t *testing.T) {
	socket := "agents"
	said := about{GateInbox: "/far/gate-inbox", Tmux: "/far/tmux", TmuxSocket: &socket}
	h, _ := resolveHost("box", hostSettings{GateInbox: "/mine/gate-inbox"})
	h = h.withAbout(said)
	if h.Tmux != "/far/tmux" || h.GateInbox != "/mine/gate-inbox" || h.TmuxSocket != "agents" {
		t.Fatalf("%+v", h)
	}
	empty := ""
	if h = h.withAbout(about{TmuxSocket: &empty}); h.TmuxSocket != "" {
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
		{"poll", "poll.example", "poll"},
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
	h, err := e.addMachine(context.Background(), addOptions{Destination: "me@box.example"}, &out)
	if err != nil {
		t.Fatal(err, out.String())
	}
	if h.Name != "box-1" || h.GateInbox != "/home/me/go/bin/gate-inbox" || h.Tmux != "/usr/bin/tmux" {
		t.Fatalf("%+v\n%s", h, out.String())
	}
	if !strings.Contains(out.String(), "it reports hostname Box-1.local") {
		t.Fatal(out.String())
	}
	// Another process of this build reads the same files.
	other := newRegistry(e.reg.dir)
	if got, ok := other.host("box-1"); !ok || got.Tmux != "/usr/bin/tmux" {
		t.Fatalf("%+v", got)
	}
	for _, opts := range []addOptions{
		{Destination: "me@box.example"},
		{Destination: "other", Name: "mini"},
		{Destination: "-oProxyCommand=x"},
		{Destination: "two words"},
	} {
		if _, err := e.addMachine(context.Background(), opts, &out); err == nil {
			t.Errorf("%+v was added", opts)
		}
	}
}

func TestAddPrintsTheCommandForAnSSHFailure(t *testing.T) {
	for _, tc := range []struct{ failure, command string }{
		{"Permission denied (publickey).", "ssh-copy-id me@box"},
		{"Host key verification failed.", "ssh me@box true"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			far := healthyFar()
			far.sshErr = errors.New(tc.failure)
			e := newManaged(t, far)
			var out bytes.Buffer
			_, err := e.addMachine(context.Background(), addOptions{Destination: "me@box"}, &out)
			if err == nil || !strings.Contains(out.String(), "`"+tc.command+"`") {
				t.Fatal(err, out.String())
			}
			if len(e.machines().all()) != 0 || len(far.calls) != 1 {
				t.Fatal(e.machines().all(), far.calls)
			}
		})
	}
}

func TestAddRefusesABrokenMachineUnlessForced(t *testing.T) {
	far := healthyFar()
	far.hidden = true
	e := newManaged(t, far)
	var out bytes.Buffer
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &out); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatal(err, out.String())
	}
	h, err := e.addMachine(context.Background(), addOptions{Destination: "box", Force: true}, &out)
	if err != nil || h.Name != "box" {
		t.Fatal(err)
	}
}

func TestAddFindsGateInboxThroughTheLoginShell(t *testing.T) {
	far := healthyFar()
	far.hidden, far.loginPath = true, "/home/me/go/bin/gate-inbox"
	e := newManaged(t, far)
	var out bytes.Buffer
	h, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &out)
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
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &out); err != nil {
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

func TestRemovePauseAndResume(t *testing.T) {
	far := healthyFar()
	e := newManaged(t, far, mini)
	var out bytes.Buffer
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box", Name: "box"}, &out); err != nil {
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
	if err := e.removeMachine("box"); err != nil {
		t.Fatal(err)
	}
	if err := e.removeMachine("box"); err == nil {
		t.Fatal("removed an unknown machine")
	}
	if all := e.machines().all(); len(all) != 1 || all[0].Name != "mini" || !all[0].Paused {
		t.Fatal(all)
	}
	if err := e.pauseMachine("mini", false); err != nil {
		t.Fatal(err)
	}
	if active := e.machines().active(); len(active) != 1 || active[0].Name != "mini" {
		t.Fatal(active)
	}
}

func TestRegistryFollowsOtherProcesses(t *testing.T) {
	dir := t.TempDir()
	board := newRegistry(dir)
	shell := newRegistry(dir)
	if err := shell.change(func(s *storeFile) error {
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
	shell.change(func(s *storeFile) error {
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

func TestRegistryReportsAnInvalidMachine(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, storeFileName), []byte(`{"machines":{"mini":{"destination":"from-store"},"Bad Name":{"destination":"x"}}}`), 0o600)
	r := newRegistry(dir)
	if all := r.all(); len(all) != 1 || all[0].Destination != "from-store" {
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
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none"}
	m := newMirrors(cfg, b, nil, quiet(), run, machineStore(t, t.TempDir(), mini))
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

func TestPassPutsAwayAPausedMachinesMirrors(t *testing.T) {
	dir := t.TempDir()
	b := newFakeBoard()
	replies := hosts{"mini": snapshot{Version: wireVersion, BoardRunning: boolPtr(true), Sessions: []machineSession{live("a1", "waiting")}}}
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none"}
	m := newMirrors(cfg, b, nil, quiet(), replies.run, machineStore(t, dir, mini))
	m.pass(context.Background())
	if len(b.launches) != 1 || len(b.archives) != 0 {
		t.Fatal(b.launches, b.archives)
	}
	m.reg.change(func(s *storeFile) error {
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
	if !strings.Contains(body, "› mini  online · 2 sessions · 1 not mirrored  mini") {
		t.Fatal(body)
	}
	if strings.Count(body, "mini") != 2 {
		t.Fatal("a machine on the board is offered again:\n" + body)
	}
	if !strings.Contains(body, "devbox  from ~/.ssh/config · enter to check and add") {
		t.Fatal(body)
	}
	// Space pauses mini, off the event loop; the screen reads the store
	// again.
	v.Key(extension.ViewKey{Action: actionPause, Key: "space"})
	waitFor(t, "the pause", func() bool { return strings.Contains(text(v.Render(120, 40)), "mini  paused") })
	if h, _ := e.machines().host("mini"); !h.Paused {
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

func TestMachinesScreenStartsOnRenderAndCancelsLoadOnClose(t *testing.T) {
	e := newManaged(t, healthyFar(), mini)
	u := &screenUI{}
	e.ui = u
	started := make(chan struct{})
	cancelled := make(chan struct{})
	e.run = func(ctx context.Context, argv []string) ([]byte, error) {
		if slices.Contains(argv, "-G") && argv[len(argv)-1] == "mini" {
			close(started)
			<-ctx.Done()
			close(cancelled)
		}
		return nil, ctx.Err()
	}
	t.Setenv("HOME", t.TempDir())

	e.openMachines(t.Context())
	_, view := u.last()
	v := view.(*machinesView)
	listeners := func() int {
		e.status.mu.Lock()
		defer e.status.mu.Unlock()
		return len(e.status.listeners)
	}
	if listeners() != 0 {
		t.Fatal("registered a listener before the board rendered the view")
	}
	v.Render(120, 40)
	v.Render(120, 40)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the screen did not start loading")
	}
	if listeners() != 1 {
		t.Fatal("expected one listener after render")
	}

	v.Closed(extension.CloseDismissed)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the screen did not cancel its SSH config lookup")
	}
	if listeners() != 0 {
		t.Fatal("closing the screen left a listener registered")
	}
}

func TestMachinesScreenRemovesOnTheSecondPress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	far := healthyFar()
	e := newManaged(t, far)
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box", Name: "box"}, &bytes.Buffer{}); err != nil {
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
	waitFor(t, "the removal", func() bool { return len(e.machines().all()) == 0 })
	if !strings.Contains(text(v.Render(120, 40)), "No machines yet") {
		t.Fatal(text(v.Render(120, 40)))
	}
}

func TestAddFormShowsTheSSHCommandAndCanRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	far := healthyFar()
	far.sshErr = errors.New("Permission denied (publickey).")
	e := newManaged(t, far)
	u := &screenUI{}
	e.ui = u
	e.openAdd(context.Background(), "")
	_, view := u.last()
	f := view.(*addForm)
	values := map[string]string{"destination": "me@box", "name": "box", "group": "work", "home": "~/board", "gate_inbox": "/opt/bin/gate-inbox"}
	f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: values})
	waitFor(t, "the SSH command", func() bool {
		return strings.Contains(text(f.Render(120, 40)), "`ssh-copy-id me@box`")
	})
	if screen, _ := u.last(); screen != addScreen {
		t.Fatal(screen)
	}

	far.setErr(nil)
	f.Key(extension.ViewKey{Action: extension.ActionSubmit, Values: values})
	waitFor(t, "the machines screen", func() bool { s, _ := u.last(); return s == machinesScreen })
	h, ok := e.machines().host("box")
	if !ok || h.Group != "work" || h.Home != "~/board" || h.GateInbox != "/opt/bin/gate-inbox" {
		t.Fatal(h)
	}
	if !slices.Equal(u.notes, []string{"Added box (me@box)"}) {
		t.Fatal(u.notes)
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
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	keys[0].Run(context.Background(), extension.Press{})
	if screen, view := u.last(); screen != spawnScreen || view.(*spawnForm).machine != "box-1" {
		t.Fatal(screen)
	}
}

func resolved(user, hostname, port string) string {
	return "user " + user + "\nhostname " + hostname + "\nport " + port + "\nbatchmode no\n"
}

func TestSSHTargetReadsWhatSSHWouldConnectTo(t *testing.T) {
	far := healthyFar()
	far.targets = map[string]string{"alias": resolved("me", "Mini.Example.COM.", "22")}
	e := newManaged(t, far)
	if target, ok := e.sshTarget(context.Background(), "alias"); !ok || target != "me@mini.example.com:22" {
		t.Fatal(target, ok)
	}
	if _, ok := e.sshTarget(context.Background(), "unknown"); ok {
		t.Fatal("a destination ssh -G refused has a target")
	}
	if _, ok := e.sshTarget(context.Background(), "-oProxyCommand=x"); ok {
		t.Fatal("an option was passed as a destination")
	}
}

// Suggestions match existing machines by their SSH targets without polling.
func TestSuggestionsLeaveOutMachinesAlreadyOnTheBoard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Host mini-alias devbox\n"), 0o600)
	far := healthyFar()
	far.snap.About.Hostname = "hung-mini"
	far.targets = map[string]string{
		"mini.example.com": resolved("me", "mini.example.com", "22"),
		"mini-alias":       resolved("me", "mini.example.com", "22"),
		"devbox":           resolved("me", "devbox.example.com", "22"),
		"10.0.0.7":         resolved("me", "10.0.0.7", "22"),
	}
	mini := host{Name: "mini", Destination: "mini.example.com", GateInbox: "gate-inbox", Tmux: "tmux"}
	e := newManaged(t, far, mini)
	e.reg.learn("mini", "mini.example.com", about{Hostname: "hung-mini"})
	e.cfg.Discover = []string{"sh", "-c", "printf '10.0.0.7\\n'"}
	found, problem := e.suggestions(context.Background())
	want := []suggestion{{Destination: "10.0.0.7", From: fromDiscover}, {Destination: "devbox", From: fromSSHConfig}}
	if problem != "" || !slices.Equal(found, want) || len(far.calls) != 0 {
		t.Fatal(found, problem, far.calls)
	}
}

func TestAddRefusesAnotherNameForAMachineOnTheBoard(t *testing.T) {
	far := healthyFar()
	far.targets = map[string]string{
		"mini.example.com": resolved("me", "mini.example.com", "22"),
		"mini-alias":       resolved("me", "mini.example.com", "22"),
	}
	mini := host{Name: "mini", Destination: "mini.example.com", GateInbox: "gate-inbox", Tmux: "tmux"}
	e := newManaged(t, far, mini)
	var out bytes.Buffer
	_, err := e.addMachine(context.Background(), addOptions{Destination: "mini-alias"}, &out)
	if err == nil || !strings.Contains(err.Error(), "reaches the same host as mini (me@mini.example.com:22)") {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("probed a machine it was going to refuse:", out.String())
	}
	// The same host with a board home of its own is a second board.
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "mini-alias", Home: "~/other-board"}, &out); err != nil {
		t.Fatal(err)
	}
}

// A machine on the board with a home of its own is still the one meant
// when an add for its host names no home.
func TestAddWithNoHomeMeansTheBoardAlreadyThere(t *testing.T) {
	far := healthyFar()
	far.targets = map[string]string{
		"mini.example.com": resolved("me", "mini.example.com", "22"),
		"mini-alias":       resolved("me", "mini.example.com", "22"),
	}
	mini := host{Name: "mini", Destination: "mini.example.com", GateInbox: "gate-inbox", Tmux: "tmux", Home: "~/review/board"}
	e := newManaged(t, far, mini)
	_, err := e.addMachine(context.Background(), addOptions{Destination: "mini-alias"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "with board home ~/review/board; for another board on that host, give its board home") {
		t.Fatal(err)
	}
}

// A LAN address resolves to another host, but the machine says who it is.
func TestAddRefusesAMachineThatReportsAHostnameOnTheBoard(t *testing.T) {
	far := healthyFar()
	e := newManaged(t, far)
	var out bytes.Buffer
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box.example.com"}, &out); err != nil {
		t.Fatal(err)
	}
	_, err := e.addMachine(context.Background(), addOptions{Destination: "10.0.0.7"}, &out)
	if err == nil || !strings.Contains(err.Error(), "same machine as box-1: both report hostname Box-1.local") {
		t.Fatal(err)
	}
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "10.0.0.7", Force: true}, &out); err != nil {
		t.Fatal("--force did not add it:", err)
	}
}

func TestAddTakesAGateInboxPath(t *testing.T) {
	far := healthyFar()
	far.hidden, far.loginPath = true, "/srv/review/bin/gate-inbox-machines"
	// A machine reports the path of the binary that answered.
	far.snap.About.GateInbox = far.loginPath
	e := newManaged(t, far)
	var out bytes.Buffer
	_, err := e.addMachine(context.Background(), addOptions{Destination: "box", Name: "x"}, &out)
	if err != nil {
		t.Fatal("the login shell should still find it:", err)
	}
	// Neither PATH nor the login shell knows a binary under another name.
	far.loginPath = ""
	out.Reset()
	_, err = e.addMachine(context.Background(), addOptions{Destination: "box2"}, &out)
	if err == nil || !strings.Contains(out.String(), "give this build's full path there as the gate-inbox path (--gate-inbox)") {
		t.Fatal(err, out.String())
	}
	far.loginPath = "/srv/review/bin/gate-inbox-machines"
	far.snap.About.Hostname = "box2.local"
	h, err := e.addMachine(context.Background(), addOptions{Destination: "box2", Name: "y", GateInbox: far.loginPath}, &out)
	if err != nil || h.Settings.GateInbox != far.loginPath {
		t.Fatal(err, h)
	}
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box3", GateInbox: "bin/gate-inbox"}, &out); err == nil {
		t.Fatal("a relative path was taken")
	}
}

func TestAddTheSameDestinationWithAnotherHomeIsASecondBoard(t *testing.T) {
	far := healthyFar()
	e := newManaged(t, far)
	var out bytes.Buffer
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box", Name: "box", Home: "~/one"}, &out); err != nil {
		t.Fatal(err)
	}
	_, err := e.addMachine(context.Background(), addOptions{Destination: "box"}, &out)
	if err == nil || !strings.Contains(err.Error(), "already on this board as box with board home ~/one") {
		t.Fatal(err)
	}
	far.snap.About.Hostname = "other-board.local"
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box", Name: "box2", Home: "~/two"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := e.addMachine(context.Background(), addOptions{Destination: "box", Home: "~/two"}, &out); err == nil {
		t.Fatal("the same board was added twice")
	}
}

func TestCheckSaysWhenEveryMachineIsPaused(t *testing.T) {
	e := newManaged(t, healthyFar(), mini)
	if err := e.pauseMachine("mini", true); err != nil {
		t.Fatal(err)
	}
	err := e.runCheck(context.Background(), nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "every machine is paused") {
		t.Fatal(err)
	}
	if err := e.runCheck(context.Background(), []string{"mini"}, &bytes.Buffer{}); err != nil {
		t.Fatal("a paused machine named outright is checked:", err)
	}
}

func TestMachineStatusShowsTheVersion(t *testing.T) {
	e := newManaged(t, healthyFar())
	e.status.set(map[string]machineState{"box": {BoardRunning: true}})
	h := host{Name: "box", About: &about{Version: Version}}
	if status, _ := e.machineStatus(h); !strings.Contains(status, "machines "+Version) {
		t.Fatal(status)
	}
	h.About.Version = "0.0.1"
	if status, tone := e.machineStatus(h); !strings.Contains(status, "machines 0.0.1, this one "+Version) || tone != extension.ToneWarn {
		t.Fatal(status, tone)
	}
}

// Drawing the machines screen reads the registry on the board's event
// loop, so it must not wait while another process holds the machines
// file's lock.
func TestRegistryReadsDontWaitOnTheFileLock(t *testing.T) {
	dir := t.TempDir()
	r := newRegistry(dir)
	lock, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	learned := make(chan struct{})
	go func() {
		r.learn("box", "box", about{Hostname: "box"})
		close(learned)
	}()
	time.Sleep(50 * time.Millisecond)
	read := make(chan struct{})
	go func() {
		r.all()
		r.issue()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(2 * time.Second):
		t.Fatal("reading the registry waited on the file lock")
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	<-learned
}

func TestAddFormDoesNotReopenTheScreenOverAViewThatReplacedIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	e := newManaged(t, healthyFar())
	u := &screenUI{}
	e.ui = u
	e.openAdd(context.Background(), "")
	_, view := u.last()
	view.(*addForm).Closed(extension.CloseReplaced)
	if screen, _ := u.last(); screen != addScreen {
		t.Fatal("reopened the machines screen over the view that replaced the form")
	}
	view.(*addForm).Closed(extension.CloseDismissed)
	if screen, _ := u.last(); screen != machinesScreen {
		t.Fatal(screen)
	}
}

// A machine's local network name is its hostname with ".local", which is
// enough to leave an alias for it out of the suggestions without
// connecting to it.
func TestSuggestionsLeaveOutALocalNameForAMachineOnTheBoard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte("Host mini-lan mini-local-dot box-lan devbox\n"), 0o600)
	far := healthyFar()
	far.targets = map[string]string{
		"mini.example.com": resolved("me", "mini.example.com", "22"),
		"mini-lan":         resolved("me", "hung-mini.local", "22"),
		"mini-local-dot":   resolved("me", "Hung-Mini.local.", "22"),
		"box-lan":          resolved("me", "box.local", "22"),
		"devbox":           resolved("me", "hung-mini.example.org", "22"),
	}
	mini := host{Name: "mini", Destination: "mini.example.com", GateInbox: "gate-inbox", Tmux: "tmux"}
	e := newManaged(t, far, mini)
	// macOS can report its hostname with ".local" on the end too.
	e.reg.learn("mini", "mini.example.com", about{Hostname: "Hung-Mini.local"})
	found, _ := e.suggestions(context.Background())
	var got []string
	for _, s := range found {
		got = append(got, s.Destination)
	}
	if strings.Join(got, ",") != "box-lan,devbox" {
		t.Fatal(got)
	}
}

func TestShortHostname(t *testing.T) {
	for in, want := range map[string]string{
		"hung-mini": "hung-mini", "Hung-Mini.local": "hung-mini", "hung-mini.local.": "hung-mini",
		"hung-mini.example.com": "hung-mini.example.com", "192.168.1.5": "192.168.1.5", "": "",
	} {
		if got := shortHostname(in); got != want {
			t.Errorf("shortHostname(%q) = %q, want %q", in, got, want)
		}
	}
	if got := targetHost("me@hung-mini.local:22"); got != "hung-mini.local" {
		t.Fatal(got)
	}
}
