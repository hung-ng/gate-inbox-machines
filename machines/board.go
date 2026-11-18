package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

// boardOps is the part of the board a reconcile acts through. The board's
// own BoardHost is one; tests use a fake.
type boardOps interface {
	List(ctx context.Context, filter extension.SessionFilter) (extension.SessionList, error)
	Launch(ctx context.Context, req extension.LaunchRequest) (extension.SessionInfo, error)
	PinStatus(ctx context.Context, id, status string) error
	Archive(ctx context.Context, id string) error
	Replace(ctx context.Context, id string, req extension.LaunchRequest, opts ...extension.ReplaceOptions) (extension.SessionInfo, extension.ReplaceHandle, error)
}

// runner runs one command and returns what it printed. Tests swap it for
// one that answers without ssh.
type runner func(ctx context.Context, argv []string) (stdout []byte, err error)

func execRunner(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, lastLine(msg))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// launchBackoff is how long a mirror whose launch failed waits before it is
// tried again, so a launch the board refuses is not retried every pass.
const launchBackoff = 30 * time.Second

// mirrors runs the reconcile loop on the board.
type mirrors struct {
	cfg       config
	board     boardOps
	ui        extension.UIHost
	log       *slog.Logger
	run       runner
	observeMu sync.Mutex
	path      string // the state file
	now       func() time.Time
	state     state
	pinned    map[string]string
	failed    map[string]time.Time
	badges    map[string]string
	lastErr   map[string]string
	skipped   map[string]int
	// current is the mirrors this run of the board launched or reconnected.
	current  map[string]bool
	wake     <-chan string
	since    map[string]time.Time
	stopped  map[string]bool
	owned    map[string]bool
	tools    *toolCache
	previews *previewRoutes
	// reg is where the machines come from, read again every pass so one
	// added or paused from a shell shows up without a restart.
	reg *registry
	// status is where each pass leaves every machine's state, for the
	// machines screen.
	status *statusBoard
}

// stateFile is the mirrors' saved state, in the extension's data dir.
const stateFile = "mirrors.json"

func newMirrors(cfg config, board boardOps, ui extension.UIHost, log *slog.Logger, run runner, dataDir string) *mirrors {
	m := &mirrors{
		cfg: cfg, board: board, ui: ui, log: log, run: run,
		path:    filepath.Join(dataDir, stateFile),
		now:     time.Now,
		pinned:  map[string]string{},
		failed:  map[string]time.Time{},
		badges:  map[string]string{},
		lastErr: map[string]string{},
		skipped: map[string]int{},
		current: map[string]bool{},
		since:   map[string]time.Time{},
		stopped: map[string]bool{},
		owned:   map[string]bool{},
	}
	m.reg = newRegistry(cfg.Hosts, dataDir)
	m.state = m.load()
	return m
}

func (m *mirrors) loop(ctx context.Context) {
	tick := time.NewTicker(m.cfg.Poll)
	defer tick.Stop()
	for {
		m.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-tick.C:
		}
	}
}

// pass is one poll of every host and one reconcile.
func (m *mirrors) pass(ctx context.Context) {
	if m.reg.reload() {
		m.log.Info("the machines changed on disk", "machines", len(m.reg.all()))
	}
	if issue := m.reg.issue(); issue != m.lastErr[""] {
		if issue != "" {
			m.log.Warn("some added machines can't be used", "err", issue)
		}
		m.lastErr[""] = issue
	}
	hosts := m.reg.active()
	observed := m.observe(ctx, hosts)
	m.publish(observed)
	local, setups, err := m.localRows(ctx)
	if err != nil {
		m.log.Warn("could not list the board's mirrors", "err", err)
		return
	}
	m.closeSetups(ctx, setups)
	actions, next := plan(m.state, hosts, observed, local, m.pinned, m.current)
	for _, key := range sortedKeys(next.Dismissed) {
		if _, before := m.state.Dismissed[key]; !before {
			m.log.Info("a mirror was archived from the board; it stays away until its session stops running", "key", key)
		}
	}
	for _, key := range sortedKeys(next.Mirrors) {
		if id, before := m.state.Dismissed[key]; before && id == next.Mirrors[key] {
			m.log.Info("a mirror was restored from the archive; following it again", "key", key, "session", id)
		}
	}
	for _, a := range actions {
		switch a.Kind {
		case actArchive:
			if err := m.board.Archive(ctx, a.LocalID); err != nil {
				m.log.Warn("could not archive a mirror", "session", a.LocalID, "key", a.Key, "err", err)
			} else {
				m.log.Info("archived a mirror", "session", a.LocalID, "key", a.Key, "why", a.Why)
			}
			delete(m.pinned, a.LocalID)
			delete(m.badges, a.LocalID)
			delete(m.owned, a.LocalID)
			delete(m.current, a.LocalID)
		case actPin:
			m.pin(ctx, a.LocalID, a.Status)
		case actRelaunch:
			if until, ok := m.failed[a.Key]; ok && m.now().Before(until) {
				continue
			}
			// Replace keeps the row's place, name and priority. A replacement
			// takes its predecessor's group, so the request names none.
			req := launchRequest(a.Host, a.Session)
			req.Group, req.Name = "", ""
			info, _, err := m.board.Replace(ctx, a.LocalID, req)
			if err != nil {
				m.failed[a.Key] = m.now().Add(launchBackoff)
				m.log.Warn("could not reconnect a mirror", "key", a.Key, "session", a.LocalID, "err", err)
				continue
			}
			delete(m.failed, a.Key)
			delete(m.pinned, a.LocalID)
			delete(m.badges, a.LocalID)
			delete(m.owned, a.LocalID)
			delete(m.current, a.LocalID)
			m.current[info.ID] = true
			next.Mirrors[a.Key] = info.ID
			m.log.Info("reconnected a mirror", "key", a.Key, "was", a.LocalID, "session", info.ID, "why", a.Why)
			m.pin(ctx, info.ID, a.Status)
			// Replace leaves the old row dead with its last screen kept, for a
			// revive. A mirror has nothing to revive, so the old row goes now
			// rather than lingering as a second, dead copy of the session.
			if err := m.board.Archive(ctx, a.LocalID); err != nil {
				m.log.Warn("could not archive the mirror a reconnect replaced", "session", a.LocalID, "err", err)
			}
		case actLaunch:
			if until, ok := m.failed[a.Key]; ok && m.now().Before(until) {
				continue
			}
			info, err := m.board.Launch(ctx, launchRequest(a.Host, a.Session))
			if err != nil {
				m.failed[a.Key] = m.now().Add(launchBackoff)
				m.log.Warn("could not launch a mirror", "key", a.Key, "err", err)
				continue
			}
			delete(m.failed, a.Key)
			m.current[info.ID] = true
			next.Mirrors[a.Key] = info.ID
			m.log.Info("mirroring a session", "key", a.Key, "session", info.ID, "name", info.Name)
			m.pin(ctx, info.ID, a.Status)
		}
	}
	if !sameState(m.state, next) {
		if err := m.save(next); err != nil {
			m.log.Warn("could not save the mirror state", "err", err)
		}
	}
	m.state = next
	if m.previews != nil {
		m.previews.update(next, observed)
	}
	m.decorate(observed, local)
}

func (m *mirrors) pin(ctx context.Context, id, status string) {
	if err := m.board.PinStatus(ctx, id, status); err != nil {
		delete(m.pinned, id)
		m.log.Warn("could not pin a mirror's status", "session", id, "status", status, "err", err)
		return
	}
	m.pinned[id] = status
}

// observe polls every host at once, each bounded by its own timeout, so one
// slow host does not hold the others' rows back.
func (m *mirrors) observe(ctx context.Context, hosts []host) []observation {
	observed := make([]observation, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			observed[i] = m.observeOne(ctx, h)
		}()
	}
	wg.Wait()
	for _, obs := range observed {
		name := obs.Host.Name
		if obs.Err != nil {
			if m.since[name].IsZero() {
				m.since[name] = m.now()
			}
		} else {
			delete(m.since, name)
			if m.tools != nil {
				m.tools.set(name, obs.Snapshot.Tools)
			}
			stopped := !obs.Snapshot.boardRunning()
			if stopped != m.stopped[name] {
				m.log.Info("machine board running changed", "machine", name, "board_running", !stopped)
			}
			m.stopped[name] = stopped
		}
	}
	return observed
}

func (m *mirrors) observeOne(ctx context.Context, h host) observation {
	timeout := max(3*m.cfg.Poll, 15*time.Second)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	obs := observation{Host: h}
	obs.Snapshot, obs.Err = pollHost(ctx, m.run, m.cfg, h)
	if obs.Err == nil && obs.Snapshot.About != nil {
		changed, err := m.reg.learn(h.Name, h.Destination, *obs.Snapshot.About)
		if err != nil {
			m.log.Warn("could not save what a machine said about itself", "machine", h.Name, "err", err)
		}
		if changed {
			learned, ok := m.reg.host(h.Name)
			if ok {
				m.log.Info("machine described itself", "machine", h.Name, "hostname", learned.About.Hostname,
					"gate_inbox", learned.GateInbox, "tmux", learned.Tmux, "tmux_socket", learned.TmuxSocket, "version", learned.About.Version)
				// The reply said which sessions can be attached to through
				// the tmux it was asked about. If the machine's own board
				// keeps them on another server, ask again about that one
				// before any mirror is started or put away.
				if learned.TmuxSocket != h.TmuxSocket || learned.Tmux != h.Tmux {
					obs.Snapshot, obs.Err = pollHost(ctx, m.run, m.cfg, learned)
				}
				obs.Host = learned
			}
		}
	}
	err := obs.Err
	m.observeMu.Lock()
	defer m.observeMu.Unlock()
	// Say when a host goes away and when it comes back, not once a pass.
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != m.lastErr[h.Name] {
		if msg == "" {
			m.log.Info("machine is reachable", "machine", h.Name)
		} else {
			m.log.Warn("machine is unreachable", "machine", h.Name, "err", msg)
		}
		m.lastErr[h.Name] = msg
	}
	// Running sessions a mirror cannot reach are adopted panes, or a
	// tmux_socket this config has wrong. Either way the operator should be
	// able to find out why they are missing.
	unreachable := 0
	for _, s := range obs.Snapshot.Sessions {
		if s.Running && !s.Attachable {
			unreachable++
		}
	}
	if unreachable != m.skipped[h.Name] {
		if unreachable > 0 {
			m.log.Info("machine has running sessions with no gi_ tmux session to attach to; they are not mirrored (adopted panes, or tmux_socket differs from that machine's board)",
				"machine", h.Name, "sessions", unreachable)
		}
		m.skipped[h.Name] = unreachable
	}
	return obs
}

// localRows is every unarchived mirror row on this board, and every
// setup row.
func (m *mirrors) localRows(ctx context.Context) (map[string]extension.SessionInfo, map[string]extension.SessionInfo, error) {
	local := map[string]extension.SessionInfo{}
	setups := map[string]extension.SessionInfo{}
	after := ""
	for {
		page, err := m.board.List(ctx, extension.SessionFilter{After: after})
		if err != nil {
			return nil, nil, err
		}
		for _, s := range page.Sessions {
			switch {
			case s.Archived:
			case s.Role == mirrorRole:
				local[s.ID] = s
			case s.Role == setupRole:
				setups[s.ID] = s
			}
		}
		if !page.Truncated || page.Cursor == "" {
			return local, setups, nil
		}
		after = page.Cursor
	}
}

// closeSetups archives each setup row whose pane has finished: its
// machine was added or not, and the row has nothing more to show. The pane
// marks itself done, since a pane whose command ends is left on a shell
// and still reads as running.
func (m *mirrors) closeSetups(ctx context.Context, setups map[string]extension.SessionInfo) {
	for _, id := range sortedKeys(setups) {
		if setups[id].Running && !setupDone(m.reg.dir, id) {
			continue
		}
		if err := m.board.Archive(ctx, id); err != nil {
			m.log.Warn("could not archive a finished setup row", "session", id, "err", err)
			continue
		}
		clearSetupDone(m.reg.dir, id)
		m.log.Info("archived a finished setup row", "session", id)
	}
}

// pollHost asks one machine for its sessions. When the machine can't find
// gate-inbox, and the operator's settings don't name it, it is asked once
// more through the operator's login shell there, by its bare name: that is
// where an installer usually puts it on PATH, and it covers a binary that
// moved since the machine last said where it was. The reply says where it
// is now, and later polls run it by that path, straight from ssh.
func pollHost(ctx context.Context, run runner, cfg config, h host) (snapshot, error) {
	out, err := run(ctx, listArgv(cfg, h))
	if err == nil {
		return parseSnapshot(bytes.NewReader(out))
	}
	if !notFound(err) || h.Settings.GateInbox != "" {
		return snapshot{}, err
	}
	bare := h
	bare.GateInbox = "gate-inbox"
	out, loginErr := run(ctx, loginShell(cfg, h.Destination, listCommand(bare)))
	if loginErr != nil {
		return snapshot{}, err
	}
	snap, parseErr := parseSnapshotAfterGreeting(out)
	if parseErr != nil || snap.About == nil || snap.About.GateInbox == "" {
		return snapshot{}, err
	}
	return snap, nil
}

// launchRequest is the mirror row for one session on another machine: a pane running
// `machines attach <machine> <id>`, filed under the machine's group. Its
// name leads with the machine's, since the board's main list shows a row's
// name and tool, not the badge that names its machine.
func launchRequest(h host, s machineSession) extension.LaunchRequest {
	name := s.Name
	if name == "" {
		name = s.Tool + "-" + s.ID
	}
	name = h.Name + "/" + name
	return extension.LaunchRequest{
		Tool:  toolName,
		Name:  name,
		Group: h.Group,
		Role:  roleName,
		Args:  []string{h.Name, s.ID},
	}
}

// decorate marks each mirror with its host, and says when the host cannot
// be reached or the mirror's pane has ended.
func (m *mirrors) decorate(observed []observation, local map[string]extension.SessionInfo) {
	if m.ui == nil {
		return
	}
	reachable := map[string]bool{}
	status := map[string]string{}
	tools := map[string]string{}
	scrolling := map[string]bool{}
	stopped := map[string]bool{}
	for _, obs := range observed {
		reachable[obs.Host.Name] = obs.Err == nil
		stopped[obs.Host.Name] = !obs.Snapshot.boardRunning()
		for _, s := range obs.Snapshot.Sessions {
			status[mirrorKey(obs.Host.Name, s.ID)] = s.Status
			scrolling[mirrorKey(obs.Host.Name, s.ID)] = s.Scrolling
			tools[mirrorKey(obs.Host.Name, s.ID)] = s.Tool
		}
	}
	for _, key := range sortedKeys(m.state.Mirrors) {
		id := m.state.Mirrors[key]
		hostName, sessionID := splitKey(key)
		badge := extension.Badge{}
		owned := !reachable[hostName] || stopped[hostName]
		if m.owned[id] != owned {
			m.ui.Own(id, owned)
		}
		m.owned[id] = owned
		fact := hostName + " · " + tools[key] + " · " + status[key] + " · gi_" + sessionID
		switch {
		case !reachable[hostName]:
			badge = extension.Badge{Text: "✕ " + hostName + " offline", Short: "✕", Tone: extension.ToneBad}
			fact = hostName + " · unreachable since " + m.since[hostName].Format("15:04") + " · gi_" + sessionID
		case stopped[hostName]:
			badge = extension.Badge{Text: "✕ " + hostName + " board stopped", Short: "✕", Tone: extension.ToneBad}
			fact = hostName + " · no status until you start that machine's board · gi_" + sessionID
		case scrolling[key]:
			badge = extension.Badge{Text: "↕ scroll mode", Short: "↕", Tone: extension.ToneWarn}
			fact = hostName + " · " + tools[key] + " · " + status[key] + " · scroll mode: press q inside the mirror to type again · gi_" + sessionID
		case local[id].ID != "" && !local[id].Running:
			badge = extension.Badge{Text: "⇄ " + hostName + " detached", Short: "⇄ ·", Tone: extension.ToneWarn}
		}
		if m.badges[id] == badge.Text+fact {
			continue
		}
		m.badges[id] = badge.Text + fact
		if badge.Text == "" {
			m.ui.Decorate(id)
		} else {
			m.ui.Decorate(id, badge)
		}
		m.ui.Fact(id, extension.Fact{Label: "machine", Text: fact})
	}
}

func (m *mirrors) load() state {
	st := newState()
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return st
	}
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err != nil {
		// A state file that cannot be read is started over: its mirrors are
		// archived as rows nothing names, and launched again.
		m.log.Warn("could not read the mirror state; starting over", "path", m.path, "err", err)
		return newState()
	}
	if st.Mirrors == nil {
		st.Mirrors = map[string]string{}
	}
	if st.Dismissed == nil {
		st.Dismissed = map[string]string{}
	}
	return st
}

// save writes the state by rename, so a reader never sees half a file.
func (m *mirrors) save(st state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".mirrors-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.path)
}

func sameState(a, b state) bool {
	if len(a.Mirrors) != len(b.Mirrors) || len(a.Dismissed) != len(b.Dismissed) {
		return false
	}
	for k, v := range a.Mirrors {
		if b.Mirrors[k] != v {
			return false
		}
	}
	for k, v := range a.Dismissed {
		if w, ok := b.Dismissed[k]; !ok || w != v {
			return false
		}
	}
	return true
}
