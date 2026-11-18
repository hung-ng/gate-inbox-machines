package machines

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/usestring/gate-inbox/extension/cmdline"
)

// boardSession is the tmux session start-board runs a machine's board in.
const boardSession = "gate-inbox-board"

// startBoardArgv starts h's board in a detached tmux session of its own on
// tmux's default server there, through the operator's login shell, so a
// tmux server it starts has the PATH their login files set. A server
// already running keeps its own environment, which the board then gets.
func startBoardArgv(cfg config, h host) []string {
	words := joinShell([]string{h.Tmux, "new-session", "-d", "-s", boardSession, "--"})
	board := shellWord(h.GateInbox)
	if h.Home != "" {
		board = "env GATE_INBOX_HOME=" + shellWord(h.Home) + " " + board
	}
	return loginShell(cfg, h.Destination, "exec "+words+" "+board)
}

// startBoard starts a machine's board, unless it is running already, and
// says what happened in a line.
func (e *Extension) startBoard(ctx context.Context, h host) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ensureControlDir(e.cfg.ControlPath); err != nil {
		return "", err
	}
	snap, err := pollHost(ctx, e.run, e.cfg, h)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %w", h.Name, err)
	}
	if snap.About != nil {
		_, _ = e.machines().learn(h.Name, h.Destination, *snap.About)
		h = h.withAbout(*snap.About)
	}
	if snap.boardRunning() {
		return h.Name + "'s board is already running", nil
	}
	if _, err := e.run(ctx, startBoardArgv(e.cfg, h)); err != nil {
		if strings.Contains(err.Error(), "duplicate session") {
			return "", fmt.Errorf("%s already has a tmux session named %s; attach to it with `ssh -t %s tmux attach -t %s` and start the board there",
				h.Name, boardSession, h.Destination, boardSession)
		}
		return "", fmt.Errorf("could not start %s's board: %w", h.Name, err)
	}
	msg := fmt.Sprintf("Started %s's board in tmux session %s there; `ssh -t %s tmux attach -t %s` shows it", h.Name, boardSession, h.Destination, boardSession)
	if h.About != nil && h.About.OS == "darwin" {
		msg += ". On a Mac, agents a board started over ssh starts can't read the login keychain; see the README"
	}
	return msg, nil
}

func (e *Extension) runStartBoard(ctx context.Context, args []string) error {
	set := cmdline.NewFlagSet(usageStartBoard)
	operands, err := cmdline.Parse(os.Stdout, program, set, args, 1, 1)
	if err != nil {
		return err
	}
	h, ok := e.machines().host(operands[0])
	if !ok {
		return fmt.Errorf("no machine %q", operands[0])
	}
	msg, err := e.startBoard(ctx, h)
	if err != nil {
		return err
	}
	fmt.Println(msg + ".")
	return nil
}
