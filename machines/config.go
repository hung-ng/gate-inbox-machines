package machines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

// settings is the [extensions.machines] section as the operator writes it:
// a few settings of its own, and a table per machine.
//
//	[extensions.machines]
//	poll = "3s"
//
//	[extensions.machines.mini]
//	destination = "hung-mini"
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
	// Hosts are the machines whose boards this one shows, keyed by the name
	// the board puts on their rows: every table in the section.
	Hosts map[string]hostSettings `toml:"-"`
}

// settingKeys are the section's own keys. Every other key is a machine's
// table, so these are the only names a machine cannot have.
var settingKeys = []string{"poll", "ssh", "control_path", "discover"}

// decodeSettings splits the section into its own keys and its machines'
// tables, and decodes each through extension.Config, so a misspelt key is
// an error wherever it is written.
func decodeSettings(c extension.Config) (settings, error) {
	var raw map[string]rawValue
	if err := c.Decode(&raw); err != nil {
		return settings{}, err
	}
	s := settings{Hosts: map[string]hostSettings{}}
	own := map[string]any{}
	for key, value := range raw {
		if slices.Contains(settingKeys, key) {
			own[key] = value.v
			continue
		}
		table, ok := value.v.(map[string]any)
		if !ok {
			return settings{}, fmt.Errorf("unknown key %s: the section's own keys are %s, and every other key is a machine's [extensions.machines.<name>] table",
				key, strings.Join(settingKeys, ", "))
		}
		var h hostSettings
		if err := extension.NewConfig(table).Decode(&h); err != nil {
			return settings{}, fmt.Errorf("%s: %w", key, err)
		}
		s.Hosts[key] = h
	}
	if err := extension.NewConfig(own).Decode(&s); err != nil {
		return settings{}, err
	}
	return s, nil
}

// rawValue holds one key's value as the TOML decoder read it. Decoding into
// map[string]any instead would leave every key inside a machine's table
// reported as unknown, since the strict decode counts only the keys a
// typed destination claimed.
type rawValue struct{ v any }

func (r *rawValue) UnmarshalTOML(data any) error {
	r.v = data
	return nil
}

// hostSettings is one [extensions.machines.<name>] table, or one machine
// added with `machines add`, which keeps the same keys in machines.json.
type hostSettings struct {
	// Destination is what ssh connects to: a name from ~/.ssh/config, or
	// user@host. Empty uses the table's own name.
	Destination string `toml:"destination" json:"destination"`
	// GateInbox is this build's binary on that machine. The machine says
	// where its binary is in every sessions reply, so this is only needed
	// when the first poll can't find it on any PATH it tries.
	GateInbox string `toml:"gate_inbox" json:"gate_inbox,omitempty"`
	// Home is GATE_INBOX_HOME for the board on that machine, when it runs
	// on a home of its own.
	Home string `toml:"home" json:"home,omitempty"`
	// Tmux is tmux on that machine, by the same rule as GateInbox.
	Tmux string `toml:"tmux" json:"tmux,omitempty"`
	// TmuxSocket is that board's own tmux_socket setting. The machine
	// reports it, so this overrides what it says.
	TmuxSocket string `toml:"tmux_socket" json:"tmux_socket,omitempty"`
	// Group is the group this board files the machine's sessions under,
	// one that already exists on this board: an extension cannot create a
	// group. Empty files them at the top level, where each row's badge
	// names its machine.
	Group string `toml:"group" json:"group,omitempty"`
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
	// Source is where the machine was written: config.toml, or added
	// with `machines add` or the machines screen.
	Source string
	// Paused is a machine the operator paused: it is not polled, and its
	// mirrors are put away until it is resumed.
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
	Hosts       []host
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
	names := make([]string, 0, len(s.Hosts))
	for name := range s.Hosts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		h, err := resolveHost(name, s.Hosts[name])
		if err != nil {
			return config{}, fmt.Errorf("%s: %w", name, err)
		}
		h.Source = sourceConfig
		cfg.Hosts = append(cfg.Hosts, h)
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
		Tmux:        s.Tmux,
		TmuxSocket:  s.TmuxSocket,
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
	if h.Tmux == "" {
		h.Tmux = "tmux"
	}
	if h.TmuxSocket != "" && !socketNamePattern.MatchString(h.TmuxSocket) {
		return host{}, fmt.Errorf("tmux_socket %q is not a socket name", h.TmuxSocket)
	}
	for key, value := range map[string]string{
		"destination": h.Destination, "gate_inbox": h.GateInbox, "home": h.Home,
		"tmux": h.Tmux, "group": h.Group,
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
	if slices.Contains(settingKeys, name) {
		return fmt.Errorf("%q is one of the section's own keys, so no machine can have it as a name", name)
	}
	return nil
}

func (c config) host(name string) (host, bool) {
	for _, h := range c.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return host{}, false
}

// defaultControlPath keeps ssh's shared connections under /tmp rather than
// the config directory: a Unix socket path is limited to about 100 bytes on
// macOS, and "~/Library/Application Support/gate-inbox/..." plus the hash
// ssh names each connection by does not fit.
func defaultControlPath() string {
	return filepath.Join("/tmp", fmt.Sprintf("gate-inbox-machines-%d", os.Getuid()), "%C")
}
