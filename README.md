# gate-inbox-machines

A [Gate Inbox](https://github.com/usestring/gate-inbox) extension that puts the agents running on
your other machines on your board, as ordinary rows. They sort into the same triage queue as your
local sessions. The preview shows the remote conversation and live dialog, and focusing a row
opens the remote terminal.

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

Every reply also says where that machine keeps `gate-inbox` and tmux, which tmux server its board
uses, and which build it runs. Your board uses those, so a machine needs only an SSH destination.

When you select a mirror, the board asks that machine for its recent conversation and current
dialog over SSH. The preview uses the same message boxes and dialog cards as local sessions.
Enter opens the remote terminal so you can answer there, including when triage hands you the row.

The extension uses Gate Inbox's public APIs for the rows and their lifecycle. The build applies
a patch to Gate Inbox v1.1.0 so the extension can supply conversations and dialogs to its renderer.

## Install

From a checkout of this repo, build this in place of the `gate-inbox` you run now:

```bash
./scripts/gate-inbox-go install ./cmd/gate-inbox
```

Your config, state and sessions carry over. The build script applies the preview patch to a
temporary copy of the pinned Go module.

Each other machine needs this build to answer `machines sessions` and `machines preview`. Its
board can keep running stock Gate Inbox. If that machine's PATH finds the stock binary first, set
`gate_inbox` for it to this build's path.

Each machine needs an SSH key login from the one you sit at that works without a prompt, because
the extension runs SSH with `BatchMode=yes`. `machines add` sets that up for you when it can (see
below).

## Add a machine

Start each other machine's board, then add the machine from the one you sit at, by its SSH
destination: a host from `~/.ssh/config`, a hostname, or `user@host`.

```bash
gate-inbox machines add mini.example.com
gate-inbox machines add --name box me@10.0.0.7
```

Or press `O` on the board to open the machines screen, then `a`.

Adding runs each step the board needs, in order: SSH without a prompt, `gate-inbox` and this
build's `machines` command there, tmux, the sessions reply, that machine's board running, and a
session a mirror can attach to. The machine is added once the steps up to the sessions reply pass.
A stopped board or no sessions yet doesn't stop it. A running board shows its sessions within a few
seconds, with no restart.

When SSH can't log in without a prompt, `machines add` at a terminal offers the fix and checks
again:

- no key on that machine: it runs `ssh-copy-id <destination>`, and you type that machine's
  password once
- an unknown host key: it connects once without `BatchMode`, so you can check the fingerprint and
  accept it

On the machines screen, the same problems offer `ctrl+f`, which opens a setup row on the board
running `machines add` in a pane. Its status is waiting, so triage hands it to you. Open it, type
what it asks, and press Enter when it's done. The row is archived once its pane ends.

The name defaults to the first part of the hostname the machine reports, such as `box-1` for
`Box-1.local`. `--name` sets your own. Names are lower case letters, digits, `-` and `_`, and start
with a letter. Each row's name starts with it, such as `box-1-upgrade-http-client`, so a short one
reads better.

Adding a machine changes nothing on that machine. SSH must already be on there: on a Mac, Remote
Login under System Settings → General → Sharing, and on Linux, the `sshd` service.

### What each machine reports

Every sessions reply carries what that machine says about itself:

- the full path of its own `gate-inbox`
- the tmux it found on its PATH, the one described under [Settings](#settings)
- the tmux server its board keeps sessions on: `tmux_socket` in its `config.toml`, then
  `GATE_INBOX_TMUX_SOCKET`, then tmux's default server
- its hostname, OS and this extension's version

The board saves it in `learned.json` in the extension's data folder, and polls, mirrors, previews and
spawns use it from then on. When a reply names a different tmux server than the one the board
asked about, the board asks again about that one before it starts or puts away any mirror. The
paths must be absolute and the socket a plain name, or the board ignores them.

A machine whose PATH has no `gate-inbox` is asked once more through your login shell there, which
is where most installers put it. Its reply gives the full path, and later polls use it directly.

`machines check` warns when the two machines run different versions of this extension.

### Manage machines

The machines screen (`O`) lists every machine with its state: online with its session count, board
stopped, offline since a time and why, or paused. Under it are suggestions to add (see below). Keys:

| Key | What it does |
|---|---|
| `a` | Add a machine. On a suggestion, the form starts with its destination |
| `enter` | Check the machine under the cursor, or add the suggestion |
| `c` | Check the machine and show each step |
| `space` | Pause or resume. A paused machine isn't polled, and its mirrors are put away until it resumes |
| `d` | Remove, after a second `d`. Its mirrors are put away. Its agents keep running |
| `s` | Start its board (see below) |
| `n` | Start a session there, as `N` on the list does |
| `r` | Rename. Its mirrors come back under the new name |
| `esc`, `q` | Close |

From a shell:

```bash
gate-inbox machines list                # every machine, where it's written, and what it reported
gate-inbox machines check               # walk every machine through the steps above
gate-inbox machines check mini          # just one
gate-inbox machines pause mini
gate-inbox machines resume mini
gate-inbox machines rename box-1 box
gate-inbox machines remove box
gate-inbox machines start-board mini
```

A failed check step ends with what to change, and `check` exits non-zero:

```text
mini: ssh OK
mini: gate-inbox FAIL (exit status 127: env: gate-inbox: No such file or directory); set gate_inbox to its full path
```

### Start a machine's board

The board on each machine has to run for its sessions to have a status. `s` on the machines screen,
or `machines start-board <machine>`, starts it in a detached tmux session named
`gate-inbox-board` on that machine's default tmux server, through your login shell there. Nothing
happens if its board is already running. See it with
`ssh -t <destination> tmux attach -t gate-inbox-board`.

A board started over SSH starts its agents the same way. On a Mac, that means Claude Code can't
read the login keychain (see [the Mac sign-in limit](#start-a-session-on-another-machine)).
Starting the board from that Mac's own desktop session avoids it.

### Suggestions

The machines screen suggests machines to add from two places, and leaves out any already on the
board:

- **Hosts in `~/.ssh/config`**, and the files it includes. Patterns such as `*.example.com` are
  left out. The board doesn't connect to these until you pick one, since that file often names
  servers that have nothing to do with this board.
- **The `discover` command**, if you set one. It runs on the machine you sit at and prints one
  destination per line. Blank lines, lines starting with `#`, and anything after the first word
  are skipped. The board checks each one when the screen opens and shows what it found, such as
  `ready, board running` or `needs your SSH key`.

The extension knows nothing about any network, so `discover` is how a VPN's peer list, an
inventory or a plain file becomes suggestions:

```toml
[extensions.machines]
# a file you keep yourself
discover = ["cat", "/Users/me/.config/gate-inbox/machines.txt"]

# Tailscale peers that are online
# discover = ["sh", "-c", "tailscale status --json | jq -r '.Peer[] | select(.Online) | .DNSName'"]
```

### Where machines are kept

Machines added with `machines add` or the machines screen live in `machines.json`, and pauses
for any machine live there too. It's in the extension's data folder,
`<config dir>/extensions/machines/`, next to `learned.json` and the mirrors' state. The board
never rewrites your `config.toml`, the same way Gate Inbox keeps its own Settings out of it.

You can still write machines in `config.toml`, one table per machine. The table's name is the
machine's name:

```toml
[extensions.machines.mini]
destination = "mini.example.com"   # an ssh destination; defaults to the table's name
```

A machine in `config.toml` wins over an added one with the same name, and the board reads it only
at startup. The machines screen and `machines list` say where each machine is written. To remove
or rename one from `config.toml`, edit its table. Pausing works on both.

### Settings

Most machines need only a destination. The PATH a non-interactive SSH command gets usually leaves
out Homebrew. So until a machine has reported its own paths, commands sent to it look for
`gate-inbox` and tmux on its PATH and then in `~/go/bin`, `~/.local/bin`, `/opt/homebrew/bin` and
`/usr/local/bin`. Those come after the machine's own PATH, so they never override it.

The first four keys go directly under `[extensions.machines]`, so no machine can be named `poll`,
`ssh`, `control_path` or `discover`. The per-machine keys go in a `config.toml` table, or with
`machines add` for `group` and `home`. A key you set wins over what the machine reports.

| Key | Default | What it does |
|---|---|---|
| `poll` | `"3s"` | How often each machine is asked for its sessions |
| `ssh` | `["ssh"]` | The ssh command and any options of your own, such as `["ssh", "-F", "/Users/me/.ssh/agents"]`. `ssh-copy-id` doesn't get these options |
| `control_path` | `/tmp/gate-inbox-machines-<uid>/%C` | Where ssh keeps the connection it shares between a machine's polls and its mirrors. `"none"` turns sharing off |
| `discover` | none | A command whose output becomes suggestions (see above) |
| `<name>.destination` | the table's name | The ssh destination: a name from `~/.ssh/config`, or `user@host` |
| `<name>.gate_inbox` | what the machine reports | This build on that machine |
| `<name>.tmux` | what the machine reports | tmux on that machine |
| `<name>.home` | none | `GATE_INBOX_HOME` of the board on that machine, if it uses one |
| `<name>.tmux_socket` | what the machine reports | That board's `tmux_socket` |
| `<name>.group` | top level | An existing group to file the machine's rows under. An extension cannot create a group, so make it first with `ctrl+alt+g` |

The build also supplies a `[tools.mirror]` block, which is the tool mirrors and setup rows launch
with. It shows in the new-session picker. Starting it from there only prints a note saying to pick
an agent CLI, and `machines sessions` never reports such a session. Uncheck it under Settings →
CLIs to hide it.

A machine's polls and mirrors share one SSH connection. It closes 10 minutes after the last of
them stops. The checks behind suggestions use a connection of their own that closes when they end.

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
- A reachable machine whose board stopped shows its sessions anyway, so an agent started with `N`
  while that board is down can still be opened from here. Its rows are pinned to `errored` with a
  `✕ mini board stopped` badge and a detail line saying there's no status until you start that
  machine's board. They stay out of triage like an offline machine's rows. Agent processes can
  keep running after their board quits, but their saved statuses cannot be trusted. So a row
  appears for each session that machine's tmux says a mirror can attach to right now, and no row
  is put away until the board is back. That machine's `machines sessions` tells whether its board
  runs by checking that the process whose PID is in its board's `manager.lock` is alive. It never
  takes that lock, which could make a board starting at the same moment take over from a live one.
  A reply from an older build without this check reads as running. When the board is back, the
  rows get their real status again.
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
- `O` on the list opens the machines screen.
- If scrolling leaves a mirror in tmux scroll mode, press `q` inside it to type again. On the
  list, `q` quits your board.
- `x` on the list, or `ctrl+x` inside a mirror once you confirm, closes the mirror and files its
  row in the archive. The agent on the other machine keeps running. The confirm prompt is Gate
  Inbox's own and still reads "kill <row>?", about freeing its RAM; for a mirror only the local
  row and its ssh connection close. To get the mirror back, open the archived view from the `:`
  menu, press `u` on its row, then confirm with `y` or Enter.

## Start a session on another machine

Press `N` on the list to open the machine form immediately. With no machines yet, `N` opens the
machines screen instead. With one configured machine,
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

On a Mac, Claude Code keeps its sign-in in the login keychain, and nothing started from an SSH
login can read it, even after that SSH connection closes. An agent `N` starts runs in a pane of
that machine's agent tmux server, the board's `tmux_socket` or tmux's default server, so it
can sign in only if that server was started outside SSH. If the server isn't running, the spawn
starts it inside SSH, and Claude says "Not logged in" and suggests `security unlock-keychain`.
There are two ways around this:

- Keep that server running from the machine's desktop session, for example with a login item
  that runs `tmux [-L <socket>] start-server \; set -s exit-empty off`. Every agent inherits that
  server's environment, so start it with only what agents should see.
- Run `claude setup-token` on that machine once, and export the token as
  `CLAUDE_CODE_OAUTH_TOKEN` in its login shell.

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
  to run the same build on both machines. The paths a machine reports must be absolute, with no
  line breaks, and are quoted like every other word. Its tmux socket must be a plain name.
- **Discovery connects only where you asked it to.** Hosts from `~/.ssh/config` are listed, not
  contacted, until you pick one. The `discover` command is one you set yourself, and it runs on
  the machine you sit at.
- **Fixes that need a password run where you can see them.** `ssh-copy-id` and the host-key prompt
  run at your terminal or in a setup row you open. The board's own SSH never prompts.

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
- The preview shows recent user-facing messages and the current dialog. Open the terminal
  for tool output or to answer a prompt.
- Priority tiers set on the other machine are not carried over. Set a mirror's priority on your
  own board.
- On a narrow list the badge can be cut off. The detail line always shows the machine.

## Develop

```bash
./scripts/gate-inbox-go vet ./...
./scripts/gate-inbox-go test -race ./...
```

The tests cover config, quoting (each quoted word is checked against real `sh`, `bash` and
`zsh`, including `start-board`'s command), the PATH a command gets on the other machine,
`machines check`, the wire format and what a machine reports about itself, adding, pausing,
renaming and removing machines, two processes sharing the machine files, suggestions from
`~/.ssh/config` and a `discover` command, the machines screen and its add form, setup rows, the
reconcile plan (a stopped board included), the reconnect loop, the launch guard, and full passes
against a fake board.

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
v1.1.0 too.

The same two Macs then ran real Claude Code 2.1.327 and Codex 0.169.0 on a stock v1.1.0 board on
the mini:

- `N` started both, with Codex found only through the login shell
- a Claude permission dialog showed as waiting on the MacBook, and was approved from the mirror
- one Esc from the mirror declined another
- `ctrl+x` archived the mirror and left the agent on the mini running, and restoring followed it
  again
- Codex went from working to finished on both boards
- restarting either board brought the mirrors back
- a Claude started by hand, which the mini's board adopted from another tmux server, was listed as
  `not mirrored`

Machine management was then checked end to end on v1.1.0 with two boards on one Mac, each with
its own home and tmux servers, joined by the same kind of stand-in `ssh`. The far board kept its
sessions on a named tmux socket and its `gate-inbox` where only its `.zprofile` put it on PATH, and
the near board's config named neither. That run covered:

- `machines add` finding `gate-inbox` through the login shell and learning the far tmux path and
  socket, then mirrors attaching with no `tmux` or `tmux_socket` setting
- refusing a destination that can't be reached
- an agent started while the far board was stopped showing as a row with no status, then getting
  its real status once `s` on the machines screen started that board
- the add form stopping at a missing key, `ctrl+f` opening a setup row that triage offered, and the
  row being archived once its pane finished
- rename, pause and resume on the machines screen, and remove and add from a shell, each taking
  effect on the running board within a pass

Shift+enter through two tmux layers is still untested in a real terminal.

## License

Apache-2.0, the same license as Gate Inbox. See [LICENSE](LICENSE).
