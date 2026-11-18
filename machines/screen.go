package machines

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

// The extension's own screens.
const (
	machinesScreen = "machines"
	addScreen      = "machine_add"
	renameScreen   = "machine_rename"
)

// Actions on the machines screen.
const (
	actionAdd        = "add"
	actionCheck      = "check"
	actionPause      = "pause"
	actionRemove     = "remove"
	actionStartBoard = "start_board"
	actionNewSession = "new_session"
	actionRename     = "rename"
	actionFix        = "fix_in_pane"
)

func machinesKeys() []extension.KeyBinding {
	return []extension.KeyBinding{
		{Screen: machinesScreen, Action: actionAdd, Keys: []string{"a"}, Label: "add a machine"},
		{Screen: machinesScreen, Action: actionCheck, Keys: []string{"c"}, Label: "check the machine"},
		{Screen: machinesScreen, Action: actionPause, Keys: []string{"space"}, Label: "pause or resume"},
		{Screen: machinesScreen, Action: actionRemove, Keys: []string{"d"}, Label: "remove (press twice)"},
		{Screen: machinesScreen, Action: actionStartBoard, Keys: []string{"s"}, Label: "start its board"},
		{Screen: machinesScreen, Action: actionNewSession, Keys: []string{"n"}, Label: "new session there"},
		{Screen: machinesScreen, Action: actionRename, Keys: []string{"r"}, Label: "rename"},
		{Screen: machinesScreen, Action: extension.ActionClose, Keys: []string{"esc", "q"}, Label: "close"},
		{Screen: addScreen, Action: actionFix, Keys: []string{"ctrl+f"}, Label: "fix it in a pane"},
		{Screen: addScreen, Action: extension.ActionClose, Keys: []string{"esc"}, Label: "back"},
		{Screen: renameScreen, Action: extension.ActionClose, Keys: []string{"esc"}, Label: "back"},
	}
}

// openMachines shows the machines screen.
func (e *Extension) openMachines(ctx context.Context) {
	v := &machinesView{e: e, ctx: ctx, loading: true}
	v.handle = e.ui.Open(machinesScreen, v)
	if v.handle == nil {
		return
	}
	v.stop = e.status.listen(v.handle.Refresh)
	go v.load()
}

// machinesView is the machines screen: every machine with its state, and
// the suggestions to add.
type machinesView struct {
	e      *Extension
	ctx    context.Context
	handle extension.ViewHandle
	stop   func()

	mu          sync.Mutex
	cursor      int
	suggestions []suggestion
	loading     bool
	problem     string
	note        []extension.Line
	confirm     string
	busy        bool
}

func (v *machinesView) load() {
	found, problem := v.e.suggestions(v.ctx)
	v.mu.Lock()
	v.suggestions, v.problem, v.loading = found, problem, false
	v.mu.Unlock()
	v.handle.Refresh()
}

func (v *machinesView) Title() string { return "Machines" }

// row is one line the cursor can land on: a machine or a suggestion.
type row struct {
	machine    *host
	suggestion *suggestion
}

func (v *machinesView) rowsLocked() []row {
	var rows []row
	for _, h := range v.e.machines().all() {
		rows = append(rows, row{machine: &h})
	}
	for i := range v.suggestions {
		rows = append(rows, row{suggestion: &v.suggestions[i]})
	}
	return rows
}

func (v *machinesView) Render(width, height int) []extension.Line {
	v.mu.Lock()
	defer v.mu.Unlock()
	rows := v.rowsLocked()
	v.cursor = min(v.cursor, max(len(rows)-1, 0))
	var lines []extension.Line
	cursorLine := 0
	machines := v.e.machines().all()
	nameWidth := 4
	for _, h := range machines {
		nameWidth = max(nameWidth, len(h.Name))
	}
	if len(machines) == 0 {
		lines = append(lines, extension.Line{{Text: "No machines yet. Press a to add one by its SSH destination: a host from ~/.ssh/config, a hostname, or user@host."}})
	}
	for i, h := range machines {
		status, tone := v.e.machineStatus(h)
		if i == v.cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, extension.Line{
			{Text: marker(i == v.cursor), Tone: extension.ToneAccent},
			{Text: pad(h.Name, nameWidth) + "  ", Bold: true},
			{Text: status, Tone: tone},
			{Text: "  " + h.Destination + " · " + h.Source},
		})
	}
	lines = append(lines, extension.Line{}, extension.Line{{Text: "Suggestions", Bold: true}})
	switch {
	case v.loading:
		lines = append(lines, extension.Line{{Text: "  looking in ~/.ssh/config and the discover command..."}})
	case len(v.suggestions) == 0:
		lines = append(lines, extension.Line{{Text: "  none: hosts in ~/.ssh/config show here, and so does what the discover setting's command prints"}})
	}
	for i, s := range v.suggestions {
		index := len(machines) + i
		if index == v.cursor {
			cursorLine = len(lines)
		}
		status, tone := "not checked; press a to check and add it", extension.ToneMuted
		if s.Status != "" {
			status, tone = s.Status, extension.ToneWarn
			if s.Ready {
				tone = extension.ToneGood
			}
		}
		lines = append(lines, extension.Line{
			{Text: marker(index == v.cursor), Tone: extension.ToneAccent},
			{Text: s.Destination + "  ", Bold: true},
			{Text: status, Tone: tone},
			{Text: "  from " + s.From},
		})
	}
	var tail []extension.Line
	for _, problem := range []string{v.e.machines().issue(), v.problem} {
		if problem != "" {
			tail = append(tail, extension.Line{{Text: problem, Tone: extension.ToneBad}})
		}
	}
	if v.confirm != "" {
		tail = append(tail, extension.Line{{Text: "Press d again to remove " + v.confirm + ". Its mirrors are put away; its agents keep running.", Tone: extension.ToneWarn}})
	}
	if len(v.note) > 0 {
		tail = append(append(tail, extension.Line{}), v.note...)
	}
	tail = wrapLines(tail, width)
	lines = append(lines, tail...)
	// Keep the cursor's line on screen when the list runs past the card.
	if height > 0 && len(lines) > height && cursorLine >= height-len(tail) {
		skip := min(cursorLine-(height-len(tail))+1, len(lines)-height)
		lines = lines[skip:]
	}
	return lines
}

// wrapLines breaks each one-span line longer than width at spaces, so a
// long result or error is read whole rather than cut at the card's edge.
func wrapLines(lines []extension.Line, width int) []extension.Line {
	if width <= 0 {
		return lines
	}
	var out []extension.Line
	for _, line := range lines {
		if len(line) != 1 || line.Width() <= width {
			out = append(out, line)
			continue
		}
		span := line[0]
		rest := []rune(span.Text)
		for len(rest) > width {
			cut := width
			for i := width; i > width/2; i-- {
				if rest[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, extension.Line{{Text: string(rest[:cut]), Tone: span.Tone, Bold: span.Bold}})
			rest = []rune(strings.TrimLeft(string(rest[cut:]), " "))
		}
		out = append(out, extension.Line{{Text: string(rest), Tone: span.Tone, Bold: span.Bold}})
	}
	return out
}

func marker(on bool) string {
	if on {
		return "› "
	}
	return "  "
}

func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// machineStatus is a machine's state in a few words, from the last pass.
func (e *Extension) machineStatus(h host) (string, extension.Tone) {
	if h.Paused {
		return "paused", extension.ToneMuted
	}
	st, ok := e.status.get(h.Name)
	if !ok {
		return "not polled yet", extension.ToneMuted
	}
	if st.Err != "" {
		return "offline since " + st.Since.Format("15:04") + ": " + lastLine(st.Err), extension.ToneBad
	}
	parts := []string{"online"}
	tone := extension.ToneGood
	if !st.BoardRunning {
		parts, tone = []string{"board stopped, no status"}, extension.ToneWarn
	}
	parts = append(parts, plural(st.Mirrored, "session"))
	if st.NotMirrored > 0 {
		parts = append(parts, fmt.Sprintf("%d not mirrored", st.NotMirrored))
	}
	if h.About != nil {
		if h.About.OS != "" {
			parts = append(parts, h.About.OS)
		}
		if v := h.About.Version; v != "" && v != Version {
			parts = append(parts, "machines "+v+", this one "+Version)
			tone = extension.ToneWarn
		}
	}
	return strings.Join(parts, " · "), tone
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (v *machinesView) Key(k extension.ViewKey) bool {
	v.mu.Lock()
	rows := v.rowsLocked()
	if k.Action != actionRemove {
		v.confirm = ""
	}
	var current row
	if v.cursor < len(rows) {
		current = rows[v.cursor]
	}
	switch k.Key {
	case "up", "k":
		v.cursor = max(v.cursor-1, 0)
		v.mu.Unlock()
		return false
	case "down", "j":
		v.cursor = min(v.cursor+1, max(len(rows)-1, 0))
		v.mu.Unlock()
		return false
	}
	v.mu.Unlock()
	action := k.Action
	if action == "" && k.Key == "enter" {
		action = actionCheck
		if current.suggestion != nil {
			action = actionAdd
		}
	}
	switch action {
	case actionAdd:
		destination := ""
		if current.suggestion != nil {
			destination = current.suggestion.Destination
		}
		v.e.openAdd(v.ctx, destination)
	case actionCheck:
		if current.machine != nil {
			v.run("checking "+current.machine.Name+"...", func() []extension.Line {
				var out bytes.Buffer
				result := v.e.probe(v.ctx, *current.machine)
				result.print(&out, current.machine.Name)
				if result.Snapshot != nil && result.Snapshot.About != nil {
					_, _ = v.e.machines().learn(current.machine.Name, current.machine.Destination, *result.Snapshot.About)
				}
				return textLines(out.String(), result.failed())
			})
		}
	case actionPause:
		if current.machine != nil {
			h := *current.machine
			done := "Resumed " + h.Name + "."
			if !h.Paused {
				done = "Paused " + h.Name + ". Its mirrors are put away until you resume it."
			}
			v.say(v.e.pauseMachine(h.Name, !h.Paused), done)
			v.e.wakeLoop(h.Name)
		}
	case actionRemove:
		if current.machine != nil {
			v.mu.Lock()
			confirmed := v.confirm == current.machine.Name
			v.confirm = ""
			if !confirmed {
				v.confirm = current.machine.Name
			}
			v.mu.Unlock()
			if confirmed {
				err := v.e.removeMachine(current.machine.Name)
				v.say(err, "Removed "+current.machine.Name+".")
				v.e.wakeLoop(current.machine.Name)
			}
		}
	case actionStartBoard:
		if current.machine != nil {
			h := *current.machine
			v.run("starting "+h.Name+"'s board...", func() []extension.Line {
				msg, err := v.e.startBoard(v.ctx, h)
				v.e.wakeLoop(h.Name)
				if err != nil {
					return []extension.Line{{{Text: err.Error(), Tone: extension.ToneBad}}}
				}
				return []extension.Line{{{Text: msg + ".", Tone: extension.ToneGood}}}
			})
		}
	case actionNewSession:
		if current.machine != nil && !current.machine.Paused {
			v.e.openSpawn(v.ctx, current.machine.Name)
		}
	case actionRename:
		if current.machine != nil {
			if current.machine.Source != sourceAdded {
				v.say(errors.New(current.machine.Name+" is written in config.toml; rename its table there"), "")
			} else {
				v.e.openRename(v.ctx, current.machine.Name)
			}
		}
	}
	v.handle.Refresh()
	return false
}

// run does slow work off the board's loop, showing what it is doing and
// then what it found.
func (v *machinesView) run(doing string, work func() []extension.Line) {
	v.mu.Lock()
	if v.busy {
		v.mu.Unlock()
		return
	}
	v.busy = true
	v.note = []extension.Line{{{Text: doing}}}
	v.mu.Unlock()
	go func() {
		lines := work()
		v.mu.Lock()
		v.busy = false
		v.note = lines
		v.mu.Unlock()
		v.handle.Refresh()
	}()
}

func (v *machinesView) say(err error, done string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		v.note = []extension.Line{{{Text: err.Error(), Tone: extension.ToneBad}}}
		return
	}
	v.note = []extension.Line{{{Text: done, Tone: extension.ToneGood}}}
}

func (v *machinesView) Closed(extension.CloseReason) {
	if v.stop != nil {
		v.stop()
	}
}

// textLines is command output as view lines, each FAIL or WARN toned.
func textLines(text string, failed bool) []extension.Line {
	var lines []extension.Line
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		tone := extension.ToneMuted
		switch {
		case strings.Contains(line, " FAIL "):
			tone = extension.ToneBad
		case strings.Contains(line, " WARN "):
			tone = extension.ToneWarn
		}
		lines = append(lines, extension.Line{{Text: line, Tone: tone}})
	}
	if !failed {
		lines = append(lines, extension.Line{{Text: "Every check passed.", Tone: extension.ToneGood}})
	}
	return lines
}

// wakeLoop asks the poll loop to run a pass now.
func (e *Extension) wakeLoop(name string) {
	select {
	case e.wake <- name:
	default:
	}
}

// openAdd shows the add form, with destination filled in when one was
// picked from the suggestions.
func (e *Extension) openAdd(ctx context.Context, destination string) {
	f := &addForm{e: e, ctx: ctx, destination: destination}
	f.handle = e.ui.Open(addScreen, f)
}

// addForm checks a machine and adds it, from the machines screen.
type addForm struct {
	e           *Extension
	ctx         context.Context
	handle      extension.ViewHandle
	destination string

	mu      sync.Mutex
	lines   []extension.Line
	busy    bool
	pending *addOptions
	// leave is set once the form opened a setup row, so closing it does
	// not put the machines screen back over the row the operator wants.
	leave bool
}

func (f *addForm) Title() string { return "Add a machine" }

func (f *addForm) Fields() []extension.Field {
	return []extension.Field{
		{ID: "destination", Label: "Destination", Value: f.destination, Placeholder: "a host from ~/.ssh/config, a hostname, or user@host"},
		{ID: "name", Label: "Name", Placeholder: "from its hostname"},
		{ID: "group", Label: "Group", Placeholder: "top level; an existing group on this board"},
	}
}

func (f *addForm) Render(width, _ int) []extension.Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lines) == 0 {
		return wrapLines([]extension.Line{{{Text: "The board checks the machine over ssh, then adds it. It needs this build installed there, and a key login that works without a prompt."}}}, width)
	}
	return wrapLines(f.lines, width)
}

func (f *addForm) Key(k extension.ViewKey) bool {
	switch k.Action {
	case extension.ActionSubmit:
		opts := addOptions{Destination: k.Values["destination"], Name: k.Values["name"], Group: k.Values["group"]}
		f.mu.Lock()
		if f.busy {
			f.mu.Unlock()
			return false
		}
		f.busy, f.pending = true, nil
		f.lines = []extension.Line{{{Text: "Checking " + strings.TrimSpace(opts.Destination) + "..."}}}
		f.mu.Unlock()
		go f.add(opts)
	case actionFix:
		f.mu.Lock()
		pending := f.pending
		f.mu.Unlock()
		if pending == nil {
			return false
		}
		name, err := f.e.openSetupRow(f.ctx, *pending)
		if err != nil {
			f.setLines(append(f.snapshotLines(), extension.Line{{Text: err.Error(), Tone: extension.ToneBad}}))
			return false
		}
		f.e.ui.Notify("Opened " + name + ": open it from the list to finish adding " + pending.Destination)
		f.mu.Lock()
		f.leave = true
		f.mu.Unlock()
		return true
	}
	return false
}

func (f *addForm) add(opts addOptions) {
	var out bytes.Buffer
	h, err := f.e.addMachine(f.ctx, opts, &out, nil)
	lines := textLines(out.String(), true)
	f.mu.Lock()
	f.busy = false
	f.mu.Unlock()
	var needs errNeedsTerminal
	switch {
	case errors.As(err, &needs):
		what := "your SSH key needs installing there, which takes that machine's password"
		if needs.Fix == fixHostKey {
			what = "its host key needs accepting"
		}
		lines = append(lines, extension.Line{{Text: "To finish, " + what + ". Press ctrl+f to do it in a pane on this board.", Tone: extension.ToneWarn}})
		f.mu.Lock()
		f.pending = &opts
		f.mu.Unlock()
	case err != nil:
		lines = append(lines, extension.Line{{Text: err.Error(), Tone: extension.ToneBad}})
	default:
		f.e.ui.Notify("Added " + h.Name + " (" + h.Destination + ")")
		f.e.wakeLoop(h.Name)
		f.handle.Close()
		return
	}
	f.setLines(lines)
}

func (f *addForm) setLines(lines []extension.Line) {
	f.mu.Lock()
	f.lines = lines
	f.mu.Unlock()
	f.handle.Refresh()
}

func (f *addForm) snapshotLines() []extension.Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]extension.Line(nil), f.lines...)
}

// Closed puts the machines screen back, unless the form handed the
// operator a setup row to go to.
func (f *addForm) Closed(extension.CloseReason) {
	f.mu.Lock()
	leave := f.leave
	f.mu.Unlock()
	if !leave {
		f.e.openMachines(f.ctx)
	}
}

// openSetupRow starts a setup row: a pane running `machines add` for
// opts, where the operator types what the add needs. It is pinned to
// waiting, so triage hands it over.
func (e *Extension) openSetupRow(ctx context.Context, opts addOptions) (string, error) {
	e.boardMu.Lock()
	board := e.board
	e.boardMu.Unlock()
	if board == nil {
		return "", errors.New("the board is not running its loop yet; try again in a moment")
	}
	args := []string{setupFlag}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	if opts.Group != "" {
		args = append(args, "--group", opts.Group)
	}
	if opts.Home != "" {
		args = append(args, "--home", opts.Home)
	}
	args = append(args, "--", strings.TrimSpace(opts.Destination))
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := board.Launch(ctx, extension.LaunchRequest{
		Tool: toolName,
		Name: "add-" + suggestName("", opts.Destination, func(string) bool { return false }),
		Role: setupRoleName,
		Args: args,
	})
	if err != nil {
		return "", fmt.Errorf("could not open a setup row: %w", err)
	}
	// Triage hands a waiting row over. A pin that fails leaves the row
	// idle, which is still on the list to open.
	_ = board.PinStatus(ctx, info.ID, "waiting")
	return info.Name, nil
}

// openRename shows the rename form for an added machine.
func (e *Extension) openRename(ctx context.Context, name string) {
	f := &renameForm{e: e, ctx: ctx, from: name}
	f.handle = e.ui.Open(renameScreen, f)
}

type renameForm struct {
	e      *Extension
	ctx    context.Context
	handle extension.ViewHandle
	from   string
	mu     sync.Mutex
	err    string
}

func (f *renameForm) Title() string { return "Rename " + f.from }

func (f *renameForm) Fields() []extension.Field {
	return []extension.Field{{ID: "name", Label: "New name", Value: f.from}}
}

func (f *renameForm) Render(int, int) []extension.Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := []extension.Line{{{Text: "Its mirrors come back under the new name."}}}
	if f.err != "" {
		lines = append(lines, extension.Line{{Text: f.err, Tone: extension.ToneBad}})
	}
	return lines
}

func (f *renameForm) Key(k extension.ViewKey) bool {
	if k.Action != extension.ActionSubmit {
		return false
	}
	to := strings.TrimSpace(k.Values["name"])
	if to == f.from {
		return true
	}
	if err := f.e.renameMachine(f.from, to); err != nil {
		f.mu.Lock()
		f.err = err.Error()
		f.mu.Unlock()
		return false
	}
	f.e.ui.Notify("Renamed " + f.from + " to " + to)
	f.e.wakeLoop(to)
	return true
}

func (f *renameForm) Closed(extension.CloseReason) { f.e.openMachines(f.ctx) }
