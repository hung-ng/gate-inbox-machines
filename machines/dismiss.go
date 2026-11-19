package machines

import (
	"context"
	"errors"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

const dismissScreen = "mirror_dismiss"

func dismissKeys(e *Extension) []extension.KeyBinding {
	return []extension.KeyBinding{
		{Action: "dismiss_mirror", Label: "dismiss the selected remote mirror", Keys: []string{"alt+x"}, Run: e.openDismiss},
		{Screen: dismissScreen, Action: "dismiss", Label: "dismiss", Keys: []string{"y", "enter"}},
		{Screen: dismissScreen, Action: extension.ActionClose, Label: "cancel", Keys: []string{"esc"}},
	}
}

func (e *Extension) openDismiss(ctx context.Context, press extension.Press) error {
	e.boardMu.Lock()
	board := e.board
	e.boardMu.Unlock()
	if board == nil || press.SessionID == "" {
		return errors.New("select a remote mirror first")
	}
	row, err := board.Get(ctx, press.SessionID)
	if err != nil {
		return err
	}
	if row.Role != mirrorRole || row.Archived {
		return errors.New("select an active remote mirror")
	}
	e.ui.Open(dismissScreen, &dismissView{e: e, board: board, ctx: ctx, row: row})
	return nil
}

type dismissView struct {
	e     *Extension
	board boardOps
	ctx   context.Context
	row   extension.SessionInfo
}

func (*dismissView) Title() string { return "Dismiss remote mirror" }

func (v *dismissView) Render(width, _ int) []extension.Line {
	return wrapLines([]extension.Line{
		{{Text: "Close and archive " + v.row.Name + " on this board?"}},
		{{Text: "The remote session keeps running."}},
	}, width)
}

func (v *dismissView) Key(key extension.ViewKey) bool {
	if key.Action != "dismiss" {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(v.ctx, 15*time.Second)
		defer cancel()
		if err := v.board.Archive(ctx, v.row.ID); err != nil {
			v.e.ui.Notify("Could not dismiss " + v.row.Name + ": " + err.Error())
			return
		}
		v.e.ui.Notify("Dismissed " + v.row.Name + ". The remote session keeps running.")
		v.e.wakeLoop("")
	}()
	return true
}
