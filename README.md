# gate-inbox-machines

See agents on your other machines in one [Gate Inbox](https://github.com/usestring/gate-inbox)
board. Each mirror is a local row that follows the agent's remote status and opens its terminal
over SSH. The remote board still runs the agent and reads its pane.

## Install

Use Go 1.27.3 or newer to install this build, which uses stock Gate Inbox v1.1.0.

```sh
go install github.com/hung-ng/gate-inbox-machines/cmd/gate-inbox@latest
```

Install it on each machine so your board can run `machines sessions` there. The remote board
itself can run stock Gate Inbox. Keep your existing config and sessions.

## Add and manage machines

Start the remote board and set up SSH key access from your machine. Then press `O` on your
board and `a` to add a machine, or run:

```sh
gate-inbox machines add user@host
gate-inbox machines check
```

The Gate Inbox key map (`h`) and Quick Actions (`:`) list **add and manage your other
machines** (`O`) and **new session on another machine** (`N`).

Use an SSH alias, hostname, or `user@host`. If an SSH check fails, run the command it shows
in a terminal, then retry. Press `T` on the board to open a terminal.

Only the SSH destination is required. You can give the machine a name or put its rows in an
existing local group. Use `g` on the main list to make a group. Set **Board home** if the
remote board uses a custom `GATE_INBOX_HOME`. Set **Binary path** if SSH finds a different
`gate-inbox` first. Press Alt+a in the add form to show these path overrides. Run
`gate-inbox machines add --help` for CLI options.

On the machines screen:

| Key | Action |
|---|---|
| `a` | Add a machine |
| Enter | Check the selected machine or fill the add form from a suggestion |
| `c` | Check the selected machine |
| Space | Pause or resume polling |
| `n` | Start a session on the selected machine |
| `x` | Remove the machine. Press `y` or Enter to confirm, or Esc to cancel |

The CLI also has `machines list`, `pause`, `resume`, and `remove`. Run the verbs that
change the registry from an operator terminal outside a Gate Inbox session. Suggestions come
from concrete hosts in `~/.ssh/config` and the optional `discover` command. SSH checks start
when you submit the add form.

Machines live in `extensions/machines/machines.json` under the board's config directory.
If you have old `[extensions.machines.<name>]` tables, delete them and add those machines
through the screen or CLI.

## Use mirrors

- Press Enter on a waiting mirror to read the question and answer in the remote terminal.
  Snippet keys and `r` can type into the pane while a dialog is out of view, so open the
  terminal to respond.
- Use `ctrl+q` to leave the focused mirror or advance triage. `ctrl+\` returns to the list.
  If scrolling put the remote pane in tmux scroll mode, press `q` inside the mirror to leave it.
- Press `N` on the list to start a remote session. Choose a machine and press Enter to load
  its installed CLIs. With one machine, it is already selected. An agent can take a model,
  name, first task, group, and directory. **Terminal** takes a directory and opens a shell
  on the worker's tmux server. It appears on the controller as a mirror. Type `exit`
  in the shell to end it. The check and launch both use the worker's login shell.
- Restarting your board leaves mirrors running. Use `v` to revive a dead mirror. Press `alt+x`
  on a mirror, or choose **dismiss the selected remote mirror** from Quick Actions, to close
  the local SSH pane and archive its row. The remote agent keeps running. The row stays dismissed
  until you restore it or the remote session ends. To restore it, open the archive from the `:`
  menu, press `u` on its row, and confirm.
- Offline, stopped and paused machines keep their rows and last status. A badge and the
  `remote` details show the state and remote working directory. Removing a machine archives
  its mirrors. After losing SSH, an open mirror retries with delays up to 10 seconds.
- A selected mirror shows the worker's CPU and RAM under `remote`. The board marks the
  last reading stale if it stops getting worker stats. The `computer` meters show the
  machine running the board.
- Sessions need a `gi_<id>` tmux session. A pane adopted in place does not have one. To relaunch
  it as a board session, use stock `a` on the remote board.

On macOS, an agent that needs the login keychain may need its tmux server started from the
desktop session. For example, run `tmux -L <socket> start-server \; set -s exit-empty off`
from there before spawning over SSH.

## Settings

Set these under `[extensions.machines]` in your board's `config.toml`.

| Key | Default | Purpose |
|---|---|---|
| `poll` | `"3s"` | How often the board polls each machine |
| `ssh` | `["ssh"]` | SSH executable and any extra options |
| `control_path` | `/tmp/gate-inbox-machines-<uid>/%C` | Shared SSH socket path. Use a private directory with mode 0700, or set `"none"` to disable sharing |
| `discover` | unset | Command that prints one suggested SSH destination per line |

Registry edits take effect on the next poll. Restart the board after editing
`[extensions.machines]`. A running mirror keeps the SSH settings it started with. Kill it
and press `v` to pick up changes to those settings. When a mirror reconnects, it rereads
the machine and its learned tmux settings.

The remote board's `tmux_socket` config takes precedence over `GATE_INBOX_TMUX_SOCKET`
and tmux's default socket. The extension learns the socket from the remote command, so an
environment-only setting must also be available to that command on first discovery.

Uncheck `mirror` in Settings → CLIs to hide it in both local launch pickers.
Use Shift+n to start a session on another machine. The board creates its mirror.

## Verify

```sh
GOWORK=off go build ./cmd/gate-inbox
GOWORK=off go vet ./...
env -u TMUX -u TMUX_PANE TMUX_TMPDIR="$(mktemp -d)" GOWORK=off go test -race ./...
```

Apache-2.0. See [LICENSE](LICENSE).
