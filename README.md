# gate-inbox-machines

A [Gate Inbox](https://github.com/usestring/gate-inbox) extension that puts the agents running on
your other machines on your board, as ordinary rows. They sort into the same triage queue as your
local sessions, their preview is the other machine's pane, and focusing one types into the agent
there.

Without it, agents on three machines mean three boards in three SSH windows, three queues, and
notifications only from whichever board you are attached to.

## How it works

Each machine keeps running its own board, which still reads its own panes and holds its own
questions. This extension adds two things to the same build:

- **`gate-inbox machines sessions --json`** prints a board's agent sessions. It works from a plain
  shell, so another machine can run it over SSH.
- **A loop on your board** runs that command on each configured machine every few seconds. For each
  session there it launches a local row (a "mirror") whose pane runs
  `ssh -t <machine> tmux attach -t =gi_<id>`, and pins the row's status to the one that machine's
  board reports. When the session ends on its machine, the mirror is archived.

Everything after that is the board's own code. Triage hands over a mirror that is waiting, focus
is the other machine's agent on screen, and answering it there answers that agent.

It is built entirely on the public `extension` package: a role with `PinnedStatus`, `Launch`,
`PinStatus`, `Archive`, badges, a detail line, and a CLI command. Nothing in Gate Inbox is
changed.

## Install

On the machine you sit at, build this in place of the `gate-inbox` you run now:

```bash
go install github.com/hung-ng/gate-inbox-machines/cmd/gate-inbox@latest
```

Your config, state and sessions carry over. It is pinned to Gate Inbox v1.1.0, the release a plain
`go install github.com/usestring/gate-inbox@latest` installs; `go get
github.com/usestring/gate-inbox@<version>` and a rebuild moves it to another.

Each other machine needs this build only to answer `machines sessions`. Its board can keep
running stock Gate Inbox, as long as `gate_inbox` names a copy of this build on that machine.

Each machine needs an SSH key login from the one you sit at that works without a prompt, because
the extension runs SSH with `BatchMode=yes`. Check it with `ssh <machine> true`.

## Configure

On the machine you sit at, in `config.toml`, one table per machine. The table's name is what its
rows' badges say:

```toml
[extensions.machines.mini]
destination = "hung-mini"                      # an ssh destination; defaults to the table's name

[extensions.machines.box]
destination = "me@box.example.ts.net"
```

Most setups need only `destination`. The PATH a non-interactive SSH command gets usually leaves
out Homebrew, so commands sent to a machine look for `gate-inbox` and tmux on its PATH and then in
`~/go/bin`, `~/.local/bin`, `/opt/homebrew/bin` and `/usr/local/bin`. Those come after the
machine's own PATH, so they never override it. Set `gate_inbox` or `tmux` to a full path when
they live somewhere else.

Then check the setup from a plain shell:

```bash
gate-inbox machines check        # every configured machine
gate-inbox machines check mini   # just one
```

It tries each step the board needs, in order: SSH without a prompt, `gate-inbox` and this build's
`machines` command there, tmux, the sessions reply, that machine's board running, and a session a
mirror can attach to. A failed step ends with what to change, and the command exits non-zero:

```text
mini: ssh OK
mini: gate-inbox FAIL (exit status 127: env: gate-inbox: No such file or directory); set gate_inbox to its full path
```

Other keys. The first three go directly under `[extensions.machines]`, so a machine can't be
named `poll`, `ssh` or `control_path`:

| Key | Default | What it does |
|---|---|---|
| `poll` | `"3s"` | How often each machine is asked for its sessions |
| `ssh` | `["ssh"]` | The ssh command and any options of your own, such as `["ssh", "-F", "/Users/me/.ssh/agents"]` |
| `control_path` | `/tmp/gate-inbox-machines-<uid>/%C` | Where ssh keeps the connection it shares between a machine's polls and its mirrors. `"none"` turns sharing off |
| `<name>.destination` | the table's name | The ssh destination: a name from `~/.ssh/config`, or `user@host` |
| `<name>.gate_inbox` | `gate-inbox` | This build on that machine |
| `<name>.tmux` | `tmux` | tmux on that machine |
| `<name>.home` | none | `GATE_INBOX_HOME` of the board on that machine, if it uses one |
| `<name>.tmux_socket` | none | That board's `tmux_socket`, if it sets one |
| `<name>.group` | top level | An existing group to file the machine's rows under. An extension cannot create a group, so make it first with `ctrl+alt+g` |

The build also supplies a `[tools.mirror]` block, which is the tool mirrors launch with. It shows
in the new-session picker. Starting it from there only prints a note saying to pick an agent CLI,
and `machines sessions` never reports such a session. Uncheck it under Settings → CLIs to hide it.

A machine's polls and mirrors share one SSH connection. It closes 10 minutes after the last of
them stops.

## What you see

- A mirror row is named after its machine and the session there, such as
  `mini-upgrade-http-client`, since the board's main list shows a row's name and tool but not its
  badges. Healthy mirrors have no badge. The detail panel has a `machine` line such as
  `mini · claude · waiting · gi_ab12`.
- When a machine can't be reached, its running mirrors are pinned to `errored`, with a
  `✕ mini offline` badge and a detail line giving the time it became unreachable. This is a
  stand-in status that keeps the connection problem visible in every layout, not a claim that
  the agent failed. The extension owns those rows so they stay out of triage, the attention
  filter and Tab jumps. Nothing is archived or relaunched until the machine answers again.
- A reachable machine whose board stopped gets the same treatment, with
  `✕ mini board stopped` and a detail line asking you to start its board. Agent processes can
  keep running after their board quits, but their saved statuses cannot be trusted. That
  machine's `machines sessions` tells by checking that the process whose PID is in its board's
  `manager.lock` is alive. It never takes that lock, which could make a board starting at the same
  moment take over from a live one. A reply from an older build without this check reads as
  running. When the board is back, the rows get their real status again.
- If a mirror can't attach, its pane says why (the connection dropped, no tmux on the PATH, tmux
  refused the target) and tries again with backoff (1s, 2s, 4s, up to 30s). The row stays, and the
  board's log names the reason once. A mirror's pane never exits on its own: a pane whose command
  exits is left on a local shell, which triage would hand you as if it were the agent on the other
  machine. If the tmux client on that machine is detached, the mirror attaches again half a second
  later.
- Restarting the board reconnects every mirror in its own row, keeping its name and priority, so
  the panes pick up any setting you changed.
- Archiving a mirror puts it away until its session stops running on its machine. On Gate Inbox
  v1.1.0, `ctrl+x` kills and archives a session, so it puts a mirror away too. Restoring the
  mirror from the archive brings it back.
- The board's log records each mirror the extension starts, reconnects, archives or sees put away,
  with the reason.

## Using it

- Enter on a mirror focuses it. Every key then goes to the agent on the other machine, except
  your board's own chords: `ctrl+q` goes back to your list (in triage, on to the next session),
  and `ctrl+\` always goes back to the list. The status line at the bottom of the pane is drawn
  by the other machine's board, so its key hints are for that board.
- `N` on the list starts an agent on another machine. See the next section.
- If scrolling leaves a mirror in tmux scroll mode, press `q` inside it to type again. On the
  list, `q` quits your board.
- `x` on the list, or `ctrl+x` inside a mirror once you confirm, closes the mirror and files its
  row in the archive. The agent on the other machine keeps running. To get the mirror back, open
  the archived view from the `:` menu, press `u` on its row, then confirm with `y` or Enter.

## Start a session on another machine

Press `N` on the list to open the machine form immediately. With one configured machine,
Machine is preselected and CLI, Task and Directory appear together. With several machines,
choose one and press Enter before filling the remaining fields. The v1.1.0 form API does not
report picker changes, so changing machines also requires Enter to refresh the CLI choices.
An empty directory uses that board's default. Enter or ctrl+s submits.

CLI choices come from each machine's most recent successful poll. Without a list yet, the
CLI field accepts a typed name. Discovery lists configured tools except shells, terminal and
both mirror names, regardless of the non-interactive SSH PATH. If a CLI is not installed,
spawn's error appears in the status bar.

Submitting runs `spawn --nest=false` on that machine through your login shell there, then polls
the machine at once so the mirror shows up. The login shell matters because Gate Inbox copies the
environment of the process that starts a session into the session. A spawn run straight from SSH
would hand the agent SSH's bare PATH, and a CLI installed by Homebrew or in `~/.local/bin` would
not be found. Polls skip the login shell, which keeps them fast. A greeting a login file prints
before the reply is skipped.

## Scroll mode limitation

Scrolling with the mouse or trackpad inside a focused mirror can put the session's pane on the
other machine into tmux's scroll mode. While it is there, what you type goes to the scrolling,
not to the agent. The row then shows a `↕ scroll mode` warning, and its detail line says to press
`q` inside the mirror, which leaves scroll mode. An offline or stopped-board warning takes
precedence over this one.

The cause: tmux on the other machine turns on mouse reporting in the mirror's pane, so wheel
events travel over SSH and reach it. A test with tmux 3.8 showed this, and changing `TERM` did
not stop it. The extension changes no tmux settings on the other machine, since its board owns
them. A full fix would sit between SSH and the pane and filter out the mouse-reporting
sequences, which is left as future work.

## Safety

- **Nothing listens on a port.** Everything goes over SSH with your key, so the extension can
  reach on a machine exactly what you could by SSH-ing in.
- **What reaches an agent on another machine:** keys you type into a focused mirror, and an agent
  you start with `N`. The board refuses to queue messages into a mirror, because the `mirror` tool
  block declares no `activity_cutoff`: a queued message would skip the checks that machine's
  board makes on when an agent can safely read input. `skip_rename_directive` and `mcp = "none"`
  keep the board's naming request and its MCP server out of a new mirror's pane. Don't add an
  `activity_cutoff` to that block.
- **Two list keys still type into a mirror straight away.** On v1.1.0, a snippet key and `r`
  (which asks the agent to name itself) type into a row's pane at once. On a local row the board
  holds a snippet back while the agent shows a dialog. It can't for a mirror: the board spots a
  dialog by matching the pane against the tool's patterns, and giving the `mirror` tool the agent
  CLIs' patterns also overrides the status pinned from the other machine. So a snippet or `r` on a
  mirror could answer a question on the other machine. Don't use either on a mirror.
- **Boards that mirror each other don't loop.** `machines sessions` leaves out mirror rows, so two
  machines can each list the other.
- **Commands sent to a machine are quoted for any login shell.** The command ssh hands the far
  side is run by your login shell there, which on a Mac is zsh. Every word is quoted so that sh,
  bash and zsh all read it back as written, and the tests check this against each shell.
- **What comes back from a machine is checked.** Session ids must be the board's hex ids before they
  are used in a tmux target, and a reply in another wire version is refused, with a message saying
  to run the same build on both machines.

## Limits

- A mirror can't move to another tool or migrate to a local agent. The board refuses the launch,
  since it would start an agent here in place of the one on the other machine. Forking is refused
  too, as a mirror has no conversation here. Use `N` to start an agent on that machine.
- A session started by another session on its machine shows here as its own top-level row, not
  under its parent. The extension API can file a new row under a parent but can't move a row later,
  and a parent reconnected on restart would leave its children under the old row. Keeping the
  layout needs a way to move a row.
- Only sessions the machine's board launched are mirrored: each has a `gi_<id>` tmux session to
  attach to. A pane the board adopted as it was, started by hand inside some other tmux session,
  is left out, because attaching would bring that whole tmux session, other windows and all, into
  the mirror. The board's log says how many a machine left out. `machines sessions` there marks them
  `not mirrored`. Bringing such a pane into the board (`ctrl+alt+a`) relaunches it as a `gi_`
  session, and it is mirrored from then on.
- A mirror's preview has no conversation to show, because the transcript lives on the other
  machine. Focus the row to see that machine's screen.
- Priority tiers set on the other machine are not carried over. Set a mirror's priority on your
  own board.
- On a narrow list the badge can be cut off. The detail line always shows the machine.

## Develop

```bash
go vet ./...
go test -race ./...
```

The tests cover config, quoting (each quoted word is checked against real `sh`, `bash` and
`zsh`), the PATH a command gets on the other machine, `machines check`, the wire format, the
reconcile plan, the reconnect loop, the launch guard, and full passes against a fake board.

It was also checked end to end with two boards on one machine. Board A ran the scripted agent from
Gate Inbox's `tools/demo/fake-agent.sh`, and board B mirrored A through a stand-in `ssh` that runs
the command locally with the bare environment a real non-interactive SSH session gets. That run
covered:

- the mirror showing A's pane
- B's triage handing over A's question, and an answer typed on B reaching A
- B refusing to queue a message into the mirror
- the machine dropping off and coming back
- the session ending on its machine and its mirror being archived

That run was done on Gate Inbox `main` (d3eb72d) and again on v1.1.0. Later runs on v1.1.0 added a
second far board and covered:

- `N` starting an agent on the second machine, and that agent finding a CLI that only the
  machine's `.zprofile` puts on PATH
- one machine offline while the other stays healthy, and `machines check` on both
- a stopped and restarted far board
- kill and revive, archive and restore, and a lost state file
- two boards mirroring each other

 The same flow then ran
between two Macs over real SSH, with the board on a MacBook mirroring a Mac mini whose login shell
is zsh. The question showed on the MacBook, an answer typed there reached the agent on the mini,
and the row went to finished. Restarting the board, reconnecting and archiving were checked on
v1.1.0 too. A real adopted pane has not been tried: the fake agent is not a CLI the board adopts,
so that case is covered by a test against a real tmux server instead.

## License

Apache-2.0, the same license as Gate Inbox. See [LICENSE](LICENSE).
