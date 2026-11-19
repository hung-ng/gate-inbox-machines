package machines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// attachRunner runs one interactive ssh in the pane and returns its exit
// code. Tests swap it for one that returns codes from a script.
type attachRunner func(ctx context.Context, argv []string) (code int, err error)

func attachContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, syscall.SIGHUP)
	fd := int(os.Stdin.Fd())
	// A terminal that closed before this check can already return EIO.
	if _, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err != nil && !errors.Is(err, unix.EIO) {
		return ctx, stop
	}

	// A pane can close without delivering SIGHUP to this process. Poll
	// for hangup without reading the input that belongs to SSH.
	ctx, cancel := context.WithCancel(ctx)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := unix.Poll(fds, 0); err == nil && fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return ctx, func() {
		cancel()
		stop()
	}
}

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
	maxRetry   = 10 * time.Second
)

// attachLoop keeps a mirror's pane attached to its session until the pane
// closes or the context is canceled. The board closes the pane when the
// remote session disappears from its machine's list.
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
