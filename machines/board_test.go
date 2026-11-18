package machines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

// fakeBoard is the board as the mirror loop sees it.
type fakeBoard struct {
	mu        sync.Mutex
	sessions  map[string]extension.SessionInfo
	launches  []extension.LaunchRequest
	pins      []string
	archives  []string
	replaces  []string
	next      int
	launchErr error
}

func newFakeBoard() *fakeBoard { return &fakeBoard{sessions: map[string]extension.SessionInfo{}} }

func (b *fakeBoard) List(_ context.Context, _ extension.SessionFilter) (extension.SessionList, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var list extension.SessionList
	for _, id := range sortedKeys(b.sessions) {
		list.Sessions = append(list.Sessions, b.sessions[id])
	}
	return list, nil
}

func (b *fakeBoard) Launch(_ context.Context, req extension.LaunchRequest) (extension.SessionInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.launchErr != nil {
		return extension.SessionInfo{}, b.launchErr
	}
	b.next++
	info := extension.SessionInfo{
		ID: fmt.Sprintf("l%d", b.next), Name: req.Name, Tool: req.Tool, Group: req.Group,
		Role: extensionID + "/" + req.Role, Running: true,
	}
	b.sessions[info.ID] = info
	b.launches = append(b.launches, req)
	return info, nil
}

func (b *fakeBoard) PinStatus(_ context.Context, id, status string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pins = append(b.pins, id+"="+status)
	return nil
}

// Replace starts a new row in the old one's place, as the board does: the
// new row keeps the old one's name and group, and the old one is left dead
// on the list until something archives it.
func (b *fakeBoard) Replace(_ context.Context, id string, req extension.LaunchRequest, _ ...extension.ReplaceOptions) (extension.SessionInfo, extension.ReplaceHandle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.sessions[id]
	dead := old
	dead.Running = false
	b.sessions[id] = dead
	b.next++
	info := extension.SessionInfo{
		ID: fmt.Sprintf("l%d", b.next), Name: old.Name, Tool: req.Tool, Group: old.Group,
		Role: extensionID + "/" + req.Role, Running: true,
	}
	b.sessions[info.ID] = info
	b.replaces = append(b.replaces, id+" "+strings.Join(req.Args, " ")+" group="+req.Group+" name="+req.Name)
	return info, nil, nil
}

func (b *fakeBoard) Archive(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, id)
	b.archives = append(b.archives, id)
	return nil
}

// fakeUI records the badges and facts the loop puts on rows.
type fakeUI struct {
	extension.UIHost
	mu       sync.Mutex
	badges   map[string]string
	facts    map[string]string
	owned    map[string]bool
	ownCalls int
}

func (u *fakeUI) Decorate(id string, badges ...extension.Badge) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.badges[id] = ""
	if len(badges) > 0 {
		u.badges[id] = badges[0].Text
	}
}

func (u *fakeUI) Own(id string, owned bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.owned == nil {
		u.owned = map[string]bool{}
	}
	u.owned[id] = owned
	u.ownCalls++
}

func (u *fakeUI) Fact(id string, fact extension.Fact) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.facts[id] = fact.Text
}

// hosts answers polls from a table, as a machine's `machines sessions --json` would.
type hosts map[string]any

func (h hosts) run(_ context.Context, argv []string) ([]byte, error) {
	destination := argv[len(argv)-2]
	switch v := h[destination].(type) {
	case error:
		return nil, v
	case snapshot:
		return json.Marshal(v)
	}
	return nil, fmt.Errorf("no host %s", destination)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPassMirrorsAHostsSessionsAndFollowsThem(t *testing.T) {
	dir := t.TempDir()
	grouped := mini
	grouped.Group = "mini group"
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none", Hosts: []host{grouped}}
	board, ui := newFakeBoard(), &fakeUI{badges: map[string]string{}, facts: map[string]string{}}
	reply := hosts{"mini": snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{live("a1", "waiting")}}}
	m := newMirrors(cfg, board, ui, quiet(), reply.run, dir)

	m.pass(context.Background())
	if len(board.launches) != 1 {
		t.Fatalf("launches = %+v, want one", board.launches)
	}
	req := board.launches[0]
	if req.Tool != toolName || req.Role != roleName || req.Group != "mini group" || strings.Join(req.Args, " ") != "mini a1" || req.Name != "mini/agent a1" {
		t.Errorf("launch = %+v", req)
	}
	if strings.Join(board.pins, ",") != "l1=waiting" {
		t.Errorf("pins = %v", board.pins)
	}
	if ui.badges["l1"] != "" || ui.facts["l1"] != "mini · claude · waiting · gi_a1" {
		t.Errorf("badge %q fact %q", ui.badges["l1"], ui.facts["l1"])
	}

	// The state survives the board: a new loop on the same directory keeps
	// the row it launched rather than launching another, and reconnects it
	// once in its place, so the pane runs with the settings this run read.
	var saved state
	data, err := os.ReadFile(filepath.Join(dir, "mirrors.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil || saved.Mirrors["mini/a1"] != "l1" {
		t.Fatalf("saved %s (%v)", data, err)
	}
	reply["mini"] = snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{live("a1", "working")}}
	m = newMirrors(cfg, board, ui, quiet(), reply.run, dir)
	m.pass(context.Background())
	if len(board.launches) != 1 {
		t.Errorf("a restarted loop launched again: %+v", board.launches)
	}
	if strings.Join(board.replaces, ";") != "l1 mini a1 group= name=" || board.pins[len(board.pins)-1] != "l2=working" {
		t.Errorf("replaces = %v, pins = %v; want l1 reconnected as l2 and pinned working", board.replaces, board.pins)
	}
	m.pass(context.Background())
	if len(board.replaces) != 1 {
		t.Errorf("reconnected more than once in one run: %v", board.replaces)
	}

	// The host drops off: the row stays, marked.
	reply["mini"] = errors.New("ssh: connect to host mini port 22: Network is unreachable")
	archived := len(board.archives)
	m.pass(context.Background())
	if len(board.archives) != archived {
		t.Errorf("archived %v while the host was unreachable", board.archives[archived:])
	}
	if ui.badges["l2"] != "✕ mini offline" {
		t.Errorf("badge = %q", ui.badges["l2"])
	}

	// It comes back without the session: the mirror goes. (l1 went when l2
	// replaced it.)
	reply["mini"] = snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{}}
	m.pass(context.Background())
	if strings.Join(board.archives, ",") != "l1,l2" {
		t.Errorf("archives = %v", board.archives)
	}
}

// Killing a mirror is how the operator reconnects it: the next pass
// replaces it in its own place and follows the new row.
func TestPassReconnectsAKilledMirrorInPlace(t *testing.T) {
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none", Hosts: []host{mini}}
	board := newFakeBoard()
	reply := hosts{"mini": snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{live("a1", "waiting")}}}
	m := newMirrors(cfg, board, nil, quiet(), reply.run, t.TempDir())
	m.pass(context.Background())
	board.mu.Lock()
	killed := board.sessions["l1"]
	killed.Running = false
	board.sessions["l1"] = killed
	board.mu.Unlock()
	m.pass(context.Background())
	if strings.Join(board.replaces, ";") != "l1 mini a1 group= name=" {
		t.Fatalf("replaces = %q, want l1 replaced with the same attach and no group or name", board.replaces)
	}
	if m.state.Mirrors["mini/a1"] != "l2" || board.pins[len(board.pins)-1] != "l2=waiting" {
		t.Errorf("state = %v, pins = %v; want the new row l2 followed and pinned", m.state.Mirrors, board.pins)
	}
	if strings.Join(board.archives, ",") != "l1" {
		t.Errorf("archives = %v, want the replaced row l1 archived at once", board.archives)
	}
	m.pass(context.Background())
	if len(board.replaces) != 1 || len(board.launches) != 1 {
		t.Errorf("a running replacement was replaced or launched again: %v %v", board.replaces, board.launches)
	}
}

func TestPassBacksOffALaunchTheBoardRefuses(t *testing.T) {
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none", Hosts: []host{mini}}
	board := newFakeBoard()
	board.launchErr = errors.New("no tool machines")
	reply := hosts{"mini": snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{live("a1", "idle")}}}
	m := newMirrors(cfg, board, nil, quiet(), reply.run, t.TempDir())
	m.pass(context.Background())
	board.launchErr = nil
	m.pass(context.Background())
	if len(board.launches) != 0 {
		t.Errorf("relaunched inside the backoff: %+v", board.launches)
	}
	m.now = func() time.Time { return time.Now().Add(launchBackoff + time.Second) }
	m.pass(context.Background())
	if len(board.launches) != 1 {
		t.Errorf("did not launch once the backoff passed: %+v", board.launches)
	}
}
