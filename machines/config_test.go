package machines

import (
	"strings"
	"testing"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

func configure(t *testing.T, section map[string]any) (*Extension, error) {
	t.Helper()
	e := New()
	return e, e.Configure(extension.NewConfig(section))
}

func TestConfigureFillsDefaults(t *testing.T) {
	e, err := configure(t, map[string]any{
		"mini": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.cfg.Poll != defaultPoll {
		t.Errorf("poll = %s, want %s", e.cfg.Poll, defaultPoll)
	}
	if strings.Join(e.cfg.SSH, " ") != "ssh" {
		t.Errorf("ssh = %q", e.cfg.SSH)
	}
	h, ok := e.cfg.host("mini")
	if !ok {
		t.Fatal("host mini missing")
	}
	want := host{Name: "mini", Destination: "mini", GateInbox: "gate-inbox", Tmux: "tmux", Source: sourceConfig}
	if h != want {
		t.Errorf("host = %+v, want %+v", h, want)
	}
}

func TestConfigureWithNoSectionMirrorsNothing(t *testing.T) {
	e, err := configure(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.cfg.Hosts) != 0 {
		t.Errorf("hosts = %v, want none", e.cfg.Hosts)
	}
}

func TestConfigureRefuses(t *testing.T) {
	for name, section := range map[string]map[string]any{
		"misspelt setting":        {"pol": "3s"},
		"misspelt machine key":    {"mini": map[string]any{"destnation": "x"}},
		"machine name with a dot": {"mini.local": map[string]any{}},
		"destination as option":   {"mini": map[string]any{"destination": "-oProxyCommand=x"}},
		"socket as a path":        {"mini": map[string]any{"tmux_socket": "/tmp/x"}},
		"line break in a path":    {"mini": map[string]any{"gate_inbox": "a\nb"}},
		"poll too short":          {"poll": "100ms"},
		"empty ssh command":       {"ssh": []any{""}},
		"setting as a machine":    {"ssh": map[string]any{"destination": "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := configure(t, section); err == nil {
				t.Error("configured, want an error")
			}
		})
	}
}

func TestConfigureReadsEveryHostKey(t *testing.T) {
	e, err := configure(t, map[string]any{
		"poll":         "5s",
		"ssh":          []any{"ssh", "-F", "/dev/null"},
		"control_path": "none",
		"box": map[string]any{
			"destination": "me@box.example.ts.net",
			"gate_inbox":  "~/go/bin/gate-inbox",
			"home":        "~/.gi",
			"tmux":        "/opt/homebrew/bin/tmux",
			"tmux_socket": "agents",
			"group":       "linux box",
		},
		"mini": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.cfg.Poll != 5*time.Second || e.cfg.ControlPath != "none" || len(e.cfg.SSH) != 3 || len(e.cfg.Hosts) != 2 {
		t.Errorf("cfg = %+v", e.cfg)
	}
	h, _ := e.cfg.host("box")
	settings := hostSettings{
		Destination: "me@box.example.ts.net", GateInbox: "~/go/bin/gate-inbox",
		Home: "~/.gi", Tmux: "/opt/homebrew/bin/tmux", TmuxSocket: "agents", Group: "linux box",
	}
	want := host{
		Name: "box", Destination: "me@box.example.ts.net", GateInbox: "~/go/bin/gate-inbox",
		Home: "~/.gi", Tmux: "/opt/homebrew/bin/tmux", TmuxSocket: "agents", Group: "linux box",
		Source: sourceConfig, Settings: settings,
	}
	if h != want {
		t.Errorf("host = %+v, want %+v", h, want)
	}
}
