package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	if directory != "" {
		args = append(args, "--directory", directory)
	}
	args = append(args, "--json")
	return loginShell(cfg, h.Destination, machineCommand(h, h.GateInbox, args...))
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
		{Screen: extension.ScreenList, Action: "new_on_machine", Label: "new session on another machine", Keys: []string{"N"},
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
		{Screen: extension.ScreenList, Action: "machines", Label: "add and manage your other machines", Keys: []string{"O"},
			Run: func(ctx context.Context, _ extension.Press) error {
				e.openMachines(ctx)
				return nil
			}},
		{Screen: spawnScreen, Action: extension.ActionClose, Keys: []string{"esc"}, Label: "close"},
	}
	return extension.UI{Keys: append(keys, machinesKeys()...)}
}

// openSpawn shows the new-session form, on machine when one is named.
func (e *Extension) openSpawn(ctx context.Context, machine string) {
	var names []string
	for _, h := range e.machines().active() {
		names = append(names, h.Name)
	}
	known := e.tools.copy()
	f := &spawnForm{e: e, ctx: ctx, names: names, known: known}
	if machine == "" && len(names) == 1 {
		machine = names[0]
	}
	if machine != "" {
		f.machine = machine
		f.tools = known[machine]
	}
	e.ui.Open(spawnScreen, f)
}

type spawnForm struct {
	e       *Extension
	ctx     context.Context
	names   []string
	machine string
	tools   []string
	known   map[string][]string
}

func (f *spawnForm) Title() string { return "New session on another machine" }
func (f *spawnForm) Render(int, int) []extension.Line {
	if f.machine == "" {
		return []extension.Line{{{Text: "Select a machine and press Enter to use its cached CLIs."}}}
	}
	return []extension.Line{{{Text: "Choose a CLI, enter a task and submit."}}}
}
func (f *spawnForm) Fields() []extension.Field {
	fields := []extension.Field{{ID: "machine", Label: "Machine", Choices: f.names, Value: f.machine}}
	if f.machine == "" {
		return fields
	}
	return append(fields,
		extension.Field{ID: "cli", Label: "CLI", Choices: f.tools},
		extension.Field{ID: "task", Label: "Task"},
		extension.Field{ID: "directory", Label: "Directory", Placeholder: "that board's default"})
}
func (f *spawnForm) Key(k extension.ViewKey) bool {
	if k.Action != extension.ActionSubmit {
		return false
	}
	name := k.Values["machine"]
	h, ok := f.e.machines().host(name)
	if !ok {
		return false
	}
	if f.machine != name {
		// The form API does not deliver picker changes. Submit commits the
		// machine selection before exposing that machine's CLI choices.
		f.machine = name
		f.tools = f.known[name]
		return false
	}
	cli, task, directory := k.Values["cli"], k.Values["task"], k.Values["directory"]
	if strings.TrimSpace(cli) == "" {
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
		select {
		case f.e.wake <- name:
		default:
		}
	}()
	return true
}

// Polls and key handlers run concurrently. Copy slices so each open form
// keeps its choices stable while later successful polls refresh the cache.
type toolCache struct {
	mu    sync.Mutex
	known map[string][]string
}

func (c *toolCache) set(name string, tools []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known == nil {
		c.known = map[string][]string{}
	}
	c.known[name] = append([]string(nil), tools...)
}
func (c *toolCache) copy() map[string][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := map[string][]string{}
	for name, tools := range c.known {
		result[name] = append([]string(nil), tools...)
	}
	return result
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
