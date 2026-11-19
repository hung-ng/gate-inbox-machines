package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/usestring/gate-inbox/extension"
)

// wireVersion is the shape of `machines sessions --json`. Both ends run this
// build, but not always the same release of it: a board that reads a
// version it does not know shows the host as unreachable and says why,
// rather than guessing at fields.
const wireVersion = 1

// snapshot is what `machines sessions --json` prints: the agent sessions a
// host's board is running, as that board reads them.
type snapshot struct {
	// About is what the machine says about itself. A build from before
	// it leaves it out, and the board then goes on its own settings.
	About        *about           `json:"about,omitempty"`
	BoardRunning *bool            `json:"board_running,omitempty"`
	Stats        *workerStats     `json:"stats,omitempty"`
	Tools        []string         `json:"tools,omitempty"`
	Version      int              `json:"version"`
	Sessions     []machineSession `json:"sessions"`
}

type machineSession struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Tool      string `json:"tool"`
	Group     string `json:"group,omitempty"`
	Directory string `json:"directory,omitempty"`
	Status    string `json:"status"`
	Running   bool   `json:"running"`
	Scrolling bool   `json:"scrolling,omitempty"`
	// Attachable is whether the session has a tmux session of its own,
	// gi_<id>, on the tmux server the host's mirrors attach through. A
	// session the board launched does. A pane the board adopted as it was,
	// started by hand in some other tmux session, does not, and is not
	// mirrored: attaching to its tmux session would put the whole of that
	// session, other windows and all, in the mirror.
	Attachable bool   `json:"attachable"`
	ParentID   string `json:"parent_id,omitempty"`
}

// The board's own session ids. The id goes into a tmux target on the far
// side, so anything else is refused before it gets there.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]+$`)

// mirrorRole is the role mirrors are launched under, qualified the way the
// board reports it back.
const (
	roleName   = "mirror"
	mirrorRole = extensionID + "/" + roleName
)

// listSessions reads agents and top-level terminals for `machines sessions`.
// It leaves out this extension's own mirrors, and anything else on its
// tool: two boards that mirror each other would otherwise mirror each
// other's mirrors, a row deeper each poll.
//
// attachable is the session ids with a gi_<id> tmux session, as
// tmuxSessions read them.
func listSessions(ctx context.Context, sessions extension.SessionService, attachable map[string]bool) (snapshot, error) {
	snap := snapshot{Version: wireVersion, Sessions: []machineSession{}}
	rows, err := allSessions(ctx, sessions.List, extension.SessionFilter{IncludeTerminals: true})
	if err != nil {
		return snapshot{}, err
	}
	for _, s := range rows {
		// The picker can launch the mirror tool without the mirror role.
		// Those panes have no target and are left out too.
		if (s.Terminal && s.ParentID != "") || s.Archived || s.Role == mirrorRole || s.Tool == toolName || s.Tool == "machines" {
			continue
		}
		snap.Sessions = append(snap.Sessions, machineSession{
			ID: s.ID, Name: s.Name, Tool: s.Tool, Group: s.Group, Directory: s.Directory,
			Status: s.Status, Running: s.Running, ParentID: s.ParentID,
			Attachable: attachable[s.ID],
		})
	}
	return snap, nil
}

func allSessions(ctx context.Context, list func(context.Context, extension.SessionFilter) (extension.SessionList, error), filter extension.SessionFilter) ([]extension.SessionInfo, error) {
	var rows []extension.SessionInfo
	matched := 0
	seen := map[string]bool{}
	cursors := map[string]bool{}
	filter.Limit = 500
	for {
		page, err := list(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, row := range page.Sessions {
			if seen[row.ID] {
				return nil, fmt.Errorf("board listed session %s twice", row.ID)
			}
			seen[row.ID] = true
		}
		rows = append(rows, page.Sessions...)
		matched = max(matched, page.Matched)
		if !page.Truncated {
			// A row can move behind the cursor between pages. Retry the
			// scan rather than treating a missed row as gone.
			if len(rows) < matched {
				return nil, fmt.Errorf("board matched %d sessions but returned %d across all pages", matched, len(rows))
			}
			return rows, nil
		}
		if page.Cursor == "" || cursors[page.Cursor] {
			return nil, errors.New("board returned an incomplete session list without a new cursor")
		}
		cursors[page.Cursor] = true
		filter.After = page.Cursor
	}
}

// parseSnapshot reads another board's `machines sessions --json`. Everything in
// it came from another machine, so what the board will act on is checked
// here: an id that is not a session id is dropped with the rest of the
// reply rather than passed on to an attach.
func parseSnapshot(r io.Reader) (snapshot, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxCommandOutput+1))
	if err != nil {
		return snapshot{}, err
	}
	if len(data) > maxCommandOutput {
		return snapshot{}, errCommandOutput
	}
	return decodeSnapshot(data)
}

func decodeSnapshot(data []byte) (snapshot, error) {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return snapshot{}, fmt.Errorf("the machine's reply is not a session list: %w", err)
	}
	return checkSnapshot(snap)
}

// parseSnapshotAfterGreeting reads a reply that came through a login
// shell, whose login files may print a greeting first: the list is the
// first line that starts one.
func parseSnapshotAfterGreeting(out []byte) (snapshot, error) {
	if len(out) > maxCommandOutput {
		return snapshot{}, errCommandOutput
	}
	var first error
	for offset := 0; offset < len(out); {
		line, rest, more := bytes.Cut(out[offset:], []byte("\n"))
		if line = bytes.TrimSpace(line); len(line) > 0 && line[0] == '{' {
			snap, err := decodeSnapshot(out[offset:])
			if err == nil {
				return snap, nil
			}
			if first == nil {
				first = err
			}
		}
		if !more {
			break
		}
		offset = len(out) - len(rest)
	}
	if first == nil {
		first = errors.New("the machine's reply contains no session list")
	}
	return snapshot{}, first
}

func checkSnapshot(snap snapshot) (snapshot, error) {
	if snap.Version != wireVersion {
		return snapshot{}, fmt.Errorf("the machine's build speaks version %d of the session list and this one speaks %d; run the same build on both", snap.Version, wireVersion)
	}
	if snap.Sessions == nil {
		return snapshot{}, errors.New("the machine's reply must include a sessions array")
	}
	seen := map[string]bool{}
	for _, s := range snap.Sessions {
		if !sessionIDPattern.MatchString(s.ID) {
			return snapshot{}, fmt.Errorf("the machine listed a session id %q that is not one", s.ID)
		}
		if seen[s.ID] {
			return snapshot{}, fmt.Errorf("the machine listed session %s twice", s.ID)
		}
		seen[s.ID] = true
	}
	if snap.About != nil {
		a := snap.About.clean()
		snap.About = &a
	}
	return snap, nil
}

func formatSnapshot(snap snapshot) string {
	if len(snap.Sessions) == 0 {
		return "no agent sessions"
	}
	var b strings.Builder
	for _, s := range snap.Sessions {
		note := "running"
		switch {
		case !s.Running:
			note = "not running"
		case !s.Attachable:
			note = "running, not mirrored: no gi_" + s.ID + " tmux session"
		}
		fmt.Fprintf(&b, "%s  %-10s  %-8s  %s  (%s)\n", s.ID, s.Status, s.Tool, s.Name, note)
	}
	return strings.TrimRight(b.String(), "\n")
}

var errNoSessions = errors.New("the board lent this command no session service")

// tmuxSessions is the ids of the gi_<id> tmux sessions on one tmux server,
// asked through argv: a tmux command line up to, not including, its
// subcommand. A server that is not running has none. Any other failure is
// an error, so a host whose tmux cannot be run reads as unreachable with
// the reason, rather than as a board with nothing to mirror.
func tmuxSessions(ctx context.Context, run runner, argv []string) (map[string]bool, []machineSession, error) {
	// tmux 3.8 replaces tabs in format output. Session names we use have no
	// spaces, so split only the first two spaces and keep the directory whole.
	out, err := run(ctx, append(append([]string{}, argv...), "list-sessions", "-F",
		"#{session_name} #{"+terminalMark+"} #{pane_current_path}"))
	if err != nil {
		if tmuxHasNothing(err, false) {
			return map[string]bool{}, nil, nil
		}
		return nil, nil, fmt.Errorf("could not list tmux sessions: %w", err)
	}
	var terminals []machineSession
	for _, line := range strings.Split(string(out), "\n") {
		columns := strings.SplitN(line, " ", 3)
		id, ok := strings.CutPrefix(columns[0], "gi_")
		if len(columns) == 3 && columns[1] == "1" && ok && sessionIDPattern.MatchString(id) {
			terminals = append(terminals, terminalSession(id, columns[2]))
		}
	}
	return parseTmuxSessions(string(out)), terminals, nil
}

// tmuxHasNothing checks the final tmux diagnostic. Other socket errors
// also start with "error connecting to", including a non-socket at the path.
func tmuxHasNothing(err error, panes bool) bool {
	msg := lastLine(strings.TrimSpace(err.Error()))
	if strings.HasPrefix(msg, "exit status ") {
		_, msg, _ = strings.Cut(msg, ": ")
	}
	return strings.HasPrefix(msg, "no server running on ") ||
		(strings.HasPrefix(msg, "error connecting to ") && strings.HasSuffix(msg, " (No such file or directory)")) ||
		(panes && msg == "no current target")
}

func parseTmuxSessions(out string) map[string]bool {
	ids := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if id, ok := strings.CutPrefix(fields[0], "gi_"); ok && sessionIDPattern.MatchString(id) {
			ids[id] = true
		}
	}
	return ids
}

// Missing board_running predates board detection and must not suppress mirrors.
func (s snapshot) boardRunning() bool { return s.BoardRunning == nil || *s.BoardRunning }

func boolPtr(v bool) *bool { return &v }

// Only read the singleton PID. Even a brief flock probe can make a starting
// board take its takeover path and signal the PID recorded here.
func boardRunning(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "manager.lock"))
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || pid <= 0 || pid > 1<<31-1 {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func tmuxScrolling(ctx context.Context, run runner, argv []string) (map[string]bool, error) {
	out, err := run(ctx, append(append([]string{}, argv...), "list-panes", "-a", "-F", "#{session_name} #{pane_in_mode}"))
	if err != nil {
		if tmuxHasNothing(err, true) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("could not list tmux pane modes: %w", err)
	}
	return parseTmuxScrolling(string(out)), nil
}

func parseTmuxScrolling(out string) map[string]bool {
	modes := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "1" {
			continue
		}
		if id, ok := strings.CutPrefix(fields[0], "gi_"); ok && sessionIDPattern.MatchString(id) {
			modes[id] = true
		}
	}
	return modes
}
