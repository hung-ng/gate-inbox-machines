package machines

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

func TestUnavailablePinsAndOwnsWithoutReconcile(t *testing.T) {
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none"}
	b := newFakeBoard()
	u := &fakeUI{badges: map[string]string{}, facts: map[string]string{}}
	replies := hosts{"mini": snapshot{Version: wireVersion, BoardRunning: boolPtr(true), Sessions: []machineSession{live("a1", "waiting")}}}
	m := newMirrors(cfg, b, u, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	m.now = func() time.Time { return time.Date(2026, 1, 1, 12, 34, 0, 0, time.UTC) }
	m.pass(context.Background())
	for _, offline := range []bool{true, false} {
		if offline {
			replies["mini"] = context.DeadlineExceeded
		} else {
			// A stopped board can return stale sessions, or none at all.
			replies["mini"] = snapshot{Version: wireVersion, BoardRunning: boolPtr(false)}
		}
		m.pass(context.Background())
		if len(b.archives) != 0 || len(b.launches) != 1 || len(b.replaces) != 0 {
			t.Fatal("reconciled unavailable board")
		}
		if b.pins[len(b.pins)-1] != "l1=errored" || !u.owned["l1"] {
			t.Fatal(b.pins, u.owned)
		}
		if offline {
			if u.badges["l1"] != "✕ mini offline" || u.facts["l1"] != "mini · unreachable since 12:34 · gi_a1" {
				t.Fatal(u.badges, u.facts)
			}
		} else if u.badges["l1"] != "✕ mini board stopped" || !strings.Contains(u.facts["l1"], "start that machine's board") {
			t.Fatal(u.badges, u.facts)
		}
	}
	replies["mini"] = snapshot{Version: wireVersion, BoardRunning: boolPtr(true), Sessions: []machineSession{live("a1", "waiting")}}
	m.pass(context.Background())
	if u.owned["l1"] || u.badges["l1"] != "" || b.pins[len(b.pins)-1] != "l1=waiting" {
		t.Fatal(u.owned, u.badges, b.pins)
	}
}

func TestLegacyMirrorReplacedWithMirrorTool(t *testing.T) {
	b := newFakeBoard()
	b.sessions["old"] = extension.SessionInfo{ID: "old", Tool: "machines", Role: mirrorRole, Running: true}
	replies := hosts{"mini": snapshot{Version: wireVersion, BoardRunning: boolPtr(true), Sessions: []machineSession{live("a1", "waiting")}}}
	m := newMirrors(config{SSH: []string{"ssh"}, Poll: defaultPoll, ControlPath: "none"}, b, nil, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	m.state = withMirrors("mini/a1", "old")
	m.pass(context.Background())
	if b.sessions[m.state.Mirrors["mini/a1"]].Tool != "mirror" {
		t.Fatal(b.sessions)
	}
}

func TestWireAdditionsAndLegacyExclusion(t *testing.T) {
	s, err := listSessions(context.Background(), &fakeSessions{pages: [][]extension.SessionInfo{{
		{ID: "a1", Tool: "machines"}, {ID: "a2", Tool: "mirror"}, {ID: "a3", Role: mirrorRole}, {ID: "a4", Tool: "agent"},
	}}}, nil)
	if err != nil || len(s.Sessions) != 1 || s.Sessions[0].ID != "a4" {
		t.Fatal(s, err)
	}
	s.Tools = []string{"agent"}
	s.BoardRunning = boolPtr(true)
	data, _ := json.Marshal(s)
	back, err := parseSnapshot(strings.NewReader(string(data)))
	if err != nil || !back.boardRunning() || len(back.Tools) != 1 || back.Version != 1 {
		t.Fatal(back, err)
	}
	old, err := parseSnapshot(strings.NewReader(`{"version":1,"sessions":[]}`))
	if err != nil || !old.boardRunning() {
		t.Fatal(old, err)
	}
	ids := parseTmuxSessions("gi_a1 0\ngi_poll-anchor 1\n")
	if !ids["a1"] || ids["poll-anchor"] || parseTmuxSessions("gi_poll-anchor 0")["poll-anchor"] {
		t.Fatal(ids)
	}
}

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

// A stopped board's tmux still says what can be attached to, so a session
// there gets a row with no status, and nothing is archived on the word of
// its saved sessions alone.
func TestPlanStoppedBoardMirrorsLiveSessionsWithoutStatus(t *testing.T) {
	prev := withMirrors("mini/a1", "l1")
	obs := seen(mini, live("b2", "waiting"))
	obs.Snapshot.BoardRunning = boolPtr(false)
	actions, next := plan(prev, []host{mini}, []observation{obs}, rows(mirrorRow("l1", true)), nil, nil)
	if got := describe(actions); strings.Join(got, ",") != "launch mini/b2 as errored,pin l1 errored" {
		t.Fatal(got)
	}
	if next.Mirrors["mini/a1"] != "l1" {
		t.Fatal("a stopped board must not end a mirror", next)
	}
	actions, _ = plan(prev, []host{mini}, []observation{obs}, rows(mirrorRow("l1", false)), nil, nil)
	if got := describe(actions); strings.Join(got, ",") != "launch mini/b2 as errored" {
		t.Fatal("must not reconnect a mirror whose session it can't see", got)
	}
	obs = seen(mini, live("a1", "waiting"))
	obs.Snapshot.BoardRunning = boolPtr(false)
	actions, _ = plan(prev, []host{mini}, []observation{obs}, rows(mirrorRow("l1", false)), nil, nil)
	if got := describe(actions); strings.Join(got, ",") != "relaunch l1 for mini/a1 as errored" {
		t.Fatal(got)
	}
	actions, _ = plan(prev, []host{mini}, []observation{obs}, rows(mirrorRow("l1", true)), map[string]string{"l1": "waiting"}, map[string]bool{"l1": true})
	if got := describe(actions); strings.Join(got, ",") != "pin l1 errored" {
		t.Fatal(got)
	}
}

func TestBoardSingletonPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.lock")
	if boardRunning(dir) {
		t.Fatal("missing file")
	}
	for _, text := range []string{"", "garbage", "0", "-1", "4294967297"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if boardRunning(dir) {
			t.Fatalf("running for %q", text)
		}
	}
	os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\nextra line"), 0600)
	if !boardRunning(dir) {
		t.Fatal("own PID stopped")
	}
	child := exec.Command("/bin/sh", "-c", "exit 0")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(strconv.Itoa(pid)), 0600)
	if boardRunning(dir) {
		t.Fatal("reaped child running")
	}
}

func TestCachedToolsSurviveFailureAndAreCopied(t *testing.T) {
	e := New()
	e.cfg = config{Poll: defaultPoll, SSH: []string{"ssh"}}
	replies := hosts{"mini": snapshot{Version: wireVersion, Tools: []string{"agent"}}}
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

func TestScrollingAndOwnershipTransitions(t *testing.T) {
	u := &fakeUI{badges: map[string]string{}, facts: map[string]string{}}
	m := newMirrors(config{}, newFakeBoard(), u, quiet(), nil, t.TempDir())
	m.state = withMirrors("mini/a1", "l1")
	session := live("a1", "working")
	session.Scrolling = true
	obs := seen(mini, session)
	local := rows(mirrorRow("l1", false))
	for i := 0; i < 3; i++ {
		m.decorate([]observation{obs}, local)
	}
	if u.ownCalls != 0 || u.badges["l1"] != "↕ scroll mode" || u.facts["l1"] != "mini · claude · working · scroll mode: press q inside the mirror to type again · gi_a1" {
		t.Fatal(u.ownCalls, u.badges, u.facts)
	}
	obs.Snapshot.BoardRunning = boolPtr(false)
	m.decorate([]observation{obs}, local)
	m.decorate([]observation{obs}, local)
	if u.ownCalls != 1 || u.badges["l1"] != "✕ mini board stopped" {
		t.Fatal(u.ownCalls, u.badges)
	}
	obs.Err = context.DeadlineExceeded
	m.decorate([]observation{obs}, local)
	if u.ownCalls != 1 || u.badges["l1"] != "✕ mini offline" {
		t.Fatal(u.ownCalls, u.badges)
	}
	obs.Err = nil
	obs.Snapshot.BoardRunning = boolPtr(true)
	obs.Snapshot.Sessions[0].Scrolling = false
	m.decorate([]observation{obs}, local)
	if u.ownCalls != 2 || u.badges["l1"] != "⇄ mini detached" {
		t.Fatal(u.ownCalls, u.badges)
	}
	modes := parseTmuxScrolling("gi_a1 0\ngi_a1 1\ngi_b2 0\ngi_poll-anchor 1\n")
	if !modes["a1"] || modes["b2"] || modes["poll-anchor"] {
		t.Fatal(modes)
	}
}

func TestOwnershipTrackingClearedOnArchiveAndReplace(t *testing.T) {
	b := newFakeBoard()
	u := &fakeUI{badges: map[string]string{}, facts: map[string]string{}}
	replies := hosts{"mini": snapshot{Version: wireVersion, Sessions: []machineSession{live("a1", "working")}}}
	m := newMirrors(config{Poll: defaultPoll, SSH: []string{"ssh"}}, b, u, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	m.pass(context.Background())
	first := m.state.Mirrors["mini/a1"]
	if _, ok := m.owned[first]; !ok {
		t.Fatal("missing initial tracking")
	}
	row := b.sessions[first]
	row.Running = false
	b.sessions[first] = row
	m.pass(context.Background())
	if _, ok := m.owned[first]; ok {
		t.Fatal("replacement retained old ownership")
	}
	second := m.state.Mirrors["mini/a1"]
	if first == second {
		t.Fatal("did not replace")
	}
	replies["mini"] = snapshot{Version: wireVersion}
	m.pass(context.Background())
	if _, ok := m.owned[second]; ok {
		t.Fatal("archive retained ownership")
	}
}
