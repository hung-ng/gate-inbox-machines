// Package machines is a Gate Inbox extension that puts the agent sessions
// running on your other machines on this board, beside its own.
//
// Each machine runs its own board, as it does today: that board reads its
// panes, holds its questions and types its messages. This extension asks
// each configured machine's board for its sessions over ssh, and gives
// every one a row here: a session of the extension's own whose pane runs
// `ssh -t <machine> tmux attach -t gi_<id>`, with its status pinned to the
// one that machine's board reports. The board then does the rest with what
// it already has: the row sorts into triage, its preview is the other
// machine's pane, and focusing it types into the agent there.
//
// Nothing listens on a port. Everything goes over ssh with the operator's
// own key, so this extension can reach on a machine exactly what the
// operator could reach by ssh-ing in.
package machines

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/usestring/gate-inbox/extension"
	"github.com/usestring/gate-inbox/extension/cmdline"
)

const (
	extensionID = "machines"
	// toolName is the [tools.<name>] block mirrors launch with; ConfigDefaults
	// supplies it.
	toolName = "machines"
	// Version is this extension's own version, in the board's log.
	Version = "0.1.0"
	program = "gate-inbox"
)

// ConfigDefaults is the tool block a build carrying this extension lays
// under the operator's config (app.Options.ConfigDefaults).
//
// The pane runs this build's own `machines attach`, which holds the ssh
// connection and reconnects it. It is reached through GATE_INBOX_BIN, which
// the board gives every pane it launches. The block declares no input line
// (activity_cutoff), so the board refuses to queue a message into a
// mirror: anything typed there would go straight to the agent on the
// other machine, past that machine's board's own checks. skip_rename_directive keeps the
// board's naming request out of the pane for the same reason, and mcp
// "none" keeps it from registering an MCP server for a process that is
// only ssh.
const ConfigDefaults = `[tools.machines]
command = "sh -c 'exec \"${GATE_INBOX_BIN:-gate-inbox}\" machines attach \"$@\"' --"
default_status = "idle"
mcp = "none"
skip_rename_directive = true
`

// Extension is the machines extension. The zero value is not ready; use New.
type Extension struct {
	cfg    config
	conf   extension.Config
	ui     extension.UIHost
	run    runner
	attach attachRunner
}

// New returns the extension, to list in app.Options.Extensions.
func New() *Extension {
	return &Extension{run: execRunner, attach: execAttach}
}

func (e *Extension) Descriptor() extension.Descriptor {
	return extension.Descriptor{ID: extensionID, Version: Version}
}

// Configure reads [extensions.machines]. With no machines the extension
// mirrors nothing, but still answers `machines sessions`, which is how
// another machine's board reads this one: the same build runs on every
// machine.
func (e *Extension) Configure(c extension.Config) error {
	s, err := decodeSettings(c)
	if err != nil {
		return err
	}
	cfg, err := resolve(s)
	if err != nil {
		return err
	}
	e.cfg, e.conf = cfg, c
	return nil
}

// Roles makes a mirror a row of its own rather than somebody's helper: it
// is kept in view and queued like a top-level session, its status is the
// one pinned from its machine, and nothing it does is relayed to anyone.
func (e *Extension) Roles() []extension.RoleSpec {
	return []extension.RoleSpec{{
		Name:             roleName,
		OnScreen:         true,
		Silent:           true,
		SkipSendChildren: true,
		PinnedStatus:     true,
	}}
}

func (e *Extension) UI(host extension.UIHost) (extension.UI, error) {
	e.ui = host
	return extension.UI{}, nil
}

// StartBoard runs the mirror loop for as long as the board does. It does
// nothing when no machine is configured.
func (e *Extension) StartBoard(ctx context.Context, board extension.BoardHost) (func(), error) {
	if len(e.cfg.Hosts) == 0 {
		return func() {}, nil
	}
	dataDir, err := e.conf.DataDir()
	if err != nil {
		return nil, err
	}
	if err := ensureControlDir(e.cfg.ControlPath); err != nil {
		return nil, err
	}
	m := newMirrors(e.cfg, board, e.ui, board.Logger(), e.run, dataDir)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.loop(ctx)
	}()
	return func() { cancel(); <-done }, nil
}

const (
	usageSessions = "machines sessions [--tmux <path>] [--tmux-socket <name>] [--json]"
	usageAttach   = "machines attach <machine> <session-id>"
)

func (e *Extension) Commands() []extension.Command {
	return []extension.Command{{
		Name:  "machines",
		Usage: "machines <sessions|attach>",
		About: "show this board's sessions to your other machines' boards, and attach to one of theirs",
		Run:   e.runMachines,
	}}
}

func (e *Extension) runMachines(ctx context.Context, args []string, host extension.Host) error {
	return cmdline.Dispatch(os.Stdout, program, "machines", []cmdline.Verb{
		{
			Name: "sessions", Usage: usageSessions,
			About: "list this board's agent sessions, for your other machines' boards to show",
			Run:   func(args []string) error { return e.runSessions(ctx, args, host) },
		},
		{
			Name: "attach", Usage: usageAttach,
			About: "attach this terminal to an agent session on a configured machine, reconnecting until the session ends; mirrors run it",
			Run:   func(args []string) error { return e.runAttach(ctx, args, host) },
		},
	}, args)
}

func (e *Extension) runSessions(ctx context.Context, args []string, host extension.Host) error {
	set := cmdline.NewFlagSet(usageSessions)
	tmux := set.String("tmux", "tmux", "the tmux that mirrors attach through, to tell which sessions they can reach")
	socket := set.String("tmux-socket", "", "that tmux's -L socket name, when the board sets tmux_socket")
	asJSON := cmdline.JSONFlag(set)
	if _, err := cmdline.Parse(os.Stdout, program, set, args, 0, 0); err != nil {
		return err
	}
	if host == nil || host.Sessions() == nil {
		return errNoSessions
	}
	if *socket != "" && !socketNamePattern.MatchString(*socket) {
		return fmt.Errorf("--tmux-socket %q is not a socket name", *socket)
	}
	tmuxArgv := []string{*tmux}
	if *socket != "" {
		tmuxArgv = append(tmuxArgv, "-L", *socket)
	}
	attachable, err := tmuxSessions(ctx, e.run, tmuxArgv)
	if err != nil {
		return err
	}
	snap, err := listSessions(ctx, host.Sessions(), attachable)
	if err != nil {
		return err
	}
	return cmdline.Emit(os.Stdout, *asJSON, snap, formatSnapshot(snap))
}

func (e *Extension) runAttach(ctx context.Context, args []string, host extension.Host) error {
	// The board launches the tool with its prompt ahead of these arguments,
	// and a mirror has none, so an empty word may lead them.
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "" })
	if len(args) == 0 {
		// The machines tool is in the new-session picker like any other,
		// and picking it starts this with nothing to attach to.
		return errors.New("the machines tool is how this board shows your other machines' sessions, and the board launches it for each one; start an agent CLI instead")
	}
	set := cmdline.NewFlagSet(usageAttach)
	operands, err := cmdline.Parse(os.Stdout, program, set, args, 2, 2)
	if err != nil {
		return err
	}
	hostName, id := operands[0], operands[1]
	h, ok := e.cfg.host(hostName)
	if !ok {
		return fmt.Errorf("no machine %q: add an [extensions.machines.%s] table", hostName, hostName)
	}
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("%q is not a session id", id)
	}
	if err := ensureControlDir(e.cfg.ControlPath); err != nil {
		return err
	}
	note := func(code int, msg string) {
		if host != nil && host.Logger() != nil {
			host.Logger().Warn("a mirror cannot attach", "machine", hostName, "session", id, "code", code, "err", msg)
		}
	}
	err = attachLoop(ctx, os.Stderr, e.attach, attachArgv(e.cfg, h, id), hostName, id, sleepCtx, note)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// The board finds each capability by asking for its interface, so a
// signature that drifts from the contract would switch the capability off
// without a word. These fail the build instead.
var (
	_ extension.Extension     = (*Extension)(nil)
	_ extension.RoleProvider  = (*Extension)(nil)
	_ extension.UIProvider    = (*Extension)(nil)
	_ extension.BoardProvider = (*Extension)(nil)
	_ extension.CLIProvider   = (*Extension)(nil)
)
