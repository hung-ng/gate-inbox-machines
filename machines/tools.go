package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/usestring/gate-inbox/extension"
	"github.com/usestring/gate-inbox/extension/cmdline"
)

func runTools(ctx context.Context, args []string, host extension.Host) error {
	set := cmdline.NewFlagSet(usageTools)
	asJSON := cmdline.JSONFlag(set)
	if _, err := cmdline.Parse(os.Stdout, program, set, args, 0, 0); err != nil {
		return err
	}
	tools, err := installedTools(ctx, host)
	if err != nil {
		return err
	}
	return cmdline.Emit(os.Stdout, *asJSON, tools, strings.Join(tools, "\n"))
}

func installedTools(ctx context.Context, host extension.Host) ([]string, error) {
	if host == nil {
		return nil, errors.New("the board lent this command no tool service")
	}
	names, err := availableTools(ctx, host)
	if err != nil {
		return nil, err
	}
	// Host.Tools reports the effective tool names, but leaves out their
	// commands. Read the overrides so aliases and absolute paths work too.
	var config struct {
		Tools map[string]struct{ Command string }
	}
	_, err = toml.DecodeFile(filepath.Join(host.ConfigDir(), "config.toml"), &config)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ready := []string{}
	for _, name := range names {
		binary := name
		// Gate Inbox's spawn checks the first word of the command.
		if fields := strings.Fields(config.Tools[name].Command); len(fields) > 0 {
			binary = fields[0]
		}
		if _, err := exec.LookPath(binary); err == nil {
			ready = append(ready, name)
		}
	}
	return ready, nil
}

func (e *Extension) installedOnMachine(ctx context.Context, h host) ([]string, error) {
	command := machineCommand(h, h.GateInbox, "machines", "tools", "--json")
	out, err := e.run(ctx, loginShell(e.cfg, h.Destination, command))
	if err != nil {
		return nil, err
	}
	// The user's login files may print a greeting before the reply.
	for rest := out; len(rest) > 0; {
		if bytes.HasPrefix(bytes.TrimSpace(rest), []byte("[")) {
			var tools []string
			if err := json.NewDecoder(bytes.NewReader(rest)).Decode(&tools); err == nil && tools != nil {
				return tools, nil
			}
		}
		_, rest, _ = bytes.Cut(rest, []byte("\n"))
	}
	return nil, errors.New("the machine did not list its installed CLIs")
}
