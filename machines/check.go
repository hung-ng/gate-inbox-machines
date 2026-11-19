package machines

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// runCheck checks the named machines, or all unpaused machines when no
// names are given. It uses the same SSH command builders as polling.
func (e *Extension) runCheck(ctx context.Context, names []string, out io.Writer) error {
	reg := e.machines()
	hosts := reg.active()
	if len(names) > 0 {
		hosts = nil
		for _, name := range names {
			h, ok := reg.host(name)
			if !ok {
				return fmt.Errorf("no machine %q: add it with `gate-inbox machines add`", name)
			}
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		if len(reg.all()) > 0 {
			return errors.New("every machine is paused: name one to check it, or resume it with `gate-inbox machines resume`")
		}
		return errors.New("no machines configured: add one with `gate-inbox machines add <destination>`")
	}
	if err := ensureControlDir(e.cfg.ControlPath); err != nil {
		return err
	}
	failed := false
	for _, h := range hosts {
		result := e.probe(ctx, h)
		result.print(out, h.Name)
		if result.failed() {
			failed = true
		}
		if result.Snapshot != nil && result.Snapshot.About != nil {
			if _, err := reg.learn(h.Name, h.Destination, *result.Snapshot.About); err != nil && !errors.Is(err, errNoStore) {
				fmt.Fprintf(out, "%s: could not save what it said about itself: %v\n", h.Name, err)
			}
		}
	}
	if failed {
		return errors.New("machine checks failed")
	}
	return nil
}

// sshHint is the fix for an ssh that could not run a command at all.
func sshHint(err error, destination string) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "permission denied"):
		return fmt.Sprintf("run `ssh-copy-id %s` in a terminal (T opens one on the board), then try again", shellWord(destination))
	case strings.Contains(msg, "host key verification failed"):
		return fmt.Sprintf("run `ssh %s true` once in a terminal to accept its host key", shellWord(destination))
	case strings.Contains(msg, "resolve"), strings.Contains(msg, "timed out"), strings.Contains(msg, "timeout"), strings.Contains(msg, "refused"):
		return "check the network, and that the machine accepts SSH logins (on a Mac, Remote Login in System Settings > General > Sharing; on Linux, the sshd service)"
	default:
		return "check the network and SSH configuration"
	}
}

// exitCoder is what an *exec.ExitError offers; asking for the interface
// lets a test runner stand in for a process.
type exitCoder interface{ ExitCode() int }

// commandNotFound is the shell's exit status for a program it can't find.
const commandNotFound = 127

func notFound(err error) bool {
	var exit exitCoder
	return errors.As(err, &exit) && exit.ExitCode() == commandNotFound
}

// checkStep is one step of a probe.
type checkStep struct {
	Name string
	Err  error
	Fix  string
	// Note follows OK, such as where a binary was found.
	Note string
	// Soft is a failure that leaves the machine usable, so `machines add`
	// adds it anyway: a board that is stopped, or has nothing to attach
	// to yet.
	Soft bool
	// Warn is reported but fails nothing.
	Warn bool
}

// probeResult is one machine walked through the board's needs.
type probeResult struct {
	Steps    []checkStep
	Snapshot *snapshot
}

// failed is whether any step failed, soft ones included: `check` reports a
// stopped board as a failure, since nothing on it has a status.
func (r probeResult) failed() bool {
	for _, s := range r.Steps {
		if s.Err != nil && !s.Warn {
			return true
		}
	}
	return false
}

// blocking is the first failed step that keeps a machine from being added.
func (r probeResult) blocking() *checkStep {
	for i, s := range r.Steps {
		if s.Err != nil && !s.Warn && !s.Soft {
			return &r.Steps[i]
		}
	}
	return nil
}

func (r probeResult) print(out io.Writer, label string) {
	for _, line := range r.lines(label) {
		fmt.Fprintln(out, line)
	}
}

func (r probeResult) lines(label string) []string {
	var lines []string
	for _, s := range r.Steps {
		switch {
		case s.Err != nil && s.Warn:
			lines = append(lines, fmt.Sprintf("%s: %s WARN (%v); %s", label, s.Name, s.Err, s.Fix))
		case s.Err != nil:
			lines = append(lines, fmt.Sprintf("%s: %s FAIL (%v); %s", label, s.Name, s.Err, s.Fix))
		case s.Note != "":
			lines = append(lines, fmt.Sprintf("%s: %s OK (%s)", label, s.Name, s.Note))
		default:
			lines = append(lines, fmt.Sprintf("%s: %s OK", label, s.Name))
		}
	}
	if r.Snapshot != nil && r.Snapshot.About != nil {
		lines = append(lines, label+": it reports "+describeAbout(*r.Snapshot.About))
	}
	return lines
}

// describeAbout is what a machine said about itself, in a line.
func describeAbout(a about) string {
	var parts []string
	if a.Hostname != "" {
		parts = append(parts, "hostname "+a.Hostname)
	}
	if a.OS != "" {
		parts = append(parts, a.OS)
	}
	if a.GateInbox != "" {
		parts = append(parts, "gate-inbox at "+a.GateInbox)
	}
	if a.Tmux != "" {
		parts = append(parts, "tmux at "+a.Tmux)
	}
	switch {
	case a.TmuxSocket == nil:
	case *a.TmuxSocket == "":
		parts = append(parts, "sessions on tmux's default server")
	default:
		parts = append(parts, "sessions on tmux socket "+*a.TmuxSocket)
	}
	if a.Version != "" {
		parts = append(parts, "machines "+a.Version)
	}
	return strings.Join(parts, ", ")
}

// probe walks h through each step the board needs from it, stopping at the
// first that fails and leaves nothing after it to try.
func (e *Extension) probe(ctx context.Context, h host) probeResult {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var result probeResult
	step := func(s checkStep) bool {
		result.Steps = append(result.Steps, s)
		return s.Err == nil
	}
	run := func(program string, args ...string) ([]byte, error) {
		return e.run(ctx, sshArgv(e.cfg, false, h.Destination, machineCommand(h, program, args...)))
	}
	if _, err := run("true"); err != nil {
		step(checkStep{Name: "ssh", Err: err, Fix: sshHint(err, h.Destination)})
		return result
	}
	step(checkStep{Name: "ssh"})
	if _, err := run(h.GateInbox, "--version"); err != nil {
		fix := "check that gate-inbox runs on that machine"
		if notFound(err) {
			fix = "remove it and add it again with --gate-inbox, giving this build's full path there"
			if h.Settings.GateInbox == "" {
				// An installer often puts it on a PATH only the login
				// files set.
				if path := e.findThroughLogin(ctx, h, "gate-inbox"); path != "" {
					h.GateInbox = path
					err = nil
					step(checkStep{Name: "gate-inbox", Note: "found through your login shell at " + path})
				}
			}
		}
		if err != nil {
			step(checkStep{Name: "gate-inbox", Err: err, Fix: fix})
			return result
		}
	} else {
		step(checkStep{Name: "gate-inbox"})
	}
	if _, err := run(h.GateInbox, "machines", "--help"); err != nil {
		step(checkStep{Name: "machines command", Err: err, Fix: "install this build there; that machine's board can stay stock"})
		return result
	}
	step(checkStep{Name: "machines command"})
	if _, err := run(h.Tmux, "-V"); err != nil {
		step(checkStep{Name: "tmux", Err: err, Fix: "install tmux there and make it available on PATH"})
		return result
	}
	step(checkStep{Name: "tmux"})
	snap, err := e.listOnce(ctx, h)
	if err == nil && snap.About != nil {
		// The reply says which sessions can be attached to through the tmux
		// it was asked about; if the machine's board keeps them on another
		// server, ask about that one.
		learned := h.withAbout(*snap.About)
		if learned.TmuxSocket != h.TmuxSocket || learned.Tmux != h.Tmux {
			snap, err = e.listOnce(ctx, learned)
		}
		h = learned
	}
	if !step(checkStep{Name: "sessions reply", Err: err, Fix: "install the same build on both machines and check its sessions reply"}) {
		return result
	}
	result.Snapshot = &snap
	if snap.About != nil && snap.About.Version != "" && snap.About.Version != Version {
		step(checkStep{Name: "version", Warn: true,
			Err: fmt.Errorf("that machine runs machines %s and this one runs %s", snap.About.Version, Version),
			Fix: "install the same build on both"})
	}
	if !snap.boardRunning() {
		step(checkStep{Name: "board running", Soft: true, Err: errors.New("board stopped"), Fix: boardStartHint(h)})
		return result
	}
	step(checkStep{Name: "board running"})
	// The poller only mirrors sessions that are both running and attachable.
	hasRunning := false
	for _, s := range snap.Sessions {
		if !s.Running {
			continue
		}
		hasRunning = true
		if s.Attachable {
			step(checkStep{Name: "attachable session"})
			return result
		}
	}
	if hasRunning {
		fix := "check tmux_socket in that board's config.toml"
		if h.About != nil && h.About.TmuxSocket != nil {
			// The machine said which server its board uses, and it was
			// asked about that one.
			fix = notMirroredFix
		}
		step(checkStep{Name: "attachable session", Soft: true, Err: errors.New("no attachable sessions"), Fix: fix})
	}
	return result
}

func boardStartHint(h host) string {
	hint := "on " + h.Destination + ", start Gate Inbox"
	if h.Home != "" {
		hint += " with board home " + h.Home
	}
	if h.TmuxSocket != "" {
		hint += " on tmux socket " + h.TmuxSocket
	}
	return hint + ", then run Check again"
}

func (e *Extension) listOnce(ctx context.Context, h host) (snapshot, error) {
	data, err := e.run(ctx, listArgv(e.cfg, h))
	if err != nil {
		return snapshot{}, err
	}
	return parseSnapshot(bytes.NewReader(data))
}

// notMirroredFix explains running sessions with nothing to attach to on
// the tmux server the machine's own board reports using.
const notMirroredFix = "its running sessions have no gi_ tmux session there, as with panes its board adopted, so they aren't mirrored; press a on that board to bring a pane in"

// findThroughLogin is where the machine's login shell finds program, or
// "" when it finds none.
func (e *Extension) findThroughLogin(ctx context.Context, h host, program string) string {
	out, err := e.run(ctx, loginShell(e.cfg, h.Destination, "command -v "+shellWord(program)))
	if err != nil {
		return ""
	}
	// A greeting the login files print comes first; the path is the last
	// line.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	path := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r") {
		return ""
	}
	return path
}
