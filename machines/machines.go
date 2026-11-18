// Package machines is a Gate Inbox extension that puts the agent sessions
// running on your other machines on this board, beside its own.
//
// Each machine runs its own board, as it does today: that board reads its
// panes, holds its questions and types its messages. This extension asks
// each added machine's board for its sessions over ssh, and gives
// every one a row here: a session of the extension's own whose pane runs
// `ssh -t <machine> tmux attach -t gi_<id>`, with its status pinned to the
// one that machine's board reports. The row sorts into triage, and
// focusing it opens the remote terminal.
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
	"path/filepath"
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
	cfg     config
	conf    extension.Config
	reg     *registry
	ui      extension.UIHost
	boardMu sync.Mutex
	board   boardOps
	run     runner
	attach  attachRunner
	wake    chan string
	tools   toolCache
	status  statusBoard
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
	var s settings
	if err := c.Decode(&s); err != nil {
		// Decode fills this map even when it reports unknown keys inside
		// a table. Read it only to identify old machine tables.
		var section map[string]any
		_ = c.Decode(&section)
		for _, name := range sortedKeys(section) {
			if table, ok := section[name].(map[string]any); ok {
				destination, _ := table["destination"].(string)
				if destination == "" {
					destination = name
				}
				return fmt.Errorf("delete the [extensions.machines.%s] table, then add the machine with `gate-inbox machines add %s`", name, shellWord(destination))
			}
		}
		return err
	}
	cfg, err := resolve(s)
	if err != nil {
		return err
	}
	e.cfg, e.conf = cfg, c
	dir, _ := c.DataDir()
	e.reg = newRegistry(dir)
	return nil
}

// machines reads the registry for this extension.
func (e *Extension) machines() *registry {
	if e.reg != nil {
		return e.reg
	}
	// Built without Configure, as in a test that set cfg itself.
	return newRegistry("")
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
	usageSessions = "machines sessions [--tmux <path>] [--tmux-socket <name>] [--json]"
	usageAttach   = "machines attach [<machine> <session-id>]"
	usageCheck    = "machines check [<machine>...]"

	usageAdd    = "machines add [--name <name>] [--group <group>] [--home <dir>] [--gate-inbox <path>] [--force] <destination>"
	usageRemove = "machines remove <machine>"

	usagePause  = "machines pause <machine>"
	usageResume = "machines resume <machine>"
	usageList   = "machines list [--json]"
)

func (e *Extension) Commands() []extension.Command {
	return []extension.Command{{
		Name:  "machines",
		Usage: "machines <add|list|remove|pause|resume|check|sessions|attach>",
		About: "add and manage the other machines whose agents this board shows, and answer their boards",
		Run:   e.runMachines,
	}}
}

func (e *Extension) runMachines(ctx context.Context, args []string, host extension.Host) error {
	operatorOnly := func(verb string, run func([]string) error) func([]string) error {
		return func(args []string) error {
			if host != nil && host.Caller() != "" {
				return fmt.Errorf("run machines %s from an operator terminal outside a Gate Inbox session", verb)
			}
			return run(args)
		}
	}
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
			Run: func(args []string) error {
				names, err := cmdline.Parse(os.Stdout, program, cmdline.NewFlagSet(usageCheck), args, 0, cmdline.AnyNumber)
				if err != nil {
					return err
				}
				return e.runCheck(ctx, names, os.Stdout)
			},
		},
		{
			Name: "add", Usage: usageAdd,
			About: "check a machine by its ssh destination and add it",
			Run:   operatorOnly("add", func(args []string) error { return e.runAdd(ctx, args) }),
		},
		{
			Name: "remove", Usage: usageRemove,
			About: "remove a machine added with add; its mirrors are put away",
			Run:   operatorOnly("remove", e.runRemove),
		},
		{
			Name: "pause", Usage: usagePause,
			About: "stop polling a machine and keep its mirrors",
			Run:   operatorOnly("pause", func(args []string) error { return e.runPause(args, true) }),
		},
		{
			Name: "resume", Usage: usageResume,
			About: "poll a paused machine again",
			Run:   operatorOnly("resume", func(args []string) error { return e.runPause(args, false) }),
		},
		{
			Name: "list", Usage: usageList,
			About: "list every machine and what it last said about itself",
			Run:   func(args []string) error { return e.runList(args) },
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
	var hostName, id string
	if len(args) == 0 {
		// Stock revive and restore keep the row ID but omit launch Args.
		localID := os.Getenv("GATE_INBOX_SESSION_ID")
		if localID == "" {
			fmt.Fprintln(os.Stderr, "No Gate Inbox row ID. Use machines attach <machine> <session-id> to attach from a terminal.")
			<-ctx.Done()
			return nil
		}
		var lastError string
		for {
			target, err := e.savedMirrorTarget(localID)
			if err == nil {
				hostName, id = splitKey(target)
				break
			}
			if msg := err.Error(); msg != lastError {
				fmt.Fprintf(os.Stderr, "[gate-inbox machines] %s. Waiting for the saved mirror target.\r\n", msg)
				lastError = msg
			}
			if err := sleepCtx(ctx, firstRetry); err != nil {
				return nil
			}
		}
	} else {
		set := cmdline.NewFlagSet(usageAttach)
		operands, err := cmdline.Parse(os.Stdout, program, set, args, 2, 2)
		if err != nil {
			return err
		}
		hostName, id = operands[0], operands[1]
	}
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("%q is not a session id", id)
	}
	reg := e.machines()
	attempt := func(ctx context.Context) (int, error) {
		reg.reload()
		h, ok := reg.host(hostName)
		if !ok {
			return -1, fmt.Errorf("no machine %q: add it with `gate-inbox machines add`", hostName)
		}
		if err := ensureControlDir(e.cfg.ControlPath); err != nil {
			return -1, err
		}
		return e.attach(ctx, attachArgv(e.cfg, h, id))
	}
	note := func(code int, msg string) {
		if host != nil && host.Logger() != nil {
			host.Logger().Warn("a mirror cannot attach", "machine", hostName, "session", id, "code", code, "err", msg)
		}
	}
	err := attachLoop(ctx, os.Stderr, attempt, hostName, id, sleepCtx, note)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (e *Extension) savedMirrorTarget(localID string) (string, error) {
	dir, err := e.conf.DataDir()
	if err != nil {
		return "", err
	}
	saved, err := readState(filepath.Join(dir, stateFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("could not read %s: %w", stateFile, err)
	}
	key := saved.target(localID)
	name, id := splitKey(key)
	if name == "" || !sessionIDPattern.MatchString(id) {
		return "", fmt.Errorf("no target in %s for row %s", stateFile, localID)
	}
	reg := e.machines()
	reg.reload()
	if _, ok := reg.host(name); !ok {
		return "", fmt.Errorf("machine %q is missing from %s", name, storeFileName)
	}
	return key, nil
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
