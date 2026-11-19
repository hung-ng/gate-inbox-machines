package machines

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

func TestDismissRequiresAMirrorAndArchivesTheLocalRow(t *testing.T) {
	ctx := context.Background()
	e := New()
	board := newFakeBoard()
	board.sessions["local"] = extension.SessionInfo{ID: "local", Name: "local job"}
	board.sessions["mirror"] = extension.SessionInfo{ID: "mirror", Name: "mini/job", Role: mirrorRole, Running: true}
	e.board = board
	ui := &spawnUI{notifications: make(chan string, 1)}
	e.ui = ui
	if err := e.openDismiss(ctx, extension.Press{SessionID: "local"}); err == nil || ui.opened != nil {
		t.Fatal("dismiss accepted a local job")
	}
	if err := e.openDismiss(ctx, extension.Press{SessionID: "mirror"}); err != nil {
		t.Fatal(err)
	}
	if ui.screen != dismissScreen || !strings.Contains(text(ui.opened.Render(44, 6)), "The remote session keeps running.") {
		t.Fatal(ui.opened)
	}
	if ui.opened.Key(extension.ViewKey{Action: extension.ActionClose}) {
		t.Fatal("closed without confirmation")
	}
	if !ui.opened.Key(extension.ViewKey{Action: "dismiss"}) {
		t.Fatal("confirmation did not close the dialog")
	}
	select {
	case <-ui.notifications:
	case <-time.After(time.Second):
		t.Fatal("dismiss did not finish")
	}
	if _, err := board.Get(ctx, "mirror"); err == nil {
		t.Fatal("local mirror stayed active")
	}
	if _, err := board.Get(ctx, "local"); err != nil {
		t.Fatal("dismiss archived the local job")
	}
}
