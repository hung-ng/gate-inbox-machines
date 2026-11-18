package machines

import (
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
	BoardRunning *bool            `json:"board_running,omitempty"`
	Tools        []string         `json:"tools,omitempty"`
	Version      int              `json:"version"`
	Sessions     []machineSession `json:"sessions"`
}

type machineSession struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Tool      string `json:"tool"`
	Group     string `json:"group,omitempty"`
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

// listSessions reads every agent session a board runs, for `machines sessions`.
// It leaves out this extension's own mirrors, and anything else on its
// tool: two boards that mirror each other would otherwise mirror each
// other's mirrors, a row deeper each poll.
//
// attachable is the session ids with a gi_<id> tmux session, as
// tmuxSessions read them.
func listSessions(ctx context.Context, sessions extension.SessionService, attachable map[string]bool) (snapshot, error) {
	snap := snapshot{Version: wireVersion, Sessions: []machineSession{}}
	after := ""
	for {
		page, err := sessions.List(ctx, extension.SessionFilter{After: after})
		if err != nil {
			return snapshot{}, err
		}
		for _, s := range page.Sessions {
			// A session on the machines tool without the mirror role is one
			// somebody started from the picker by hand: an attach with
			// nothing to attach to, not an agent.
			if s.Terminal || s.Archived || s.Role == mirrorRole || s.Tool == toolName || s.Tool == "machines" {
				continue
			}
			snap.Sessions = append(snap.Sessions, machineSession{
				ID: s.ID, Name: s.Name, Tool: s.Tool, Group: s.Group,
				Status: s.Status, Running: s.Running, ParentID: s.ParentID,
				Attachable: attachable[s.ID],
			})
		}
		if !page.Truncated || page.Cursor == "" {
			return snap, nil
		}
		after = page.Cursor
	}
}

// parseSnapshot reads another board's `machines sessions --json`. Everything in
// it came from another machine, so what the board will act on is checked
// here: an id that is not a session id is dropped with the rest of the
// reply rather than passed on to an attach.
func parseSnapshot(r io.Reader) (snapshot, error) {
	var snap snapshot
	decoder := json.NewDecoder(io.LimitReader(r, 8<<20))
	if err := decoder.Decode(&snap); err != nil {
		return snapshot{}, fmt.Errorf("the machine's reply is not a session list: %w", err)
	}
	if snap.Version != wireVersion {
		return snapshot{}, fmt.Errorf("the machine's build speaks version %d of the session list and this one speaks %d; run the same build on both", snap.Version, wireVersion)
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
func tmuxSessions(ctx context.Context, run runner, argv []string) (map[string]bool, error) {
	out, err := run(ctx, append(append([]string{}, argv...), "list-sessions", "-F", "#{session_name} #{session_attached}"))
	if err != nil {
		if tmuxHasNothing(err) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("could not list tmux sessions: %w", err)
	}
	return parseTmuxSessions(string(out)), nil
}

// tmuxHasNothing is whether a failed tmux listing only means there was
// nothing to list: no server on the socket, or a server kept up with no
// sessions (exit-empty off), where list-panes -a fails with "no current
// target". Keeping the agents' server up that way is how a Mac lets an agent
// started over ssh read the login keychain, so it must not read as a failure.
func tmuxHasNothing(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "error connecting to") ||
		strings.Contains(msg, "no current target")
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
		if tmuxHasNothing(err) {
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
