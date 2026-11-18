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

// runCheck walks each configured machine, or the ones named, through what
// the board needs from it, in the order each step depends on the last. Setup
// was the hard part of every first run: a key ssh can't use without asking,
// a binary or tmux off the PATH an ssh command gets, socket names that don't
// match. Each failure says what to change.
//
// It builds its commands with the board's own argv builders, so a machine
// that passes is one the board can poll and attach to.
func (e *Extension) runCheck(ctx context.Context, names []string, out io.Writer) error {
	hosts := e.cfg.Hosts
	if len(names) > 0 {
		hosts = nil
		for _, name := range names {
			h, ok := e.cfg.host(name)
			if !ok {
				return fmt.Errorf("no machine %q: add an [extensions.machines.%s] table", name, name)
			}
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return errors.New("no machines configured: add an [extensions.machines.<name>] table for each one")
	}
	if err := ensureControlDir(e.cfg.ControlPath); err != nil {
		return err
	}
	failed := false
	for _, h := range hosts {
		if !e.checkMachine(ctx, h, out) {
			failed = true
		}
	}
	if failed {
		return errors.New("machine setup checks failed")
	}
	return nil
}

// sshHint is the fix for an ssh that could not run a command at all.
func sshHint(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "permission denied"):
		return "install your SSH key on that machine"
	case strings.Contains(msg, "host key verification failed"):
		return "ssh in once by hand to accept its host key"
	case strings.Contains(msg, "resolve"), strings.Contains(msg, "timed out"), strings.Contains(msg, "timeout"), strings.Contains(msg, "refused"):
		return "check the network, and that the machine accepts SSH logins"
	default:
		return "check the network and SSH configuration"
	}
}

// exitCoder is what an *exec.ExitError offers; asking for the interface
// lets a test runner stand in for a process.
type exitCoder interface{ ExitCode() int }

// commandNotFound is the shell's exit status for a program it can't find.
const commandNotFound = 127

func (e *Extension) checkMachine(ctx context.Context, h host, out io.Writer) bool {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	say := func(check string, err error, fix string) bool {
		if err != nil {
			fmt.Fprintf(out, "%s: %s FAIL (%v); %s\n", h.Name, check, err, fix)
			return false
		}
		fmt.Fprintf(out, "%s: %s OK\n", h.Name, check)
		return true
	}
	run := func(program string, args ...string) ([]byte, error) {
		return e.run(ctx, sshArgv(e.cfg, false, h.Destination, machineCommand(h, program, args...)))
	}
	if _, err := run("true"); err != nil {
		return say("ssh", err, sshHint(err))
	}
	say("ssh", nil, "")
	if _, err := run(h.GateInbox, "--version"); err != nil {
		fix := "check that gate-inbox runs on that machine"
		var exit exitCoder
		if errors.As(err, &exit) && exit.ExitCode() == commandNotFound {
			fix = "set gate_inbox to its full path"
		}
		return say("gate-inbox", err, fix)
	}
	say("gate-inbox", nil, "")
	if _, err := run(h.GateInbox, "machines", "--help"); err != nil {
		return say("machines command", err, "install this build there; that machine's board can stay stock")
	}
	say("machines command", nil, "")
	if _, err := run(h.Tmux, "-V"); err != nil {
		return say("tmux", err, "set tmux to its full path")
	}
	say("tmux", nil, "")
	data, err := e.run(ctx, listArgv(e.cfg, h))
	var snap snapshot
	if err == nil {
		snap, err = parseSnapshot(bytes.NewReader(data))
	}
	if !say("sessions reply", err, "install the same build on both machines and check its sessions reply") {
		return false
	}
	if !snap.boardRunning() {
		return say("board running", errors.New("board stopped"), "start that machine's board")
	}
	say("board running", nil, "")
	// A board whose sessions none of them can be attached to is most often
	// one on its own tmux server that this machine's config doesn't name.
	if len(snap.Sessions) > 0 {
		for _, s := range snap.Sessions {
			if s.Attachable {
				return say("attachable session", nil, "")
			}
		}
		return say("attachable session", errors.New("no attachable sessions"), "set tmux_socket to match that board's config")
	}
	return true
}
