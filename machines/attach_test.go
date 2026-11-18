package machines

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scripted returns exit codes in order and records how long the loop
// slept between them.
type scripted struct {
	codes  []int
	runs   int
	sleeps []time.Duration
}

func (s *scripted) run(context.Context) (int, error) {
	code := s.codes[s.runs]
	s.runs++
	return code, nil
}

func (s *scripted) sleep(_ context.Context, d time.Duration) error {
	s.sleeps = append(s.sleeps, d)
	return nil
}

// The loop never ends on its own: a pane whose command exits is left on a
// local login shell, which triage would hand over as if it were the agent
// on the other machine. It ends when its context does, which is the pane
// being closed.
func TestAttachLoopRetriesEveryFailureUntilThePaneCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scripted{codes: []int{sshFailed, sshFailed, 0, 1, 1, 127}}
	run := func(ctx context.Context) (int, error) {
		if s.runs == len(s.codes)-1 {
			cancel()
		}
		return s.run(ctx)
	}
	var out bytes.Buffer
	var notes []int
	err := attachLoop(ctx, &out, run, "mini", "a1", s.sleep, func(code int, _ string) { notes = append(notes, code) })
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled: only closing the pane ends it", err)
	}
	if s.runs != 6 {
		t.Errorf("ran ssh %d times, want 6", s.runs)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 500 * time.Millisecond, time.Second, 2 * time.Second}
	if len(s.sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", s.sleeps, want)
	}
	for i := range want {
		if s.sleeps[i] != want[i] {
			t.Errorf("sleep %d = %s, want %s", i, s.sleeps[i], want[i])
		}
	}
	for _, said := range []string{"lost the connection to mini", "would not attach to gi_a1 (exit 1)"} {
		if !strings.Contains(out.String(), said) {
			t.Errorf("pane did not say %q:\n%s", said, out.String())
		}
	}
	// Each failure is noted once until an attach succeeds: 255 (twice in a
	// row, noted once), then 1 after the clean attach, noted once for two.
	if fmt.Sprint(notes) != "[255 1]" {
		t.Errorf("notes = %v, want [255 1]", notes)
	}
}

func TestAttachLoopBacksOffToACap(t *testing.T) {
	codes := make([]int, 0, 12)
	for range 10 {
		codes = append(codes, sshFailed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &scripted{codes: codes}
	run := func(ctx context.Context) (int, error) {
		if s.runs == len(s.codes)-1 {
			cancel()
		}
		return s.run(ctx)
	}
	_ = attachLoop(ctx, &bytes.Buffer{}, run, "mini", "a1", s.sleep, nil)
	if last := s.sleeps[len(s.sleeps)-1]; last != maxRetry {
		t.Errorf("last retry = %s, want the cap %s", last, maxRetry)
	}
}

func TestAttachLoopSaysWhenTmuxIsMissing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scripted{codes: []int{127}}
	stop := func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	var out bytes.Buffer
	_ = attachLoop(ctx, &out, s.run, "mini", "a1", stop, nil)
	if !strings.Contains(out.String(), "has no tmux") {
		t.Errorf("pane said %q", out.String())
	}
}

func TestAttachLoopStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run := func(context.Context) (int, error) { cancel(); return 0, nil }
	err := attachLoop(ctx, &bytes.Buffer{}, run, "mini", "a1", sleepCtx, nil)
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestRunAttachDropsTheEmptyPromptAndChecksItsOperands(t *testing.T) {
	e := newManaged(t, healthyFar(), mini)
	var argv []string
	ctx, cancel := context.WithCancel(context.Background())
	e.attach = func(_ context.Context, a []string) (int, error) { argv = a; cancel(); return 1, nil }
	_ = e.runAttach(ctx, []string{"", "mini", "ab12"}, nil)
	if len(argv) == 0 || argv[len(argv)-1] != "exec tmux -u attach-session -t '=gi_ab12'" {
		t.Errorf("ran %q", argv)
	}
	for _, args := range [][]string{{"mini", "AB;x"}, {"mini"}} {
		if err := e.runAttach(context.Background(), args, nil); err == nil {
			t.Errorf("attach %q ran, want an error", args)
		}
	}
}

func TestRunAttachFindsActiveOrDismissedMirrorByRowID(t *testing.T) {
	for _, field := range []string{"mirrors", "dismissed"} {
		t.Run(field, func(t *testing.T) {
			e := newManaged(t, healthyFar(), mini)
			t.Setenv("GATE_INBOX_SESSION_ID", "abcd")
			dir, err := e.conf.DataDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"`+field+`":{"mini/ab12":"abcd"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			var argv []string
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			e.attach = func(_ context.Context, a []string) (int, error) { argv = a; cancel(); return 0, nil }
			if err := e.runAttach(ctx, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(argv) == 0 || argv[len(argv)-1] != "exec tmux -u attach-session -t '=gi_ab12'" {
				t.Fatalf("attach ran %q", argv)
			}
		})
	}
}

func TestRunAttachWaitsForMissingMirrorMapping(t *testing.T) {
	e := newManaged(t, healthyFar(), mini)
	t.Setenv("GATE_INBOX_SESSION_ID", "abcd")
	e.attach = func(context.Context, []string) (int, error) {
		t.Fatal("attach ran without a mapping")
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := e.runAttach(ctx, nil, nil); err != nil || ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("attach returned before cancellation: %v, context %v", err, ctx.Err())
	}
}

func TestRunAttachReloadsLearnedSocketAfterFailure(t *testing.T) {
	e := newManaged(t, healthyFar(), mini)
	reg := e.machines()
	if err := os.WriteFile(filepath.Join(reg.dir, learnedFileName), []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var commands []string
	e.attach = func(_ context.Context, argv []string) (int, error) {
		commands = append(commands, argv[len(argv)-1])
		if len(commands) == 1 {
			socket := "learned"
			if _, err := reg.learn(mini.Name, mini.Destination, about{TmuxSocket: &socket}); err != nil {
				t.Fatal(err)
			}
			// Use a separate registry to mimic the board writing this file.
			writer := newRegistry(reg.dir)
			socket = "updated"
			if _, err := writer.learn(mini.Name, mini.Destination, about{TmuxSocket: &socket}); err != nil {
				t.Fatal(err)
			}
			return sshFailed, nil
		}
		cancel()
		return 0, nil
	}
	if err := e.runAttach(ctx, []string{mini.Name, "ab12"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || strings.Contains(commands[0], "-L") || !strings.Contains(commands[1], "-L updated") {
		t.Fatal(commands)
	}
}
