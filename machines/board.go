package machines

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/usestring/gate-inbox/extension"
)

// boardOps is the part of the board a reconcile acts through. The board's
// own BoardHost is one; tests use a fake.
type boardOps interface {
	List(ctx context.Context, filter extension.SessionFilter) (extension.SessionList, error)
	Get(ctx context.Context, id string) (extension.SessionInfo, error)
	Launch(ctx context.Context, req extension.LaunchRequest) (extension.SessionInfo, error)
	PinStatus(ctx context.Context, id, status string) error
	Archive(ctx context.Context, id string) error
}

// runner runs one command and returns what it printed. Tests swap it for
// one that answers without ssh.
type runner func(ctx context.Context, argv []string) (stdout []byte, err error)

const (
	maxCommandOutput = 8 << 20
	stderrTailSize   = 4 << 10
)

var errCommandOutput = errors.New("command output exceeds 8 MiB")

type commandOutput struct {
	buf       bytes.Buffer
	oversized bool
}

func (out *commandOutput) Write(p []byte) (int, error) {
	if len(p) > maxCommandOutput-out.buf.Len() {
		out.oversized = true
		return 0, errCommandOutput
	}
	return out.buf.Write(p)
}

type stderrTail []byte

func (tail *stderrTail) Write(p []byte) (int, error) {
	n := len(p)
	if n >= stderrTailSize {
		*tail = append((*tail)[:0], p[n-stderrTailSize:]...)
		return n, nil
	}
	if skip := len(*tail) + n - stderrTailSize; skip > 0 {
		*tail = (*tail)[:copy(*tail, (*tail)[skip:])]
	}
	*tail = append(*tail, p...)
	return n, nil
}

func execRunner(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout commandOutput
	stderr := make(stderrTail, 0, stderrTailSize)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stdout.oversized {
		return nil, errCommandOutput
	}
	if err != nil {
		if msg := strings.TrimSpace(string(stderr)); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.buf.Bytes(), nil
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
	cfg        config
	board      boardOps
	ui         extension.UIHost
	log        *slog.Logger
	run        runner
	observeMu  sync.Mutex
	path       string // the state file
	now        func() time.Time
	state      state
	dirty      bool
	stateIssue string
	pinned     map[string]string
	failed     map[string]time.Time
	badges     map[string]string
	dirs       map[string]string
	tools      map[string]string
	readings   map[string]workerReading
	lastErr    map[string]string
	skipped    map[string]int
	wake       <-chan string
	since      map[string]time.Time
	stopped    map[string]bool
	owned      map[string]bool
	health     map[string]string
	// reg is where the machines come from, read again every pass so one
	// added or paused from a shell shows up without a restart.
	reg *registry
	// status is where each pass leaves every machine's state, for the
	// machines screen.
	status *statusBoard
	// lastIssue is the machines file problem last logged, so it is logged
	// when it changes rather than every pass.
	lastIssue string
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
		dirs:    map[string]string{},
		tools:   map[string]string{},
		lastErr: map[string]string{},
		skipped: map[string]int{},
		since:   map[string]time.Time{},
		stopped: map[string]bool{},
		owned:   map[string]bool{},
	}
	m.reg = newRegistry(dataDir)
	m.state = newState()
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
	if issue := m.reg.issue(); issue != m.lastIssue {
		if issue != "" {
			m.log.Warn("some added machines can't be used", "err", issue)
		}
		m.lastIssue = issue
	}
	configured := m.reg.all()
	active := slices.DeleteFunc(slices.Clone(configured), func(h host) bool { return h.Paused })
	observed := m.observe(ctx, active)
	m.publish(observed)
	if !m.syncState() {
		return
	}
	defer m.flushState()
	if m.reg.issue() != "" {
		return
	}
	local, err := m.localRows(ctx)
	if err != nil {
		m.log.Warn("could not list the board's mirrors", "err", err)
		return
	}
	for id := range m.pinned {
		if !local[id].Running {
			delete(m.pinned, id)
		}
	}
	actions, next := plan(m.state, configured, observed, local, m.pinned)
	for _, key := range sortedKeys(next.Dismissed) {
		if _, before := m.state.Dismissed[key]; !before {
			m.forget(next.Dismissed[key])
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
				if a.Key != "" {
					next.Mirrors[a.Key] = a.LocalID
				}
				continue
			}
			m.log.Info("archived a mirror", "session", a.LocalID, "key", a.Key, "why", a.Why)
			m.forget(a.LocalID)
		case actPin:
			m.pin(ctx, a.LocalID, a.Status)
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
			next.Mirrors[a.Key] = info.ID
			m.log.Info("mirroring a session", "key", a.Key, "session", info.ID, "name", info.Name)
			if a.Status != "" {
				m.pin(ctx, info.ID, a.Status)
			}
		}
	}
	m.dirty = m.dirty || !sameState(m.state, next)
	m.state = next
	m.decorate(observed, local)
	if m.ui != nil && m.status != nil {
		if note := m.status.launchNotices(m.now(), m.state.Mirrors, m.failed); note != "" {
			m.ui.Notify(note)
		}
	}
}

func (m *mirrors) forget(id string) {
	if m.ui != nil && m.owned[id] {
		m.ui.Own(id, false)
	}
	delete(m.pinned, id)
	delete(m.badges, id)
	delete(m.dirs, id)
	delete(m.tools, id)
	delete(m.owned, id)
}

func (m *mirrors) pin(ctx context.Context, id, status string) {
	if err := m.board.PinStatus(ctx, id, status); err != nil {
		delete(m.pinned, id)
		m.log.Warn("could not pin a mirror's status", "session", id, "status", status, "err", err)
		return
	}
	m.pinned[id] = status
}

// observe polls hosts in parallel. Reconcile waits for the last reply or timeout.
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
			if ok && learned.About != nil && learned.Destination == h.Destination {
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
	// tmux server the other board did not report. The operator should be
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

// localRows is every unarchived mirror row on this board.
func (m *mirrors) localRows(ctx context.Context) (map[string]extension.SessionInfo, error) {
	local := map[string]extension.SessionInfo{}
	rows, err := allSessions(ctx, m.board.List, extension.SessionFilter{})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if !row.Archived && row.Role == mirrorRole {
			local[row.ID] = row
		}
	}
	// A reorder during pagination can hide a row. Check any saved active
	// row that List missed before letting plan treat it as dismissed.
	for _, id := range m.state.Mirrors {
		if _, ok := local[id]; ok {
			continue
		}
		row, err := m.board.Get(ctx, id)
		if err != nil {
			// Stock v1.1.0 has no public sentinel for this error.
			if strings.HasPrefix(err.Error(), "session "+id+" does not exist; call ") &&
				strings.HasSuffix(err.Error(), " for current ids") {
				continue
			}
			return nil, fmt.Errorf("look up mirror %s: %w", id, err)
		}
		if row.ID != id || row.Role != mirrorRole {
			return nil, fmt.Errorf("unexpected row returned for mirror %s", id)
		}
		if !row.Archived {
			local[id] = row
		}
	}
	return local, nil
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
	sessions := map[string]machineSession{}
	stopped := map[string]bool{}
	stats := map[string]*workerStats{}
	for _, obs := range observed {
		reachable[obs.Host.Name] = obs.Err == nil
		stopped[obs.Host.Name] = !obs.Snapshot.boardRunning()
		if obs.Err == nil {
			stats[obs.Host.Name] = obs.Snapshot.Stats
		}
		for _, s := range obs.Snapshot.Sessions {
			sessions[mirrorKey(obs.Host.Name, s.ID)] = s
		}
	}
	readings := map[string]workerReading{}
	for _, h := range m.reg.all() {
		readings[h.Name] = m.readings[h.Name].update(stats[h.Name])
	}
	m.readings = readings
	waiting := map[string]int{}
	for _, key := range sortedKeys(m.state.Mirrors) {
		id := m.state.Mirrors[key]
		hostName, _ := splitKey(key)
		if local[id].Status == "waiting" {
			waiting[hostName]++
		}
		badge := extension.Badge{}
		machine, _ := m.reg.host(hostName)
		owned := machine.Paused || !reachable[hostName] || stopped[hostName]
		if m.owned[id] != owned {
			m.ui.Own(id, owned)
		}
		m.owned[id] = owned
		session := sessions[key]
		if session.Directory != "" {
			m.dirs[id] = session.Directory
		}
		if session.Tool != "" {
			m.tools[id] = session.Tool
		}
		remoteTool := m.tools[id]
		fact := hostName
		if remoteTool != "" {
			fact = remoteTool + " on " + hostName
		}
		if reading := readings[hostName]; reading.summary != "" {
			if reading.stale {
				fact += " · stale stats"
			}
			fact += " · " + reading.summary
		}
		detail := session.Status
		switch {
		case machine.Paused:
			badge = extension.Badge{Text: "Ⅱ " + hostName + " paused", Short: "Ⅱ", Tone: extension.ToneWarn}
			detail = "polling paused"
		case !reachable[hostName]:
			badge = extension.Badge{Text: "✕ " + hostName + " offline", Short: "✕", Tone: extension.ToneBad}
			detail = "unreachable since " + m.since[hostName].Format("15:04")
		case stopped[hostName]:
			badge = extension.Badge{Text: "✕ " + hostName + " board stopped", Short: "✕", Tone: extension.ToneBad}
			detail = "no status until you start that machine's board"
		case session.Scrolling:
			badge = extension.Badge{Text: "↕ scroll mode", Short: "↕", Tone: extension.ToneWarn}
			detail = session.Status + " · scroll mode: press q inside the mirror to type again"
		case local[id].ID != "" && !local[id].Running:
			badge = extension.Badge{Text: "⇄ " + hostName + " detached", Short: "⇄ ·", Tone: extension.ToneWarn}
		}
		if detail != "" {
			fact += " · " + detail
		}
		if dir := m.dirs[id]; dir != "" {
			// The stock sidebar hides the entire detail block if it takes
			// too many rows. Keep the end of long paths visible there.
			const pathWidth = 21
			if excess := ansi.StringWidth(dir) - pathWidth; excess > 0 {
				dir = ansi.TruncateLeft(dir, excess+1, "…")
			}
			fact += " · Remote dir: " + dir
		} else if !owned && badge.Text == "" && session.Status == "waiting" {
			fact += " · Press Enter to read and answer on the remote machine"
		}
		if m.badges[id] == badge.Text+"\n"+fact {
			continue
		}
		m.badges[id] = badge.Text + "\n" + fact
		var badges []extension.Badge
		if badge.Text != "" {
			badges = append(badges, badge)
		}
		if remoteTool != "" {
			identity := extension.Badge{Text: remoteTool + " · " + hostName, Short: remoteTool, Tone: extension.ToneAccent}
			if badge.Text == "" {
				identity.Placement = extension.PlaceAfterName
			}
			badges = append(badges, identity)
		}
		m.ui.Decorate(id, badges...)
		m.ui.Fact(id, extension.Fact{Label: "remote", Text: fact})
	}
	m.notifyHealth(reachable, stopped, waiting)
}

func (m *mirrors) notifyHealth(reachable, stopped map[string]bool, waiting map[string]int) {
	health := map[string]string{}
	var notices, paused []string
	for _, h := range m.reg.all() {
		state := "online"
		switch {
		case h.Paused:
			state = "polling paused"
		case !reachable[h.Name]:
			state = "unreachable"
		case stopped[h.Name]:
			state = "board stopped"
		}
		health[h.Name] = state
		previous := m.health[h.Name]
		if previous == state {
			continue
		}
		if state == "online" {
			if previous != "" {
				notices = append(notices, h.Name+" back online")
			}
			continue
		}
		if m.health == nil && h.Paused {
			paused = append(paused, h.Name)
			continue
		}
		note := h.Name + " " + state
		if count := waiting[h.Name]; count > 0 {
			note += fmt.Sprintf(" (%d waiting outside triage)", count)
		}
		notices = append(notices, note)
	}
	if len(paused) > 0 {
		notices = append(notices, "Polling paused on "+strings.Join(paused, ", "))
	}
	m.health = health
	if len(notices) > 0 {
		m.ui.Notify(strings.Join(notices, " · "))
	}
}

// syncState checks the file before reconciling. Keep unsaved changes in
// memory, but wait for a damaged file to be repaired before replacing it.
func (m *mirrors) syncState() bool {
	st, err := readState(m.path)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(m.path); errors.Is(statErr, os.ErrNotExist) {
			err = nil
			st = m.state
			m.dirty = m.dirty || len(st.Mirrors)+len(st.Dismissed) > 0
		}
	}
	if err != nil {
		if msg := err.Error(); msg != m.stateIssue {
			m.log.Warn("could not read the mirror state; pausing reconcile", "path", m.path, "err", err)
			m.stateIssue = msg
		}
		return false
	}
	m.stateIssue = ""
	if !m.dirty {
		m.state = st
	}
	return true
}

func (m *mirrors) flushState() {
	if !m.dirty {
		return
	}
	if err := writeJSON(m.path, m.state); err != nil {
		m.log.Warn("could not save the mirror state", "err", err)
		return
	}
	m.dirty = false
}

func readState(path string) (state, error) {
	var st *state
	if err := readJSON(path, &st); err != nil {
		return state{}, err
	}
	if st == nil {
		return state{}, errors.New("expected an object in mirrors.json")
	}
	if st.Mirrors == nil {
		st.Mirrors = map[string]string{}
	}
	if st.Dismissed == nil {
		st.Dismissed = map[string]string{}
	}
	return *st, nil
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
