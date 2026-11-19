package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

const spawnScreen = "machine_spawn"
const remoteTerminal = "terminal"

type spawnOptions struct {
	tool, model, name, prompt, group, directory string
}

func spawnArgv(cfg config, h host, opts spawnOptions) []string {
	args := []string{"spawn", "--nest=false", "--tool", opts.tool}
	if opts.tool == remoteTerminal {
		args = []string{"machines", "terminal", "--tmux", h.Tmux}
	}
	for _, option := range [][2]string{
		{"--model", opts.model},
		{"--name", opts.name},
		{"--prompt", opts.prompt},
		{"--group", opts.group},
	} {
		if option[1] != "" {
			args = append(args, option[0], option[1])
		}
	}
	command := machineCommand(h, h.GateInbox, args...)
	if opts.directory != "" {
		command += " --directory " + shellPath(opts.directory)
	}
	return loginShell(cfg, h.Destination, command+" --json")
}

func availableTools(ctx context.Context, h extension.Host) ([]string, error) {
	tools, err := h.Tools(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	var shell bool
	for _, tool := range tools {
		if tool.Name == toolName || tool.Name == "machines" {
			continue
		}
		if tool.Shell {
			shell = true
			continue
		}
		if tool.Name == remoteTerminal {
			continue
		}
		names = append(names, tool.Name)
	}
	if shell {
		names = append(names, remoteTerminal)
	}
	return names, nil
}

func (e *Extension) machineUI() extension.UI {
	keys := []extension.KeyBinding{
		{Screen: extension.ScreenList, Action: "new_on_machine", Label: "new session on another machine", Keys: []string{"N", "shift+n"},
			Run: func(ctx context.Context, _ extension.Press) error {
				if len(e.machines().active()) == 0 {
					// With nowhere to start a session, N is the way to add a
					// machine.
					e.openMachines(ctx)
					return nil
				}
				e.openSpawn(ctx, "")
				return nil
			}},
		{Screen: extension.ScreenList, Action: "machines", Label: "add and manage your other machines", Keys: []string{"O", "shift+o"},
			Run: func(ctx context.Context, _ extension.Press) error {
				e.openMachines(ctx)
				return nil
			}},
		{Screen: spawnScreen, Action: extension.ActionSubmit, Keys: []string{"enter", "ctrl+s"}, Label: "create"},
		{Screen: spawnScreen, Action: extension.ActionClose, Keys: []string{"esc"}, Label: "cancel"},
	}
	keys = append(keys, dismissKeys(e)...)
	return extension.UI{Keys: append(keys, machinesKeys()...)}
}

// openSpawn shows the new-session form, on machine when one is named.
func (e *Extension) openSpawn(ctx context.Context, machine string) {
	var names []string
	for _, h := range e.machines().active() {
		names = append(names, h.Name)
	}
	f := &spawnForm{e: e, ctx: ctx, names: names}
	if machine == "" && len(names) == 1 {
		machine = names[0]
	}
	f.mu.Lock()
	if h, ok := e.machines().host(machine); ok && !h.Paused {
		f.check(h)
	}
	f.handle = e.ui.Open(spawnScreen, f)
	f.mu.Unlock()
}

type spawnForm struct {
	e      *Extension
	ctx    context.Context
	names  []string
	handle extension.ViewHandle

	mu          sync.Mutex
	machine     string
	tools       []string
	loading     bool
	err         error
	cancel      context.CancelFunc
	starting    bool
	launchError string
	closed      bool
}

func (f *spawnForm) Title() string { return "New Session" }
func (f *spawnForm) Render(width, _ int) []extension.Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	var text string
	switch {
	case f.machine == "":
		text = "Select a machine, then press Enter to load its tools."
	case f.loading:
		text = "Checking installed CLIs on " + f.machine + "…"
	case f.starting:
		text = "Starting on " + f.machine + "…"
	case f.launchError != "":
		return wrapLines([]extension.Line{{{Text: f.launchError, Tone: extension.ToneBad}}}, width)
	case f.err != nil:
		text = "Could not check installed CLIs. Enter a CLI name or choose another machine."
	case len(f.tools) == 0:
		text = "No agent CLI is installed on " + f.machine + ". Install one there and reopen this form."
	default:
		return nil
	}
	return wrapLines([]extension.Line{{{Text: text}}}, width)
}
func (f *spawnForm) Fields() []extension.Field {
	f.mu.Lock()
	defer f.mu.Unlock()
	fields := []extension.Field{{ID: "machine", Label: "machine", Choices: f.names, Value: f.machine}}
	if f.machine == "" || (len(f.tools) == 0 && f.err == nil) {
		return fields
	}
	choices := f.tools
	if f.loading {
		choices = nil
	}
	return append(fields,
		extension.Field{ID: "cli", Label: "tool", Choices: choices, Placeholder: "CLI name"},
		extension.Field{ID: "model", Label: "model", Placeholder: "default"},
		extension.Field{ID: "name", Label: "name", Placeholder: "optional: the agent names it"},
		extension.Field{ID: "task", Label: "prompt", Placeholder: "first task (optional)"},
		extension.Field{ID: "group", Label: "group", Placeholder: "remote root"},
		extension.Field{ID: "directory", Label: "dir", Placeholder: "remote default"})
}
func (f *spawnForm) Key(k extension.ViewKey) bool {
	if k.Action != extension.ActionSubmit {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starting {
		return false
	}
	name := k.Values["machine"]
	h, ok := f.e.machines().host(name)
	if !ok || h.Paused {
		f.launchError = "Choose a machine that is not paused."
		return false
	}
	if f.machine != name {
		f.check(h)
		return false
	}
	if f.loading || (len(f.tools) == 0 && f.err == nil) {
		return false
	}
	opts := spawnOptions{
		tool: strings.TrimSpace(k.Values["cli"]), model: k.Values["model"], name: k.Values["name"],
		prompt: k.Values["task"], group: k.Values["group"], directory: k.Values["directory"],
	}
	if opts.tool == "" || opts.tool == toolName || opts.tool == "machines" || (f.err == nil && !slices.Contains(f.tools, opts.tool)) {
		f.launchError = "Choose an installed CLI."
		return false
	}
	if opts.tool == remoteTerminal {
		if f.err != nil {
			f.launchError = "Could not confirm Terminal support. Run Check on that machine."
			return false
		}
		if opts.model != "" || opts.name != "" || opts.prompt != "" || opts.group != "" {
			f.launchError = "Terminal uses dir. Clear model, name, prompt, and group to create it."
			return false
		}
	}
	f.starting, f.launchError = true, ""
	go f.start(h, opts)
	return false
}

func (f *spawnForm) start(h host, opts spawnOptions) {
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	out, err := f.e.run(ctx, spawnArgv(f.e.cfg, h, opts))
	id, replyErr := parseSpawnReply(out)
	var failure string
	var exit exitCoder
	switch {
	case ctx.Err() != nil || (errors.As(err, &exit) && exit.ExitCode() == sshFailed):
		failure = "Could not confirm launch on " + h.Name + ". Check that machine before retrying."
	case err != nil:
		failure = "Could not start on " + h.Name + ": " + lastLine(err.Error())
	case replyErr != nil:
		failure = "Could not confirm launch on " + h.Name + ". Check that machine before retrying."
	}
	f.mu.Lock()
	f.starting, f.launchError = failure == "", failure
	closed := f.closed
	f.mu.Unlock()
	if failure == "" {
		f.e.ui.Notify("Started on " + h.Name + " · waiting for mirror")
		f.e.status.trackLaunch(mirrorKey(h.Name, id))
		if f.handle != nil {
			f.handle.Close()
		}
	} else if closed {
		f.e.ui.Notify(failure)
	} else if f.handle != nil {
		f.handle.Refresh()
	}
	f.e.wakeLoop(h.Name)
}

// check runs the probe in the same login shell as a spawn. A new machine
// selection cancels the old probe so it cannot replace the current picker.
func (f *spawnForm) check(h host) {
	if f.cancel != nil {
		f.cancel()
	}
	ctx, cancel := context.WithCancel(f.ctx)
	f.cancel = cancel
	f.machine, f.loading, f.launchError = h.Name, true, ""
	go func() {
		probe, stop := context.WithTimeout(ctx, 15*time.Second)
		defer stop()
		tools, err := f.e.installedOnMachine(probe, h)
		f.mu.Lock()
		if ctx.Err() != nil {
			f.mu.Unlock()
			return
		}
		f.tools, f.err, f.loading = tools, err, false
		handle := f.handle
		f.mu.Unlock()
		if handle != nil {
			handle.Refresh()
		}
	}()
}

func (f *spawnForm) Closed(extension.CloseReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.cancel != nil {
		f.cancel()
	}
}

// Login files sometimes print a greeting. Accept a JSON reply after it,
// but require a session id so a successful shell alone is not a spawn.
func parseSpawnReply(out []byte) (string, error) {
	for offset := 0; offset < len(out); {
		var reply struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(bytes.NewReader(out[offset:])).Decode(&reply) == nil && sessionIDPattern.MatchString(reply.ID) {
			return reply.ID, nil
		}
		i := bytes.IndexByte(out[offset:], '\n')
		if i < 0 {
			break
		}
		offset += i + 1
	}
	return "", fmt.Errorf("the machine's spawn reply contains no session id")
}
