package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

const spawnScreen = "machine_spawn"

func spawnArgv(cfg config, h host, cli, task, directory string) []string {
	args := []string{"spawn", "--nest=false", "--tool", cli}
	if task != "" {
		args = append(args, "--prompt", task)
	}
	command := machineCommand(h, h.GateInbox, args...)
	if directory != "" {
		command += " --directory " + shellPath(directory)
	}
	return loginShell(cfg, h.Destination, command+" --json")
}

func availableTools(ctx context.Context, h extension.Host) ([]string, error) {
	tools, err := h.Tools(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range tools {
		if tool.Shell || tool.Name == "terminal" || tool.Name == toolName || tool.Name == "machines" {
			continue
		}
		names = append(names, tool.Name)
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
		{Screen: spawnScreen, Action: extension.ActionClose, Keys: []string{"esc"}, Label: "close"},
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

	mu      sync.Mutex
	machine string
	tools   []string
	loading bool
	err     error
	cancel  context.CancelFunc
}

func (f *spawnForm) Title() string { return "New session on another machine" }
func (f *spawnForm) Render(width, _ int) []extension.Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	var text string
	switch {
	case f.machine == "":
		text = "Select a machine and press Enter to check its installed CLIs."
	case f.loading:
		text = "Checking installed CLIs on " + f.machine + "…"
	case f.err != nil:
		text = "Could not check installed CLIs. Enter a CLI name or choose another machine."
	case len(f.tools) == 0:
		text = "No agent CLI is installed on " + f.machine + ". Install one there and reopen this form."
	default:
		text = "Choose a CLI, enter a task and submit."
	}
	return wrapLines([]extension.Line{{{Text: text}}}, width)
}
func (f *spawnForm) Fields() []extension.Field {
	f.mu.Lock()
	defer f.mu.Unlock()
	fields := []extension.Field{{ID: "machine", Label: "Machine", Choices: f.names, Value: f.machine}}
	if f.machine == "" || (len(f.tools) == 0 && f.err == nil) {
		return fields
	}
	choices := f.tools
	if f.loading {
		choices = nil
	}
	return append(fields,
		extension.Field{ID: "cli", Label: "CLI", Choices: choices, Placeholder: "CLI name"},
		extension.Field{ID: "task", Label: "Task"},
		extension.Field{ID: "directory", Label: "Directory", Placeholder: "that board's default"})
}
func (f *spawnForm) Key(k extension.ViewKey) bool {
	if k.Action != extension.ActionSubmit {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := k.Values["machine"]
	h, ok := f.e.machines().host(name)
	if !ok || h.Paused {
		return false
	}
	if f.machine != name {
		f.check(h)
		return false
	}
	if f.loading || (len(f.tools) == 0 && f.err == nil) {
		return false
	}
	cli, task, directory := k.Values["cli"], k.Values["task"], k.Values["directory"]
	if strings.TrimSpace(cli) == "" || (f.err == nil && !slices.Contains(f.tools, cli)) {
		f.e.ui.Notify("Choose an agent CLI")
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		defer cancel()
		out, err := f.e.run(ctx, spawnArgv(f.e.cfg, h, cli, task, directory))
		if err == nil {
			err = parseSpawnReply(out)
		}
		if err != nil {
			f.e.ui.Notify("Could not start session on " + name + ": " + err.Error())
		} else {
			f.e.ui.Notify("Started session on " + name)
		}
		f.e.wakeLoop(name)
	}()
	return true
}

// check runs the probe in the same login shell as a spawn. A new machine
// selection cancels the old probe so it cannot replace the current picker.
func (f *spawnForm) check(h host) {
	if f.cancel != nil {
		f.cancel()
	}
	ctx, cancel := context.WithCancel(f.ctx)
	f.cancel = cancel
	f.machine, f.loading = h.Name, true
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
	if f.cancel != nil {
		f.cancel()
	}
}

// Login files sometimes print a greeting. Accept a JSON reply after it,
// but require a session id so a successful shell alone is not a spawn.
func parseSpawnReply(out []byte) error {
	for offset := 0; offset < len(out); {
		var reply struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(bytes.NewReader(out[offset:])).Decode(&reply) == nil && sessionIDPattern.MatchString(reply.ID) {
			return nil
		}
		i := bytes.IndexByte(out[offset:], '\n')
		if i < 0 {
			break
		}
		offset += i + 1
	}
	return fmt.Errorf("the machine's spawn reply contains no session id")
}
