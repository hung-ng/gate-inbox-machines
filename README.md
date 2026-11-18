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

Build this on every machine, the one you sit at and each one it shows:

```bash
go install github.com/hung-ng/gate-inbox-machines/cmd/gate-inbox@latest
```

It is Gate Inbox plus this extension, so it replaces the `gate-inbox` binary you run now. Your
config, state and sessions carry over. It is pinned to Gate Inbox v1.1.0, the release a plain
`go install github.com/usestring/gate-inbox@latest` installs; `go get
github.com/usestring/gate-inbox@<version>` and a rebuild moves it to another.

Each machine needs an SSH key login from the one you sit at that works without a prompt, because
the extension runs SSH with `BatchMode=yes`. Check it with `ssh <machine> true`.

## Configure

On the machine you sit at, in `config.toml`, one table per machine. The table's name is what its
rows' badges say:

```toml
[extensions.machines.mini]
destination = "hung-mini"                      # an ssh destination; defaults to the table's name
gate_inbox  = "~/go/bin/gate-inbox"            # this build on that machine
tmux        = "/opt/homebrew/bin/tmux"         # tmux on that machine

[extensions.machines.box]
destination = "me@box.example.ts.net"
gate_inbox  = "/usr/local/bin/gate-inbox"
```

A non-interactive SSH command gets the system PATH only. On a Mac that leaves out Homebrew, so give
`gate_inbox` and `tmux` as full paths. When `tmux` is a full path, its directory is put on PATH for
the command on that machine too, because `gate-inbox` looks tmux up even to list sessions.

Other keys. The first three go directly under `[extensions.machines]`, so a machine can't be
named `poll`, `ssh` or `control_path`:

| Key | Default | What it does |
|---|---|---|
| `poll` | `"3s"` | How often each machine is asked for its sessions |
| `ssh` | `["ssh"]` | The ssh command and any options of your own, such as `["ssh", "-F", "/Users/me/.ssh/agents"]` |
| `control_path` | `/tmp/gate-inbox-machines-<uid>/%C` | Where ssh keeps the connection it shares between a machine's polls and its mirrors. `"none"` turns sharing off |
| `<name>.home` | none | `GATE_INBOX_HOME` of the board on that machine, if it uses one |
| `<name>.tmux_socket` | none | That board's `tmux_socket`, if it sets one |
| `<name>.group` | top level | An existing group to file the machine's rows under. An extension cannot create a group, so make it first with `ctrl+alt+g` |

The build also supplies a `[tools.machines]` block, which is the tool mirrors launch with. It shows
in the new-session picker. Starting it from there only prints a note saying to pick an agent CLI,
and `machines sessions` never reports such a session. Uncheck it under Settings → CLIs to hide it.

## What you see

- A mirror row is named after its machine and the session there, such as
  `mini-upgrade-http-client`, since the board's main list shows a row's name and tool but not its
  badges. In the sidebar it carries a `⇄ mini` badge. The detail panel
  has a `machine` line with the machine, the session's status there and its tmux session.
- When a machine can't be reached, its rows keep their last status and the badge reads
  `⇄ mini offline`. Nothing is archived or relaunched until the machine answers again.
- If a mirror can't attach, its pane says why (the connection dropped, no tmux on the PATH, tmux
  refused the target) and tries again with backoff (1s, 2s, 4s, up to 30s). The row stays, and the
  board's log names the reason once. A mirror's pane never exits on its own: a pane whose command
  exits is left on a local shell, which triage would hand you as if it were the agent on the other
  machine.
- Restarting the board reconnects every mirror in its own row, keeping its name and priority, so
  the panes pick up any setting you changed.
- Archiving a mirror puts it away until its session stops running on its machine. On Gate Inbox
  v1.1.0, `ctrl+x` kills and archives a session, so it puts a mirror away too. Restoring the
  mirror from the archive brings it back.
- The board's log records each mirror the extension starts, reconnects, archives or sees put away,
  with the reason.

## Safety

- **Nothing listens on a port.** Everything goes over SSH with your key, so the extension can
  reach on a machine exactly what you could by SSH-ing in.
- **Nothing reaches an agent on another machine except what you type into its focused mirror.**
  The `machines` tool block declares no `activity_cutoff`, so the board refuses to queue messages,
  snippets or naming requests into a mirror. Anything queued there would skip the checks that
  machine's board makes on when an agent can safely read input. `skip_rename_directive` and
  `mcp = "none"` keep the board's naming request and its MCP server out of the pane. Don't add an
  `activity_cutoff` to that block.
- **Boards that mirror each other don't loop.** `machines sessions` leaves out mirror rows, so two
  machines can each list the other.
- **Commands sent to a machine are quoted for any login shell.** The command ssh hands the far
  side is run by your login shell there, which on a Mac is zsh. Every word is quoted so that sh,
  bash and zsh all read it back as written, and the tests check this against each shell.
- **What comes back from a machine is checked.** Session ids must be the board's hex ids before they
  are used in a tmux target, and a reply in another wire version is refused, with a message saying
  to run the same build on both machines.

## Limits

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
- Inside a focused mirror, `ctrl+q` is your board's own key and returns to your list. The status
  line drawn inside the pane belongs to the other machine's board, so ignore the keys it lists. If
  the tmux client on that machine is detached, the mirror attaches again half a second later.

## Develop

```bash
go vet ./...
go test -race ./...
```

The tests cover config, quoting (each quoted word is checked against a real `sh`), the wire
format, the reconcile plan, the reconnect loop, and full passes against a fake board.

It was also checked end to end with two boards on one machine. Board A ran the scripted agent from
Gate Inbox's `tools/demo/fake-agent.sh`, and board B mirrored A through a stand-in `ssh` that runs
the command locally with the bare environment a real non-interactive SSH session gets. That run
covered:

- the mirror showing A's pane
- B's triage handing over A's question, and an answer typed on B reaching A
- B refusing to queue a message into the mirror
- the machine dropping off and coming back
- the session ending on its machine and its mirror being archived

That run was done on Gate Inbox `main` (d3eb72d) and again on v1.1.0. The same flow then ran
between two Macs over real SSH, with the board on a MacBook mirroring a Mac mini whose login shell
is zsh. The question showed on the MacBook, an answer typed there reached the agent on the mini,
and the row went to finished. Restarting the board, reconnecting and archiving were checked on
v1.1.0 too. A real adopted pane has not been tried: the fake agent is not a CLI the board adopts,
so that case is covered by a test against a real tmux server instead.

## License

Apache-2.0, the same license as Gate Inbox. See [LICENSE](LICENSE).
