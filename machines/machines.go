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
// machine's conversation and current dialog, and focusing it types into
// the agent there.
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
	"sync"

	"github.com/usestring/gate-inbox/extension"
	"github.com/usestring/gate-inbox/extension/cmdline"
)

const (
	extensionID = "machines"
	// toolName is the [tools.<name>] block mirrors launch with; ConfigDefaults
	// supplies it.
	toolName = "mirror"
	// Version is this extension's own version, in the board's log.
	Version = "0.2.0"
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
const ConfigDefaults = `[tools.mirror]
command = "sh -c 'exec \"${GATE_INBOX_BIN:-gate-inbox}\" machines attach \"$@\"' --"
default_status = "idle"
mcp = "none"
skip_rename_directive = true
`

// Extension is the machines extension. The zero value is not ready; use New.
type Extension struct {
	cfg      config
	conf     extension.Config
	reg      *registry
	ui       extension.UIHost
	boardMu  sync.Mutex
	board    boardOps
	run      runner
	attach   attachRunner
	wake     chan string
	tools    toolCache
	previews previewRoutes
	status   statusBoard
}

// New returns the extension, to list in app.Options.Extensions.
func New() *Extension {
	return &Extension{run: execRunner, attach: execAttach, wake: make(chan string, 1)}
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
	// A host with no data dir still gets config.toml's machines; it just
	// has nowhere to keep added ones.
	dir, _ := c.DataDir()
	e.reg = newRegistry(cfg.Hosts, dir)
	return nil
}

// machines is every machine this build knows: config.toml's, the ones
// added from the board or the CLI, and what each said about itself.
func (e *Extension) machines() *registry {
	if e.reg != nil {
		return e.reg
	}
	// Built without Configure, as in a test that set cfg itself.
	return newRegistry(e.cfg.Hosts, "")
}

// Roles makes a mirror a row of its own rather than somebody's helper: it
// is kept in view and queued like a top-level session, its status is the
// one pinned from its machine, and nothing it does is relayed to anyone.
//
// A setup row is a pane the machines screen opens to finish adding a
// machine that needs the operator at a prompt: a password for
// ssh-copy-id, or a host key to accept. Its status is pinned to waiting,
// so triage hands it over, and it is archived once its pane ends.
func (e *Extension) Roles() []extension.RoleSpec {
	return []extension.RoleSpec{{
		Name:             roleName,
		OnScreen:         true,
		Silent:           true,
		SkipSendChildren: true,
		PinnedStatus:     true,
	}, {
		Name:             setupRoleName,
		OnScreen:         true,
		Silent:           true,
		SkipSendChildren: true,
		PinnedStatus:     true,
	}}
}

func (e *Extension) UI(host extension.UIHost) (extension.UI, error) {
	e.ui = host
	return e.machineUI(), nil
}

// StartBoard runs the mirror loop for as long as the board does. It runs
// with no machines too, so one added from the machines screen or a shell
// shows up without a restart.
func (e *Extension) StartBoard(ctx context.Context, board extension.BoardHost) (func(), error) {
	dataDir, err := e.conf.DataDir()
	if err != nil {
		return nil, err
	}
	if err := ensureControlDir(e.cfg.ControlPath); err != nil {
		return nil, err
	}
	e.boardMu.Lock()
	e.board = board
	e.boardMu.Unlock()
	m := newMirrors(e.cfg, board, e.ui, board.Logger(), e.run, dataDir)
	m.reg = e.machines()
	m.wake = e.wake
	m.tools = &e.tools
	m.previews = &e.previews
	m.status = &e.status
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.loop(ctx)
	}()
	return func() { cancel(); <-done }, nil
}

const (
	usageSessions   = "machines sessions [--tmux <path>] [--tmux-socket <name>] [--json]"
	usageAttach     = "machines attach <machine> <session-id>"
	usageCheck      = "machines check [<machine>...]"
	usagePreview    = "machines preview <session-id>"
	usageAdd        = "machines add [--name <name>] [--group <group>] [--home <dir>] [--gate-inbox <path>] [--force] <destination>"
	usageRemove     = "machines remove <machine>"
	usageRename     = "machines rename <machine> <new-name>"
	usagePause      = "machines pause <machine>"
	usageResume     = "machines resume <machine>"
	usageList       = "machines list [--json]"
	usageStartBoard = "machines start-board <machine>"
)

func (e *Extension) Commands() []extension.Command {
	return []extension.Command{{
		Name:  "machines",
		Usage: "machines <add|list|remove|rename|pause|resume|check|start-board|sessions|attach|preview>",
		About: "add and manage the other machines whose agents this board shows, and answer their boards",
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
		{
			Name: "check", Usage: usageCheck,
			About: "check that each configured machine can be polled and attached to, and say what to fix",
			Run:   func(args []string) error { return e.runCheck(ctx, args, os.Stdout) },
		},
		{
			Name: "add", Usage: usageAdd,
			About: "check a machine by its ssh destination and add it, offering to fix its ssh setup at a terminal",
			Run:   func(args []string) error { return e.runAdd(ctx, args) },
		},
		{
			Name: "remove", Usage: usageRemove,
			About: "remove a machine added with add; its mirrors are put away",
			Run:   func(args []string) error { return e.runRemove(args) },
		},
		{
			Name: "rename", Usage: usageRename,
			About: "rename a machine added with add; its mirrors come back under the new name",
			Run:   func(args []string) error { return e.runRename(args) },
		},
		{
			Name: "pause", Usage: usagePause,
			About: "stop polling a machine and put its mirrors away until it is resumed",
			Run:   func(args []string) error { return e.runPause(args, true) },
		},
		{
			Name: "resume", Usage: usageResume,
			About: "poll a paused machine again",
			Run:   func(args []string) error { return e.runPause(args, false) },
		},
		{
			Name: "list", Usage: usageList,
			About: "list every machine, where it was written, and what it last said about itself",
			Run:   func(args []string) error { return e.runList(args) },
		},
		{
			Name: "start-board", Usage: usageStartBoard,
			About: "start a machine's board in a detached tmux session there, through your login shell",
			Run:   func(args []string) error { return e.runStartBoard(ctx, args) },
		},
		{
			Name: "preview", Usage: usagePreview,
			About: "read a session's conversation and live dialog for a mirror",
			Run: func(args []string) error {
				set := cmdline.NewFlagSet(usagePreview)
				operands, err := cmdline.Parse(os.Stdout, program, set, args, 1, 1)
				if err != nil {
					return err
				}
				return writePreview(ctx, operands[0], host, os.Stdout)
			},
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
	*tmux = tmuxToUse(*tmux)
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
	snap.BoardRunning = boolPtr(boardRunning(host.ConfigDir()))
	self := describeSelf(host.ConfigDir(), *tmux)
	snap.About = &self
	scrolling, err := tmuxScrolling(ctx, e.run, tmuxArgv)
	if err != nil {
		return err
	}
	for i := range snap.Sessions {
		snap.Sessions[i].Scrolling = scrolling[snap.Sessions[i].ID]
	}
	snap.Tools, err = availableTools(ctx, host)
	if err != nil {
		return err
	}
	return cmdline.Emit(os.Stdout, *asJSON, snap, formatSnapshot(snap))
}

func (e *Extension) runAttach(ctx context.Context, args []string, host extension.Host) error {
	// The board launches the tool with its prompt ahead of these arguments,
	// and a mirror has none, so an empty word may lead them.
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "" })
	if len(args) > 0 && args[0] == setupFlag {
		// A setup row's pane: `machines add` at a terminal, for a machine
		// that needs a password or a host key typed.
		caller := os.Getenv("GATE_INBOX_SESSION_ID")
		if host != nil && host.Caller() != "" {
			caller = host.Caller()
		}
		return e.runSetupPane(ctx, args[1:], caller)
	}
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
	h, ok := e.machines().host(hostName)
	if !ok {
		return fmt.Errorf("no machine %q: add it with `gate-inbox machines add`", hostName)
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
