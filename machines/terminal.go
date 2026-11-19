package machines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/usestring/gate-inbox/extension"
	"github.com/usestring/gate-inbox/extension/cmdline"
)

const usageTerminal = "machines terminal [--directory <path>] [--tmux <path>] [--json]"
const terminalMark = "@gate-inbox-machines-terminal"

// runTerminal creates a shell on the worker's tmux server. The helper marks
// its own terminals so a poll can distinguish them from unrelated panes.
func (e *Extension) runTerminal(ctx context.Context, args []string, host extension.Host) error {
	set := cmdline.NewFlagSet(usageTerminal)
	dir := set.String("directory", ".", "working directory on this machine")
	tmux := set.String("tmux", "tmux", "tmux executable on this machine")
	asJSON := cmdline.JSONFlag(set)
	if _, err := cmdline.Parse(os.Stdout, program, set, args, 0, 0); err != nil {
		return err
	}
	if host == nil {
		return errNoSessions
	}
	socket, ok := boardSocket(host.ConfigDir())
	if !ok {
		return fmt.Errorf("could not determine the board's tmux socket")
	}
	path, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return fmt.Errorf("working directory does not exist: %s", path)
	}
	var random [16]byte
	rand.Read(random[:])
	id := hex.EncodeToString(random[:])
	name := "gi_" + id
	argv := []string{tmuxToUse(*tmux)}
	if socket != "" {
		argv = append(argv, "-L", socket)
	}
	if _, err := e.run(ctx, append(argv, "new-session", "-d", "-s", name, "-c", path,
		";", "set-option", "-t", name, terminalMark, "1")); err != nil {
		return err
	}
	session := terminalSession(id, path)
	return cmdline.Emit(os.Stdout, *asJSON, session, "created "+session.Name)
}

func terminalSession(id, directory string) machineSession {
	return machineSession{
		ID: id, Name: "terminal-" + id[:min(4, len(id))], Tool: remoteTerminal,
		Directory: directory, Status: "idle", Running: true, Attachable: true,
	}
}
