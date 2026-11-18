package machines

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"gate-inbox":          "gate-inbox",
		"/opt/homebrew/bin":   "/opt/homebrew/bin",
		"=gi_ab12":            "'=gi_ab12'",
		"":                    "''",
		"two words":           "'two words'",
		"it's":                `'it'\''s'`,
		"$(rm -rf ~)":         "'$(rm -rf ~)'",
		"~/go/bin/gate-inbox": `"$HOME"/go/bin/gate-inbox`,
		"~/my dir/x":          `"$HOME"/'my dir/x'`,
		"~":                   "'~'",
		"a;b":                 "'a;b'",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", in, got, want)
		}
	}
}

// A quoted word must come back out of a real shell as the word that went
// in, or a path or a home with a quote in it would run something else on
// the host.
func TestShellWordRoundTripsThroughSh(t *testing.T) {
	for _, word := range []string{"plain", "two words", "it's", `a"b`, "$HOME", "`id`", "a\\b", "x'y'z", "-n"} {
		out, err := exec.Command("sh", "-c", "printf '%s' "+shellWord(word)).Output()
		if err != nil {
			t.Fatalf("sh -c for %q: %v", word, err)
		}
		if string(out) != word {
			t.Errorf("round trip of %q gave %q", word, out)
		}
	}
}

// The far side runs the operator's login shell, not sh. zsh expands a word
// that starts with "=" into a command's path, which is how the exact-match
// tmux target "=gi_<id>" first failed over a real ssh to a Mac.
func TestShellWordRoundTripsThroughLoginShells(t *testing.T) {
	words := []string{"=gi_ab12", "a=b", "it's", "two words", "$HOME", "*", "?", "[ab]", "!x", "#x", "{a,b}", "%1", "^x"}
	for _, shell := range [][]string{{"zsh", "-f", "-c"}, {"bash", "--norc", "--noprofile", "-c"}, {"sh", "-c"}} {
		if _, err := exec.LookPath(shell[0]); err != nil {
			continue
		}
		for _, word := range words {
			args := append(append([]string{}, shell[1:]...), "printf '%s' "+shellWord(word))
			out, err := exec.Command(shell[0], args...).CombinedOutput()
			if err != nil || string(out) != word {
				t.Errorf("%s: %q quoted as %s gave %q (%v)", shell[0], word, shellWord(word), out, err)
			}
		}
	}
}

func testConfig() config {
	return config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "/tmp/gir-test/%C"}
}

func TestListArgv(t *testing.T) {
	h := host{Name: "mini", Destination: "me@mini", GateInbox: "~/go/bin/gate-inbox", Tmux: "tmux", Group: "mini"}
	got := listArgv(testConfig(), h)
	want := []string{
		"ssh",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2",
		"-o", "ControlMaster=auto", "-o", "ControlPath=/tmp/gir-test/%C", "-o", "ControlPersist=10m",
		"--", "me@mini", `exec "$HOME"/go/bin/gate-inbox machines sessions --json --tmux tmux`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("listArgv =\n%q\nwant\n%q", got, want)
	}
}

func TestListArgvWithHome(t *testing.T) {
	h := host{Name: "mini", Destination: "mini", GateInbox: "gate-inbox", Tmux: "tmux", Home: "/srv/gate inbox"}
	got := listArgv(testConfig(), h)
	if last := got[len(got)-1]; last != `exec env GATE_INBOX_HOME='/srv/gate inbox' gate-inbox machines sessions --json --tmux tmux` {
		t.Errorf("command on the machine = %s", last)
	}
}

// gate-inbox looks tmux up on PATH even to list, so a host whose tmux is
// named in full gets that directory on PATH: a non-interactive ssh on a
// Mac has no /opt/homebrew/bin.
func TestListArgvPutsTmuxOnPath(t *testing.T) {
	h := host{Name: "mini", Destination: "mini", GateInbox: "/Users/me/go/bin/gate-inbox", Tmux: "/opt/homebrew/bin/tmux", TmuxSocket: "agents"}
	got := listArgv(testConfig(), h)
	want := `exec env PATH=/opt/homebrew/bin:"$PATH" /Users/me/go/bin/gate-inbox machines sessions --json --tmux /opt/homebrew/bin/tmux --tmux-socket agents`
	if last := got[len(got)-1]; last != want {
		t.Errorf("command on the machine = %s\nwant                    %s", last, want)
	}
	out, err := exec.Command("sh", "-c", `PATH=/usr/bin:/bin; eval "$0"`, `env PATH=/opt/homebrew/bin:"$PATH" sh -c 'printf %s "$PATH"'`).Output()
	if err != nil || string(out) != "/opt/homebrew/bin:/usr/bin:/bin" {
		t.Errorf("the PATH form expands to %q (%v)", out, err)
	}
}

func TestAttachArgv(t *testing.T) {
	cfg := testConfig()
	cfg.ControlPath = "none"
	h := host{Name: "mini", Destination: "mini", Tmux: "/opt/homebrew/bin/tmux", TmuxSocket: "agents"}
	got := attachArgv(cfg, h, "ab12")
	want := []string{
		"ssh", "-t",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2",
		"--", "mini", "exec /opt/homebrew/bin/tmux -u -L agents attach-session -t '=gi_ab12'",
	}
	if !slices.Equal(got, want) {
		t.Errorf("attachArgv =\n%q\nwant\n%q", got, want)
	}
}

func TestOperatorSSHOptionsComeFirst(t *testing.T) {
	cfg := testConfig()
	cfg.SSH = []string{"/usr/bin/ssh", "-F", "/home/me/.ssh/agents"}
	got := listArgv(cfg, host{Name: "mini", Destination: "mini", GateInbox: "gate-inbox"})
	if strings.Join(got[:3], " ") != "/usr/bin/ssh -F /home/me/.ssh/agents" {
		t.Errorf("argv starts %q", got[:3])
	}
}
