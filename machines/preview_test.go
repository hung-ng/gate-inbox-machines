package machines

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/usestring/gate-inbox/extension"
)

func TestSelectedMirrorReadsItsRemotePreview(t *testing.T) {
	ctx := context.Background()
	e := New()
	e.cfg = config{Poll: defaultPoll, SSH: []string{"ssh"}, ControlPath: "none", Hosts: []host{mini}}
	reply := hosts{"mini": snapshot{Version: wireVersion, Sessions: []machineSession{live("a1", "waiting")}}}
	want := extension.SessionPreview{
		Messages: []extension.PreviewMessage{{Role: "user", Text: "Fix the flaky login test"}},
		Dialog:   &extension.PreviewDialog{Kind: "permission prompt", Lines: []string{"npm test -- login"}},
	}
	previewReads := 0
	e.run = func(ctx context.Context, argv []string) ([]byte, error) {
		command := argv[len(argv)-1]
		if strings.Contains(command, " machines preview ") {
			if argv[len(argv)-2] != "mini" || !strings.HasSuffix(command, " machines preview a1") {
				t.Errorf("preview read went to the wrong session: %q", argv)
			}
			previewReads++
			return json.Marshal(want)
		}
		return reply.run(ctx, argv)
	}
	board := newFakeBoard()
	m := newMirrors(e.cfg, board, nil, quiet(), e.run, t.TempDir())
	m.previews = &e.previews
	m.pass(ctx)

	id := m.state.Mirrors["mini/a1"]
	preview := e.machineUI().Preview
	got, err := preview.Read(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Messages, want.Messages) || got.Dialog == nil || got.Dialog.Kind != want.Dialog.Kind || !slices.Equal(got.Dialog.Lines, want.Dialog.Lines) {
		t.Fatalf("preview = %+v, want %+v", got, want)
	}
	if _, err := preview.Read(ctx, "another-row"); err == nil {
		t.Fatal("an unmapped row could read a remote preview")
	}
	reply["mini"] = errors.New("offline")
	m.pass(ctx)
	got, err = preview.Read(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "unavailable") || got.Dialog != nil || len(got.Messages) != 0 {
		t.Fatalf("offline preview = %+v, %v", got, err)
	}
	if previewReads != 1 {
		t.Fatalf("SSH preview reads = %d, want 1", previewReads)
	}
}
