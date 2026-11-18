package machines

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// The options every ssh this extension runs carries. BatchMode refuses a
// password or host-key prompt instead of hanging a poll or a pane on one:
// the operator sets up a key once, outside the board. The keepalives notice
// a dead connection in about ten seconds rather than at TCP's own pace.
var sshOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=5",
	"-o", "ServerAliveInterval=5",
	"-o", "ServerAliveCountMax=2",
}

// controlOptions shares one connection per host between its polls and its
// mirrors' panes, so a poll every few seconds costs a round trip rather
// than a handshake, and twenty mirrors are twenty channels on one
// connection rather than twenty logins.
func controlOptions(path string) []string {
	if path == "none" {
		return []string{"-o", "ControlPath=none"}
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + path,
		"-o", "ControlPersist=10m",
	}
}

// ensureControlDir makes the directory a control path's sockets live in,
// readable only by the operator: whoever can open a control socket can run
// commands on the host as them.
func ensureControlDir(path string) error {
	if path == "none" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode() != os.ModeDir|0o700 || !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("SSH control directory %q must be a real directory owned by uid %d with mode 0700", dir, os.Getuid())
	}
	return nil
}

// listArgv is the ssh command line that asks a host's board for its
// sessions.
//
// It names the tmux and socket the host's mirrors attach through, so the
// host marks a session attachable exactly when an attach would find it.
func listArgv(cfg config, h host) []string {
	return sshArgv(cfg, false, h.Destination, listCommand(h))
}

func listCommand(h host) string {
	args := []string{"machines", "sessions", "--json", "--tmux", h.Tmux}
	if h.TmuxSocket != "" {
		args = append(args, "--tmux-socket", h.TmuxSocket)
	}
	return machineCommand(h, h.GateInbox, args...)
}

// loginShell runs command through the operator's login shell on the
// machine, for what needs the PATH and environment their login files set:
// a spawn, whose agent inherits that environment, or a first poll of a
// machine whose gate-inbox only the login shell finds. Polls skip it to
// stay fast.
func loginShell(cfg config, destination, command string) []string {
	return sshArgv(cfg, false, destination, `exec "$SHELL" -l -c `+shellWord(command))
}

// attachArgv is the ssh command line a mirror's pane runs: the session's
// own tmux session on its machine, attached through a terminal of its own.
func attachArgv(cfg config, h host, id string) []string {
	// -u: the pane is the board's own tmux pane, which is UTF-8, but the
	// host's tmux decides from LANG and LC_*, which a non-interactive ssh
	// often does not carry; without it the agent's box drawing arrives as
	// "qqqq" and its glyphs as "_".
	args := []string{h.Tmux, "-u"}
	if h.TmuxSocket != "" {
		args = append(args, "-L", h.TmuxSocket)
	}
	// "=" makes tmux match the session name exactly rather than as a
	// prefix, so gi_ab never attaches gi_abc.
	args = append(args, "attach-session", "-t", "=gi_"+id)
	command := "exec " + shellPath(args[0]) + " " + joinShell(args[1:])
	return sshArgv(cfg, true, h.Destination, command)
}

func sshArgv(cfg config, tty bool, destination, command string) []string {
	argv := append([]string{}, cfg.SSH...)
	if tty {
		argv = append(argv, "-t")
	}
	argv = append(argv, sshOptions...)
	argv = append(argv, controlOptions(cfg.ControlPath)...)
	// "--" ends ssh's options, so nothing after it is read as one.
	return append(argv, "--", destination, command)
}

// machineCommand is the command line the host's shell runs for one of this
// build's own commands, with the board's home when it has one of its own.
//
// gate-inbox looks tmux up on PATH even to list sessions, and the PATH a
// non-interactive ssh command gets is the system's alone: on a Mac,
// Homebrew's tmux is not on it. Configured executable directories go
// first; fallbacks go after PATH so the user's choices win.
func machineCommand(h host, program string, args ...string) string {
	var dirs []string
	for _, path := range []string{h.Tmux, h.GateInbox} {
		if dir, _, ok := cutLastSlash(path); ok && dir != "" {
			if dir == "~" {
				dir = "~/"
			}
			dirs = append(dirs, shellPath(dir))
		}
	}
	dirs = append(dirs, `"$PATH"`, `"$HOME"/go/bin`, `"$HOME"/.local/bin`, "/opt/homebrew/bin", "/usr/local/bin")
	env := []string{"PATH=" + strings.Join(dirs, ":")}

	if h.Home != "" {
		env = append(env, "GATE_INBOX_HOME="+shellPath(h.Home))
	}
	// During discovery, keep the remote environment until the machine
	// reports its socket. An explicitly learned empty name means default.
	if h.TmuxSocket != "" || (h.About != nil && h.About.TmuxSocket != nil) {
		env = append(env, socketEnv+"="+shellWord(h.TmuxSocket))
	}
	command := "exec env " + strings.Join(env, " ") + " " + shellPath(program)
	if len(args) > 0 {
		command += " " + joinShell(args)
	}
	return command
}

func cutLastSlash(path string) (dir, file string, ok bool) {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "", path, false
	}
	return path[:i], path[i+1:], true
}

func joinShell(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = shellWord(word)
	}
	return strings.Join(quoted, " ")
}

// plainWord is what goes over unquoted. "=" is not in it: the far side's
// shell is the operator's login shell, and zsh expands a word that starts
// with "=" into the path of a command by that name, so tmux's exact-match
// target "=gi_<id>" reached tmux as an error.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+:,./-]+$`)

// shellPath expands a leading ~/ on the machine where the path is used.
func shellPath(s string) string {
	if rest, ok := strings.CutPrefix(s, "~/"); ok {
		if rest == "" {
			return `"$HOME"/`
		}
		return `"$HOME"/` + shellWord(rest)
	}
	return shellWord(s)
}

// shellWord preserves the value through the remote login shell.
func shellWord(s string) string {
	if s != "" && plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// describeArgv is argv for a log line or an error.
func describeArgv(argv []string) string {
	return fmt.Sprintf("%q", argv)
}
