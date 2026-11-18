package machines

import (
	"slices"
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
	e, err := configure(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.cfg.Poll != defaultPoll || !slices.Equal(e.cfg.SSH, []string{"ssh"}) || e.cfg.ControlPath != defaultControlPath() {
		t.Errorf("cfg = %+v", e.cfg)
	}
	if hosts := e.machines().all(); len(hosts) != 0 {
		t.Errorf("hosts = %v, want none", hosts)
	}
}

func TestConfigureRefuses(t *testing.T) {
	for name, section := range map[string]map[string]any{
		"misspelt setting":  {"pol": "3s"},
		"poll too short":    {"poll": "100ms"},
		"empty ssh command": {"ssh": []any{""}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := configure(t, section); err == nil {
				t.Error("configured, want an error")
			}
		})
	}
}

func TestConfigureNamesOldMachineTables(t *testing.T) {
	for _, tc := range []struct {
		name, destination string
		table             map[string]any
	}{
		{"mini", "me@mini.example", map[string]any{"destination": "me@mini.example"}},
		{"box", "box", map[string]any{}},
		{"ssh", "mini", map[string]any{"destination": "mini"}},
	} {
		_, err := configure(t, map[string]any{tc.name: tc.table})
		if err == nil || !strings.Contains(err.Error(), "[extensions.machines."+tc.name+"]") ||
			!strings.Contains(err.Error(), "gate-inbox machines add "+tc.destination) {
			t.Fatal(err)
		}
	}
}

func TestConfigureReadsSettings(t *testing.T) {
	e, err := configure(t, map[string]any{
		"poll":         "5s",
		"ssh":          []any{"ssh", "-F", "/dev/null"},
		"control_path": "none",
		"discover":     []any{"cat", "/tmp/hosts"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.cfg.Poll != 5*time.Second || e.cfg.ControlPath != "none" ||
		!slices.Equal(e.cfg.SSH, []string{"ssh", "-F", "/dev/null"}) ||
		!slices.Equal(e.cfg.Discover, []string{"cat", "/tmp/hosts"}) {
		t.Errorf("cfg = %+v", e.cfg)
	}
}

func TestResolveHostRefuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings hostSettings
	}{
		{"mini.local", hostSettings{}},
		{"mini", hostSettings{Destination: "-oProxyCommand=x"}},
		{"mini", hostSettings{GateInbox: "a\nb"}},
	} {
		if _, err := resolveHost(tc.name, tc.settings); err == nil {
			t.Errorf("resolved %s with %+v, want an error", tc.name, tc.settings)
		}
	}
}
