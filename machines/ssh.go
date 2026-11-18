package machines

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
		return nil
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
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

// listArgv is the ssh command line that asks a host's board for its
// sessions.
//
// It names the tmux and socket the host's mirrors attach through, so the
// host marks a session attachable exactly when an attach would find it.
func listArgv(cfg config, h host) []string {
	args := []string{"machines", "sessions", "--json", "--tmux", h.Tmux}
	if h.TmuxSocket != "" {
		args = append(args, "--tmux-socket", h.TmuxSocket)
	}
	command := machineCommand(h, h.GateInbox, args...)
	return sshArgv(cfg, false, h.Destination, command)
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
	command := "exec " + joinShell(args)
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
	words := append([]string{program}, args...)
	var dirs []string
	for _, path := range []string{h.Tmux, h.GateInbox} {
		if dir, _, ok := cutLastSlash(path); ok && dir != "" {
			dirs = append(dirs, shellWord(dir))
		}
	}
	dirs = append(dirs, `"$PATH"`, `"$HOME"/go/bin`, `"$HOME"/.local/bin`, "/opt/homebrew/bin", "/usr/local/bin")
	env := []string{"PATH=" + strings.Join(dirs, ":")}

	if h.Home != "" {
		env = append(env, "GATE_INBOX_HOME="+shellWord(h.Home))
	}
	if len(env) == 0 {
		return "exec " + joinShell(words)
	}
	return "exec env " + strings.Join(env, " ") + " " + joinShell(words)
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

// shellWord quotes s for the shell on the far side of ssh, which joins its
// arguments into one string for that shell to split again: sh, bash or
// zsh, whichever the operator logs in with. A leading "~/" is left for
// that shell to expand, since that is the point of writing it.
func shellWord(s string) string {
	if rest, ok := strings.CutPrefix(s, "~/"); ok {
		if rest == "" {
			return `"$HOME"/`
		}
		return `"$HOME"/` + shellWord(rest)
	}
	if s != "" && plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// describeArgv is argv for a log line or an error.
func describeArgv(argv []string) string {
	return fmt.Sprintf("%q", argv)
}
