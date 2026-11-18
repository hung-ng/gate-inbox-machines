package machines

import (
	"context"
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
	return sshArgv(cfg, false, h.Destination, machineCommand(h, h.GateInbox, args...))
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
	return extension.UI{Keys: []extension.KeyBinding{
		{Screen: extension.ScreenList, Action: "new_on_machine", Label: "new session on another machine", Keys: []string{"N"},
			Run: func(ctx context.Context, _ extension.Press) error {
				if len(e.cfg.Hosts) == 0 {
					return fmt.Errorf("no machines configured")
				}
				names := make([]string, len(e.cfg.Hosts))
				for i, h := range e.cfg.Hosts {
					names[i] = h.Name
				}
				known := e.tools.copy()
				f := &spawnForm{e: e, ctx: ctx, names: names, known: known}
				if len(names) == 1 {
					f.machine = names[0]
					f.tools = known[names[0]]
				}
				e.ui.Open(spawnScreen, f)
				return nil
			}},
		{Screen: spawnScreen, Action: extension.ActionClose, Keys: []string{"esc"}, Label: "close"},
	}}
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
	h, ok := f.e.cfg.host(name)
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
		_, err := f.e.run(ctx, spawnArgv(f.e.cfg, h, cli, task, directory))
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
