package machines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// attachRunner runs one interactive ssh in the pane and returns its exit
// code. Tests swap it for one that returns codes from a script.
type attachRunner func(ctx context.Context, argv []string) (code int, err error)

func execAttach(ctx context.Context, argv []string) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// ssh exits 255 for its own failures -- no route, a dropped connection --
// and with the code of the command it ran on the machine otherwise.
const sshFailed = 255

const (
	firstRetry = time.Second
	maxRetry   = 30 * time.Second
)

// attachLoop keeps a mirror's pane attached to its session until the pane
// is closed. It never ends on its own: the board ends the pane, through
// its reconcile loop, once the session is gone from its machine's list.
//
// That matters because a pane whose command exits is left on a login shell
// on this machine. Triage would then hand the operator a row that looks
// like the agent on the other machine and types into a local shell. So every failure is
// shown in the pane and retried, backing off:
//
//   - 0 is a detach from the machine's tmux, or its client being closed. It
//     attaches again at once, since the pane is only there to show that
//     session.
//   - 255 is ssh's own failure: no route, a dropped connection.
//   - 127 is the machine's shell finding no tmux on the PATH ssh gives it.
//   - Anything else is tmux refusing to attach: the session has just
//     ended, or the target is wrong.
//
// note is told about each failure once, until the attach next succeeds, so
// the board's log can say why a mirror is stuck.
func attachLoop(ctx context.Context, out io.Writer, attempt func(context.Context) (int, error), hostName, id string,
	sleep func(context.Context, time.Duration) error, note func(code int, msg string)) error {
	retry := firstRetry
	lastCode := 0
	for {
		code, err := attempt(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && code == 0 {
			retry, lastCode = firstRetry, 0
			if err := sleep(ctx, 500*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		var msg string
		switch {
		case err != nil:
			code = -1
			msg = fmt.Sprintf("could not attach: %v", err)
		case code == sshFailed:
			msg = "lost the connection to " + hostName
		case code == 127:
			msg = hostName + " has no tmux on the PATH ssh gives it"
		default:
			msg = fmt.Sprintf("tmux on %s would not attach to gi_%s (exit %d)", hostName, id, code)
		}
		fmt.Fprintf(out, "\r\n[gate-inbox machines] %s; trying again in %s\r\n", msg, retry)
		if code != lastCode && note != nil {
			note(code, msg)
		}
		lastCode = code
		if err := sleep(ctx, retry); err != nil {
			return err
		}
		retry = min(2*retry, maxRetry)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
