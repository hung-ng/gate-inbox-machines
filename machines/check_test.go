package machines

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/usestring/gate-inbox/extension"
)

// exitStatus stands in for an *exec.ExitError.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (e exitStatus) ExitCode() int { return int(e) }

func TestCheckFailures(t *testing.T) {
	for _, tc := range []struct {
		step             int
		msg, reply, want string
	}{
		{0, "Permission denied", "", "ssh-copy-id mini"},
		{0, "Host key verification failed", "", "ssh mini true"},
		{0, "Could not resolve hostname", "", "accepts SSH logins"},
		{0, "Connection timed out", "", "accepts SSH logins"},
		{0, "Connection refused", "", "accepts SSH logins"},
		{1, "exit 127", "", "add it again with --gate-inbox"},
		{1, "connect to 127.0.0.1 failed", "", "check that gate-inbox runs"},
		{2, "unknown command machines", "", "install this build there"},
		{3, "not found", "", "make it available on PATH"},
		{4, "", "garbage", "sessions reply FAIL"},
		{4, "", `{"version":2}`, "same build on both"},
		{5, "", `{"version":1,"board_running":false,"sessions":[]}`, "on mini, start Gate Inbox"},
		{6, "", `{"version":1,"sessions":[{"id":"a1","running":true},{"id":"b2","attachable":true}]}`, "check tmux_socket"},
	} {
		t.Run(tc.want+tc.msg, func(t *testing.T) {
			e := newManaged(t, healthyFar(), mini)
			calls := 0
			e.run = func(_ context.Context, a []string) ([]byte, error) {
				step := calls
				calls++
				if !strings.Contains(strings.Join(a, " "), "BatchMode=yes") {
					t.Fatal(a)
				}
				if step == tc.step && tc.msg == "exit 127" {
					return nil, fmt.Errorf("wrapped: %w", exitStatus(127))
				}
				if step == tc.step && tc.msg != "" {
					return nil, errors.New(tc.msg)
				}
				if step == 4 {
					return []byte(tc.reply), nil
				}
				return nil, nil
			}
			var out bytes.Buffer
			if e.runCheck(context.Background(), nil, &out) == nil || !strings.Contains(out.String(), tc.want) {
				t.Fatal(out.String())
			}
		})
	}
}

func TestCheckHealthyAndNamed(t *testing.T) {
	e := newManaged(t, healthyFar(), mini, box)
	e.run = func(_ context.Context, a []string) ([]byte, error) {
		if strings.Contains(strings.Join(a, " "), "-- box") {
			t.Fatal(a)
		}
		return []byte(`{"version":1,"board_running":true,"sessions":[{"id":"a1","running":true,"attachable":true}]}`), nil
	}
	var out bytes.Buffer
	if err := e.runCheck(context.Background(), []string{"mini"}, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if strings.Count(out.String(), " OK") != 7 {
		t.Fatal(out.String())
	}
	if e.runCheck(context.Background(), []string{"missing"}, &out) == nil {
		t.Fatal("unknown machine accepted")
	}
	e.reg = newRegistry("")
	if err := e.runCheck(context.Background(), nil, &out); err == nil || !strings.Contains(err.Error(), "no machines configured") {
		t.Fatal("no machines accepted:", err)
	}
}

func TestCheckIgnoresStoppedSessions(t *testing.T) {
	far := healthyFar()
	far.snap.Sessions = []machineSession{{ID: "a1", Status: "dead"}}
	e := newManaged(t, far, mini)
	var out bytes.Buffer
	if err := e.runCheck(context.Background(), nil, &out); err != nil {
		t.Fatalf("check with no running sessions: %v\n%s", err, out.String())
	}
}

func TestSpawnReplyGreeting(t *testing.T) {
	for _, s := range []string{`{"id":"ab12"}`, "hello from .zprofile\n{\n  \"id\": \"ab12\"\n}\n"} {
		if id, err := parseSpawnReply([]byte(s)); err != nil || id != "ab12" {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"hello", `{"id":"bad id"}`} {
		if _, err := parseSpawnReply([]byte(s)); err == nil {
			t.Fatal(s)
		}
	}
}

type callerHost struct {
	extension.Host
	caller string
}

func (h callerHost) Caller() string { return h.caller }

func TestMachinesCLIMutationsRequireOperator(t *testing.T) {
	e := newManaged(t, healthyFar(), mini)
	for _, verb := range []string{"add", "remove", "pause", "resume"} {
		if err := e.runMachines(context.Background(), []string{verb, "mini"}, callerHost{caller: "a1"}); err == nil || !strings.Contains(err.Error(), "operator terminal") {
			t.Fatalf("%s from an agent: %v", verb, err)
		}
	}
	if err := e.runMachines(context.Background(), []string{"pause", "mini"}, callerHost{}); err != nil {
		t.Fatal(err)
	}
	if h, ok := e.machines().host("mini"); !ok || !h.Paused {
		t.Fatal(h, ok)
	}
	if err := e.runMachines(context.Background(), []string{"check", "--help"}, callerHost{caller: "a1"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatal(err)
	}
}
