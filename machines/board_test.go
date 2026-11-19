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
	mu           sync.Mutex
	sessions     map[string]extension.SessionInfo
	launches     []extension.LaunchRequest
	pins         []string
	archives     []string
	next         int
	launchErr    error
	archiveErr   error
	getErr       error
	listPageSize int
	listErrAfter string
	listCursor   string
	listCalls    []extension.SessionFilter
	omitted      string
}

func newFakeBoard() *fakeBoard { return &fakeBoard{sessions: map[string]extension.SessionInfo{}} }

func (b *fakeBoard) List(_ context.Context, filter extension.SessionFilter) (extension.SessionList, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listCalls = append(b.listCalls, filter)
	if b.listErrAfter != "" && filter.After == b.listErrAfter {
		return extension.SessionList{}, errors.New("list failed mid-page")
	}
	var list extension.SessionList
	for _, id := range sortedKeys(b.sessions) {
		row := b.sessions[id]
		if id <= filter.After || id == b.omitted || row.Archived {
			continue
		}
		if b.listPageSize > 0 && len(list.Sessions) == b.listPageSize {
			list.Truncated = true
			list.Cursor = list.Sessions[len(list.Sessions)-1].ID
			break
		}
		list.Sessions = append(list.Sessions, row)
	}
	if b.listCursor != "" {
		list.Truncated = true
		list.Cursor = b.listCursor
	}
	return list, nil
}

func (b *fakeBoard) Get(_ context.Context, id string) (extension.SessionInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.getErr != nil {
		return extension.SessionInfo{}, b.getErr
	}
	row, ok := b.sessions[id]
	if !ok {
		return extension.SessionInfo{}, fmt.Errorf("session %s does not exist; call gate-inbox sessions for current ids", id)
	}
	return row, nil
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
		Role: extensionID + "/" + req.Role, Running: true, Status: "idle",
	}
	b.sessions[info.ID] = info
	b.launches = append(b.launches, req)
	return info, nil
}

func (b *fakeBoard) PinStatus(_ context.Context, id, status string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	row := b.sessions[id]
	row.Status = status
	b.sessions[id] = row
	b.pins = append(b.pins, id+"="+status)
	return nil
}

func (b *fakeBoard) Archive(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.archiveErr != nil {
		return b.archiveErr
	}
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
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none"}
	board, ui := newFakeBoard(), &fakeUI{badges: map[string]string{}, facts: map[string]string{}}
	reply := hosts{"mini": snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{live("a1", "waiting")}}}
	m := newMirrors(cfg, board, ui, quiet(), reply.run, machineStore(t, dir, grouped))

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
	if ui.badges["l1"] != "" || ui.facts["l1"] != "mini · claude · waiting · gi_a1 · Press Enter to read and answer on the remote machine" {
		t.Errorf("badge %q fact %q", ui.badges["l1"], ui.facts["l1"])
	}

	// A fresh reconcile loop follows the existing row and pane.
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
	if got := board.sessions["l1"]; got.Status != "working" || !got.Running || len(board.archives) != 0 || m.state.Mirrors["mini/a1"] != "l1" {
		t.Fatalf("restart changed the mirror: row %+v, archives %v, state %v", got, board.archives, m.state)
	}
	if got := ui.facts["l1"]; got != "mini · claude · working · gi_a1" {
		t.Errorf("working mirror detail = %q", got)
	}
	m.pass(context.Background())
	if got := strings.Join(board.pins, ","); got != "l1=waiting,l1=working" {
		t.Errorf("pins = %s", got)
	}

	// The host drops off: the row stays, marked.
	reply["mini"] = errors.New("ssh: connect to host mini port 22: Network is unreachable")
	archived := len(board.archives)
	m.pass(context.Background())
	if len(board.archives) != archived {
		t.Errorf("archived %v while the host was unreachable", board.archives[archived:])
	}
	if ui.badges["l1"] != "✕ mini offline" {
		t.Errorf("badge = %q", ui.badges["l1"])
	}

	// It comes back without the session: the mirror goes.
	reply["mini"] = snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{}}
	m.pass(context.Background())
	if strings.Join(board.archives, ",") != "l1" {
		t.Errorf("archives = %v", board.archives)
	}
}

func TestPassPinsTheSameMirrorAfterStockRevive(t *testing.T) {
	for _, observeDead := range []bool{true, false} {
		t.Run(fmt.Sprintf("observed dead=%v", observeDead), func(t *testing.T) {
			cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none"}
			board := newFakeBoard()
			reply := hosts{"mini": snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{live("a1", "waiting")}}}
			m := newMirrors(cfg, board, nil, quiet(), reply.run, machineStore(t, t.TempDir(), mini))
			ctx := context.Background()
			m.pass(ctx)
			first := board.sessions["l1"]
			if observeDead {
				dead := first
				dead.Running, dead.Status = false, "dead"
				board.sessions["l1"] = dead
				m.pass(ctx)
				m.pass(ctx)
				if board.sessions["l1"] != dead || len(board.pins) != 1 {
					t.Fatalf("reconcile changed a killed mirror: %v, pins %v", board.sessions, board.pins)
				}
			}
			revived := first
			revived.Status = "starting"
			board.sessions["l1"] = revived
			m.pass(ctx)
			m.pass(ctx)
			if len(board.launches) != 1 || len(board.archives) != 0 || m.state.Mirrors["mini/a1"] != "l1" {
				t.Fatalf("reconcile changed the row: launches %v, archives %v, state %v", board.launches, board.archives, m.state)
			}
			if got := strings.Join(board.pins, ","); got != "l1=waiting,l1=waiting" {
				t.Errorf("pins = %s", got)
			}
		})
	}
}

func TestPassBacksOffALaunchTheBoardRefuses(t *testing.T) {
	cfg := config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none"}
	board := newFakeBoard()
	board.launchErr = errors.New("no tool machines")
	reply := hosts{"mini": snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: []machineSession{live("a1", "idle")}}}
	m := newMirrors(cfg, board, nil, quiet(), reply.run, machineStore(t, t.TempDir(), mini))
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

func TestUnavailableKeepsTheLastPinAndOwnsTheRow(t *testing.T) {
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
			replies["mini"] = snapshot{Version: wireVersion, BoardRunning: boolPtr(false), Sessions: []machineSession{}}
		}
		m.pass(context.Background())
		if len(b.archives) != 0 || len(b.launches) != 1 {
			t.Fatal("reconciled unavailable board")
		}
		if strings.Join(b.pins, ",") != "l1=waiting" || !u.owned["l1"] {
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
	if u.owned["l1"] || u.badges["l1"] != "" || strings.Join(b.pins, ",") != "l1=waiting" {
		t.Fatal(u.owned, u.badges, b.pins)
	}
}

func TestLegacyRunningMirrorKeepsItsPane(t *testing.T) {
	b := newFakeBoard()
	b.sessions["old"] = extension.SessionInfo{ID: "old", Tool: "machines", Role: mirrorRole, Running: true}
	replies := hosts{"mini": snapshot{Version: wireVersion, BoardRunning: boolPtr(true), Sessions: []machineSession{live("a1", "waiting")}}}
	m := newMirrors(config{SSH: []string{"ssh"}, Poll: defaultPoll, ControlPath: "none"}, b, nil, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	if err := writeJSON(m.path, withMirrors("mini/a1", "old")); err != nil {
		t.Fatal(err)
	}
	m.pass(context.Background())
	if len(b.launches) != 0 || len(b.archives) != 0 || m.state.Mirrors["mini/a1"] != "old" {
		t.Fatal(b.sessions)
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

func TestOwnershipTrackingClearedOnArchive(t *testing.T) {
	b := newFakeBoard()
	u := &fakeUI{badges: map[string]string{}, facts: map[string]string{}}
	replies := hosts{"mini": snapshot{Version: wireVersion, Sessions: []machineSession{live("a1", "working")}}}
	m := newMirrors(config{Poll: defaultPoll, SSH: []string{"ssh"}}, b, u, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	m.pass(context.Background())
	first := m.state.Mirrors["mini/a1"]
	if _, ok := m.owned[first]; !ok {
		t.Fatal("missing initial tracking")
	}

	replies["mini"] = snapshot{Version: wireVersion, Sessions: []machineSession{}}
	m.pass(context.Background())
	if _, ok := m.owned[first]; ok {
		t.Fatal("archive retained ownership")
	}
}

func TestPassVerifiesRowsMissingFromList(t *testing.T) {
	for _, outcome := range []string{"active", "archived", "deleted", "lookup failed"} {
		t.Run(outcome, func(t *testing.T) {
			b := newFakeBoard()
			replies := hosts{"mini": snapshot{Version: wireVersion, Sessions: []machineSession{live("a1", "working")}}}
			m := newMirrors(testConfig(), b, nil, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
			m.pass(context.Background())
			b.omitted = "l1"
			switch outcome {
			case "archived":
				row := b.sessions["l1"]
				row.Archived = true
				b.sessions[row.ID] = row
			case "deleted":
				delete(b.sessions, "l1")
			case "lookup failed":
				b.getErr = errors.New("database busy")
				b.sessions["orphan"] = mirrorRow("orphan", true)
			}
			m.pass(context.Background())
			want := withMirrors("mini/a1", "l1")
			if outcome == "archived" || outcome == "deleted" {
				want = newState()
				want.Dismissed["mini/a1"] = "l1"
			}
			if !sameState(m.state, want) || len(b.launches) != 1 || len(b.archives) != 0 {
				t.Fatal(m.state, b.launches, b.archives)
			}
		})
	}
}

func TestPassWaitsForCompleteBoardList(t *testing.T) {
	b := newFakeBoard()
	b.listPageSize = 1
	b.sessions["a0"] = extension.SessionInfo{ID: "a0", Tool: "agent"}
	replies := hosts{"mini": snapshot{Version: wireVersion, Sessions: []machineSession{live("a1", "working")}}}
	m := newMirrors(testConfig(), b, nil, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	m.pass(context.Background())
	b.listCalls = nil
	m.pass(context.Background())
	if len(b.listCalls) != 2 || b.listCalls[0].Limit != 500 || b.listCalls[1].Limit != 500 || b.listCalls[1].After != "a0" {
		t.Fatal(b.listCalls)
	}
	replies["mini"] = snapshot{Version: wireVersion, Sessions: []machineSession{}}
	b.listErrAfter = "a0"
	m.pass(context.Background())
	if m.state.Mirrors["mini/a1"] != "l1" || len(b.archives) != 0 {
		t.Fatal(m.state, b.archives)
	}
	b.listErrAfter = ""
	b.listCursor = "a0"
	m.pass(context.Background())
	if m.state.Mirrors["mini/a1"] != "l1" || len(b.archives) != 0 {
		t.Fatal(m.state, b.archives)
	}
	b.listCursor = ""
	m.pass(context.Background())
	if len(m.state.Mirrors) != 0 || strings.Join(b.archives, ",") != "l1" {
		t.Fatal(m.state, b.archives)
	}
}

func TestPassKeepsMappingWhenArchiveFails(t *testing.T) {
	b := newFakeBoard()
	replies := hosts{"mini": snapshot{Version: wireVersion, Sessions: []machineSession{live("a1", "working")}}}
	m := newMirrors(testConfig(), b, nil, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	m.pass(context.Background())
	replies["mini"] = snapshot{Version: wireVersion, Sessions: []machineSession{}}
	b.archiveErr = errors.New("board busy")
	m.pass(context.Background())
	saved, err := readState(m.path)
	if err != nil || m.state.Mirrors["mini/a1"] != "l1" || !sameState(saved, m.state) || m.pinned["l1"] != "working" {
		t.Fatal(m.state, saved, err, m.pinned)
	}
	b.archiveErr = nil
	m.pass(context.Background())
	if len(m.state.Mirrors) != 0 || strings.Join(b.archives, ",") != "l1" {
		t.Fatal(m.state, b.archives)
	}
}

func TestPassRetriesFailedSaveEvenWhenListFails(t *testing.T) {
	b := newFakeBoard()
	replies := hosts{"mini": snapshot{Version: wireVersion, Sessions: []machineSession{live("a1", "working")}}}
	m := newMirrors(testConfig(), b, nil, quiet(), replies.run, machineStore(t, t.TempDir(), mini))
	m.path = filepath.Join(filepath.Dir(m.path), "missing", stateFile)
	m.pass(context.Background())
	if !m.dirty || m.state.Mirrors["mini/a1"] != "l1" {
		t.Fatal(m.dirty, m.state)
	}
	if err := os.Mkdir(filepath.Dir(m.path), 0700); err != nil {
		t.Fatal(err)
	}
	b.sessions["a0"] = extension.SessionInfo{ID: "a0", Tool: "agent"}
	b.listPageSize, b.listErrAfter = 1, "a0"
	m.pass(context.Background())
	saved, err := readState(m.path)
	if err != nil || m.dirty || saved.Mirrors["mini/a1"] != "l1" || len(b.launches) != 1 || len(b.archives) != 0 {
		t.Fatal(saved, m.dirty, err, b.launches, b.archives)
	}
}

func TestPassWaitsForDamagedStateToBeRepaired(t *testing.T) {
	for _, file := range []string{stateFile, storeFileName} {
		t.Run(file, func(t *testing.T) {
			b := newFakeBoard()
			b.sessions["l1"] = mirrorRow("l1", true)
			dir := machineStore(t, t.TempDir(), mini)
			mapped := withMirrors("mini/a1", "l1")
			if err := writeJSON(filepath.Join(dir, stateFile), mapped); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, file)
			good, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("null"), 0600); err != nil {
				t.Fatal(err)
			}
			replies := hosts{"mini": snapshot{Version: wireVersion, Sessions: []machineSession{live("a1", "waiting")}}}
			m := newMirrors(testConfig(), b, nil, quiet(), replies.run, dir)
			m.pass(context.Background())
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "null" || len(b.archives)+len(b.launches)+len(b.pins) != 0 {
				t.Fatal(string(data), err, b.archives, b.launches, b.pins)
			}
			if err := writeFileAtomic(path, good); err != nil {
				t.Fatal(err)
			}
			m.pass(context.Background())
			if !sameState(m.state, mapped) || len(b.archives)+len(b.launches) != 0 || strings.Join(b.pins, ",") != "l1=waiting" {
				t.Fatal(m.state, b.archives, b.launches, b.pins)
			}
		})
	}
}

func TestExecRunnerBoundsCommandOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := execRunner(ctx, []string{"sh", "-c", `printf '{"version":1,"sessions":[]}'; head -c 8388608 /dev/zero`})
	if out != nil || !errors.Is(err, errCommandOutput) {
		t.Fatalf("got %d bytes and error %v", len(out), err)
	}
	out, err = execRunner(ctx, []string{"sh", "-c", `printf 'dropped\n' >&2; head -c 8192 /dev/zero >&2; printf '\nfinal diagnosis\n' >&2; exit 1`})
	if out != nil || err == nil || len(err.Error()) > stderrTailSize+32 ||
		strings.Contains(err.Error(), "dropped") || !strings.HasSuffix(err.Error(), "final diagnosis") {
		t.Fatalf("got %d stdout bytes and an unexpected stderr tail", len(out))
	}
}
