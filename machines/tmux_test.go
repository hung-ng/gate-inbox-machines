package machines

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseTmuxSessionsKeepsOnlyBoardSessions(t *testing.T) {
	got := parseTmuxSessions("gi_ab12\ngi_poll-anchor\nwork\ngi_\ngi_CD34\n  gi_ef56  \n")
	if len(got) != 2 || !got["ab12"] || !got["ef56"] {
		t.Errorf("parsed %v, want ab12 and ef56 only", got)
	}
}

func TestTmuxSessionsReadsNoServerAsNoSessions(t *testing.T) {
	for _, msg := range []string{
		"exit status 1: no server running on /tmp/tmux-501/agents",
		"exit status 1: error connecting to /private/tmp/tmux-501/agents (No such file or directory)",
	} {
		run := func(context.Context, []string) ([]byte, error) { return nil, errors.New(msg) }
		got, _, err := tmuxSessions(context.Background(), run, []string{"tmux"})
		if err != nil || len(got) != 0 {
			t.Errorf("%q: got %v, %v; want no sessions and no error", msg, got, err)
		}
	}
	run := func(context.Context, []string) ([]byte, error) {
		return nil, errors.New(`exec: "tmux": executable file not found in $PATH`)
	}
	if _, _, err := tmuxSessions(context.Background(), run, []string{"tmux"}); err == nil {
		t.Error("a tmux that cannot run listed nothing, want an error")
	}
}

// Against a real tmux on a private socket: the board's session is
// attachable, a session started by hand is not. -f /dev/null keeps the
// operator's ~/.tmux.conf out of it.
func TestTmuxSessionsOnARealServer(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux on PATH")
	}
	socket := filepath.Join(t.TempDir(), "s")
	server := []string{tmux, "-f", "/dev/null", "-S", socket}
	t.Cleanup(func() { _ = exec.Command(tmux, "-S", socket, "kill-server").Run() })
	for _, name := range []string{"gi_ab12", "by-hand"} {
		cmd := exec.Command(server[0], append(server[1:], "new-session", "-d", "-s", name, "sleep 30")...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("start %s: %v %s", name, err, out)
		}
	}
	got, _, err := tmuxSessions(context.Background(), execRunner, server)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["ab12"] {
		t.Errorf("attachable = %v, want ab12 only", got)
	}

	work := filepath.Join(t.TempDir(), "work with spaces")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	work, err = filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	terminal := append(append([]string{}, server...), "new-session", "-d", "-s", "gi_ef12", "-c", work, "sleep 30",
		";", "set-option", "-t", "gi_ef12", terminalMark, "1")
	if _, err := execRunner(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	attachable, terminals, err := tmuxSessions(context.Background(), execRunner, server)
	if err != nil || !attachable["ab12"] || !attachable["ef12"] || len(terminals) != 1 || terminals[0].ID != "ef12" || terminals[0].Directory != work {
		t.Fatal(attachable, terminals, err)
	}

	modes, err := tmuxScrolling(context.Background(), execRunner, server)
	if err != nil || modes["ab12"] {
		t.Fatal(modes, err)
	}
	if _, err := execRunner(context.Background(), append(append([]string{}, server...), "copy-mode", "-t", "gi_ab12")); err != nil {
		t.Fatal(err)
	}
	modes, err = tmuxScrolling(context.Background(), execRunner, server)
	if err != nil || !modes["ab12"] {
		t.Fatal(modes, err)
	}
	if _, err := execRunner(context.Background(), append(append([]string{}, server...), "send-keys", "-t", "gi_ab12", "-X", "cancel")); err != nil {
		t.Fatal(err)
	}
	modes, err = tmuxScrolling(context.Background(), execRunner, server)
	if err != nil || modes["ab12"] {
		t.Fatal(modes, err)
	}
	none := []string{tmux, "-f", "/dev/null", "-S", filepath.Join(t.TempDir(), "none")}
	if got, _, err := tmuxSessions(context.Background(), execRunner, none); err != nil || len(got) != 0 {
		t.Errorf("no server: got %v, %v", got, err)
	}
}

// A server kept up with no sessions (exit-empty off), the way an agents'
// server is started ahead of its first agent, has nothing to list. tmux says
// so differently per command: list-sessions prints nothing, list-panes -a
// fails with "no current target".
func TestTmuxListingsOnAnEmptyServer(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux on PATH")
	}
	socket := filepath.Join(t.TempDir(), "s")
	server := []string{tmux, "-f", "/dev/null", "-S", socket}
	t.Cleanup(func() { _ = exec.Command(tmux, "-S", socket, "kill-server").Run() })
	cmd := exec.Command(server[0], append(server[1:], "start-server", ";", "set", "-s", "exit-empty", "off")...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start an empty server: %v %s", err, out)
	}
	if out, err := exec.Command(tmux, "-S", socket, "show", "-s", "exit-empty").CombinedOutput(); err != nil {
		t.Fatalf("the empty server did not stay up: %v %s", err, out)
	}
	if got, _, err := tmuxSessions(context.Background(), execRunner, server); err != nil || len(got) != 0 {
		t.Errorf("sessions on an empty server: got %v, %v; want none and no error", got, err)
	}
	if got, err := tmuxScrolling(context.Background(), execRunner, server); err != nil || len(got) != 0 {
		t.Errorf("pane modes on an empty server: got %v, %v; want none and no error", got, err)
	}
}

func TestTmuxListingsRejectSocketFailures(t *testing.T) {
	for _, msg := range []string{
		"exit status 1: error connecting to /tmp/tmux-501/agents (Socket operation on non-socket)",
		"exit status 1: error connecting to /tmp/tmux-501/agents (Permission denied)",
	} {
		run := func(context.Context, []string) ([]byte, error) { return nil, errors.New(msg) }
		if _, _, err := tmuxSessions(context.Background(), run, []string{"tmux"}); err == nil {
			t.Fatal("list-sessions accepted", msg)
		}
		if _, err := tmuxScrolling(context.Background(), run, []string{"tmux"}); err == nil {
			t.Fatal("list-panes accepted", msg)
		}
	}
	run := func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("exit status 1: no current target")
	}
	if _, _, err := tmuxSessions(context.Background(), run, []string{"tmux"}); err == nil {
		t.Fatal("list-sessions accepted no current target")
	}
	if got, err := tmuxScrolling(context.Background(), run, []string{"tmux"}); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}
