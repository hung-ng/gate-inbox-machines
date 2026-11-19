package machines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

// settings is the [extensions.machines] section.
type settings struct {
	// Poll is how often each machine's board is asked for its sessions.
	Poll extension.Duration `toml:"poll"`
	// SSH is the ssh command and any options of the operator's own. The
	// options this extension needs are added after them.
	SSH []string `toml:"ssh"`
	// ControlPath is where ssh keeps the one connection it shares between a
	// machine's polls and its mirrors' panes. "none" opens a connection
	// each time instead.
	ControlPath string `toml:"control_path"`
	// Discover is a command, run on this machine, that prints the
	// destinations of machines to suggest when one is added: one a line.
	// It is how a network's own peer list (a VPN, an inventory) becomes
	// suggestions without this extension knowing that network.
	Discover []string `toml:"discover"`
}

// hostSettings is one machine in machines.json.
type hostSettings struct {
	// Destination is what ssh connects to: a name from ~/.ssh/config, or
	// user@host. Empty uses the machine's name.
	Destination string `json:"destination"`
	// GateInbox is this build's binary on that machine. The machine says
	// where its binary is in every sessions reply, so this is only needed
	// when the first poll can't find it on any PATH it tries.
	GateInbox string `json:"gate_inbox,omitempty"`
	// Home is GATE_INBOX_HOME for the board on that machine, when it runs
	// on a home of its own.
	Home string `json:"home,omitempty"`
	// Group is the group this board files the machine's sessions under,
	// one that already exists on this board: an extension cannot create a
	// group. Empty files them at the top level, where each row's badge
	// names its machine.
	Group string `json:"group,omitempty"`
}

// host is one machine with its defaults filled in and checked, and with
// what the machine last said about itself (see about) where the operator
// set nothing.
type host struct {
	Name        string
	Destination string
	GateInbox   string
	Home        string
	Tmux        string
	TmuxSocket  string
	Group       string
	// Paused stops polling and keeps the machine's mirrors outside triage.
	Paused bool
	// Settings is what the operator wrote. A key set there wins over what
	// the machine reports about itself.
	Settings hostSettings
	// About is what the machine last reported about itself, once it has.
	About *about
}

// config is the whole section, resolved.
type config struct {
	Poll        time.Duration
	SSH         []string
	ControlPath string
	Discover    []string
}

const (
	defaultPoll = 3 * time.Second
	minPoll     = time.Second
)

// Machine names go into row badges, launch arguments and the state file's
// keys, so they are held to the board's own rule for an extension id.
var hostNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// tmux's -L takes a socket name, not a path.
var socketNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func resolve(s settings) (config, error) {
	cfg := config{
		Poll:        s.Poll.Duration,
		SSH:         s.SSH,
		ControlPath: s.ControlPath,
		Discover:    s.Discover,
	}
	if cfg.Poll == 0 {
		cfg.Poll = defaultPoll
	}
	if cfg.Poll < minPoll {
		return config{}, fmt.Errorf("poll is %s; it must be at least %s, since every poll is an ssh round trip per machine", cfg.Poll, minPoll)
	}
	if len(cfg.SSH) == 0 {
		cfg.SSH = []string{"ssh"}
	}
	if cfg.SSH[0] == "" {
		return config{}, errors.New("ssh names an empty command")
	}
	if cfg.ControlPath == "" {
		cfg.ControlPath = defaultControlPath()
	}
	if len(cfg.Discover) > 0 && cfg.Discover[0] == "" {
		return config{}, errors.New("discover names an empty command")
	}
	return cfg, nil
}

func resolveHost(name string, s hostSettings) (host, error) {
	if err := checkName(name); err != nil {
		return host{}, err
	}
	h := host{
		Name:        name,
		Destination: s.Destination,
		GateInbox:   s.GateInbox,
		Home:        s.Home,
		Tmux:        "tmux",
		Group:       s.Group,
		Settings:    s,
	}
	if h.Destination == "" {
		h.Destination = name
	}
	// ssh reads a destination that starts with '-' as an option.
	if strings.HasPrefix(h.Destination, "-") {
		return host{}, fmt.Errorf("destination %q starts with '-'", h.Destination)
	}
	if h.GateInbox == "" {
		h.GateInbox = "gate-inbox"
	}
	for key, value := range map[string]string{
		"destination": h.Destination, "gate_inbox": h.GateInbox, "home": h.Home,
		"group": h.Group,
	} {
		if strings.ContainsAny(value, "\x00\n\r") {
			return host{}, fmt.Errorf("%s holds a line break or NUL", key)
		}
	}
	return h, nil
}

// checkName refuses a name a machine can't have.
func checkName(name string) error {
	if !hostNamePattern.MatchString(name) {
		return errors.New("a machine's name is lower case, starts with a letter, and holds only letters, digits, '-' and '_'")
	}
	return nil
}

// defaultControlPath keeps ssh's shared connections under /tmp rather than
// the config directory: a Unix socket path is limited to about 100 bytes on
// macOS, and "~/Library/Application Support/gate-inbox/..." plus the hash
// ssh names each connection by does not fit.
func defaultControlPath() string {
	return filepath.Join("/tmp", fmt.Sprintf("gate-inbox-machines-%d", os.Getuid()), "%C")
}
