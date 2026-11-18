package machines

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// about is what a machine says about itself in every sessions reply, so
// the board that reads it needs nothing but an ssh destination: where this
// build and tmux are on that machine, which tmux server its board keeps
// its sessions on, and which build it runs.
//
// Each field comes from the far side, so the near side checks it before
// using it (see clean) and keeps it only where the operator set nothing
// (see host.withAbout).
type about struct {
	Hostname  string `json:"hostname,omitempty"`
	OS        string `json:"os,omitempty"`
	GateInbox string `json:"gate_inbox,omitempty"`
	Tmux      string `json:"tmux,omitempty"`
	// TmuxSocket is the board's own tmux_socket, "" for tmux's default
	// server. Nil is a machine that could not tell, such as one whose
	// config.toml did not parse.
	TmuxSocket *string `json:"tmux_socket,omitempty"`
	Version    string  `json:"version,omitempty"`
}

// describeSelf is about for this machine, as `machines sessions` reports
// it. tmux is the tmux the request named, found on the PATH the command
// was given.
func describeSelf(configDir, tmux string) about {
	a := about{OS: runtime.GOOS, Version: Version}
	if name, err := os.Hostname(); err == nil {
		a.Hostname = name
	}
	if path, err := os.Executable(); err == nil {
		a.GateInbox = path
	}
	if path, err := exec.LookPath(tmux); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			a.Tmux = abs
		}
	}
	if socket, ok := boardSocket(configDir); ok {
		a.TmuxSocket = &socket
	}
	return a
}

// tmuxToUse is the tmux a sessions request runs: the one it named, unless
// that is a full path with nothing there any more, such as the path this
// machine reported before tmux was reinstalled somewhere else. Then it is
// tmux on PATH, which the reply reports, so the asking board learns the new
// path instead of reading this machine as unreachable.
func tmuxToUse(requested string) string {
	if strings.HasPrefix(requested, "/") {
		if _, err := os.Stat(requested); err != nil {
			return "tmux"
		}
	}
	return requested
}

// socketEnv is the variable the board reads when its config sets no
// tmux_socket.
const socketEnv = "GATE_INBOX_TMUX_SOCKET"

// boardSocket is the tmux server this machine's board runs its sessions
// on, read the way the board reads it: tmux_socket in its config.toml,
// then GATE_INBOX_TMUX_SOCKET, then tmux's default server. The extension
// API does not hand an extension the board's settings, so this reads that
// one key from the file itself.
func boardSocket(configDir string) (string, bool) {
	var file struct {
		TmuxSocket string `toml:"tmux_socket"`
	}
	data, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil && !os.IsNotExist(err) {
		return "", false
	}
	if err == nil {
		if _, err := toml.Decode(string(data), &file); err != nil {
			return "", false
		}
	}
	socket := strings.TrimSpace(file.TmuxSocket)
	if socket == "" {
		socket = strings.TrimSpace(os.Getenv(socketEnv))
	}
	if socket != "" && !socketNamePattern.MatchString(socket) {
		return "", false
	}
	return socket, true
}

var (
	osPattern      = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
	versionPattern = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,64}$`)
	// A hostname is shown on the board and turned into a name, never run.
	hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)
)

// clean is a with every field it can't use dropped. The paths go into
// commands run on that machine, quoted, so they are held to absolute paths
// with no line breaks; the socket goes into tmux's -L.
func (a about) clean() about {
	keepPath := func(p string) string {
		if !strings.HasPrefix(p, "/") || len(p) > 4096 || strings.ContainsAny(p, "\x00\n\r") {
			return ""
		}
		return p
	}
	a.GateInbox = keepPath(a.GateInbox)
	a.Tmux = keepPath(a.Tmux)
	if a.TmuxSocket != nil && *a.TmuxSocket != "" && !socketNamePattern.MatchString(*a.TmuxSocket) {
		a.TmuxSocket = nil
	}
	if !osPattern.MatchString(a.OS) {
		a.OS = ""
	}
	if !versionPattern.MatchString(a.Version) {
		a.Version = ""
	}
	if !hostnamePattern.MatchString(a.Hostname) {
		a.Hostname = ""
	}
	return a
}

func (a about) equal(b about) bool {
	socket := func(s *string) string {
		if s == nil {
			return "\x00"
		}
		return *s
	}
	return a.Hostname == b.Hostname && a.OS == b.OS && a.GateInbox == b.GateInbox &&
		a.Tmux == b.Tmux && socket(a.TmuxSocket) == socket(b.TmuxSocket) && a.Version == b.Version
}

// withAbout is h with what the machine said about itself, wherever the
// operator's own settings say nothing.
func (h host) withAbout(a about) host {
	h.About = &a
	if h.Settings.GateInbox == "" && a.GateInbox != "" {
		h.GateInbox = a.GateInbox
	}
	if a.Tmux != "" {
		h.Tmux = a.Tmux
	}
	if a.TmuxSocket != nil {
		h.TmuxSocket = *a.TmuxSocket
	}
	return h
}

// suggestName is a machine name for a new machine: its hostname's first
// label, or failing that its destination's, made to fit hostNamePattern.
// taken says which names are in use already.
func suggestName(hostname, destination string, taken func(string) bool) string {
	candidates := []string{hostname}
	dest := destination
	if _, after, ok := strings.Cut(dest, "@"); ok {
		dest = after
	}
	candidates = append(candidates, dest)
	for _, c := range candidates {
		label, _, _ := strings.Cut(strings.ToLower(c), ".")
		var b strings.Builder
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
				b.WriteRune(r)
			default:
				b.WriteRune('-')
			}
		}
		name := strings.Trim(b.String(), "-")
		if name == "" {
			continue
		}
		if name[0] < 'a' || name[0] > 'z' {
			name = "m-" + name
		}
		if len(name) > 40 {
			name = strings.TrimRight(name[:40], "-")
		}
		if checkName(name) != nil {
			continue
		}
		unique := name
		for i := 2; taken(unique); i++ {
			unique = name + "-" + strconv.Itoa(i)
		}
		return unique
	}
	unique := "machine"
	for i := 2; taken(unique); i++ {
		unique = "machine-" + strconv.Itoa(i)
	}
	return unique
}
