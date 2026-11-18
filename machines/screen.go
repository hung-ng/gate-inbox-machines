package machines

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/usestring/gate-inbox/extension"
)

// The extension's own screens.
const (
	machinesScreen = "machines"
	addScreen      = "machine_add"
)

// Actions on the machines screen.
const (
	actionAdd        = "add"
	actionCheck      = "check"
	actionPause      = "pause"
	actionRemove     = "remove"
	actionNewSession = "new_session"
)

func machinesKeys() []extension.KeyBinding {
	return []extension.KeyBinding{
		{Screen: machinesScreen, Action: actionAdd, Keys: []string{"a"}, Label: "add a machine"},
		{Screen: machinesScreen, Action: actionCheck, Keys: []string{"c"}, Label: "check the machine"},
		{Screen: machinesScreen, Action: actionPause, Keys: []string{"space"}, Label: "pause or resume"},
		{Screen: machinesScreen, Action: actionRemove, Keys: []string{"d"}, Label: "remove (press twice)"},
		{Screen: machinesScreen, Action: actionNewSession, Keys: []string{"n"}, Label: "new session there"},
		{Screen: machinesScreen, Action: extension.ActionClose, Keys: []string{"esc", "q"}, Label: "close"},
		{Screen: addScreen, Action: extension.ActionClose, Keys: []string{"esc"}, Label: "back"},
	}
}

// openMachines shows the machines screen.
func (e *Extension) openMachines(ctx context.Context) {
	v := &machinesView{e: e, ctx: ctx, loading: true}
	v.mu.Lock()
	v.handle = e.ui.Open(machinesScreen, v)
	v.mu.Unlock()
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

func (v *machinesView) load(ctx context.Context) {
	found, problem := v.e.suggestions(ctx)
	v.mu.Lock()
	v.suggestions, v.problem, v.loading = found, problem, false
	v.mu.Unlock()
	if ctx.Err() == nil {
		v.handle.Refresh()
	}
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
	// The board may decline Open without calling Closed.
	if v.stop == nil && v.handle != nil {
		ctx, cancel := context.WithCancel(v.ctx)
		unlisten := v.e.status.listen(v.handle.Refresh)
		v.stop = func() {
			cancel()
			unlisten()
		}
		go v.load(ctx)
	}
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
			{Text: "  " + h.Destination},
		})
	}
	lines = append(lines, extension.Line{}, extension.Line{{Text: "Suggestions", Bold: true}})
	switch {
	case v.loading:
		lines = append(lines, extension.Line{{Text: "  looking in ~/.ssh/config and the discover command..."}})
	case len(v.suggestions) == 0:
		lines = append(lines, extension.Line{{Text: "  None yet. Hosts in ~/.ssh/config show here, and so does what the discover setting's command prints."}})
	}
	for i, s := range v.suggestions {
		index := len(machines) + i
		if index == v.cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, extension.Line{
			{Text: marker(index == v.cursor), Tone: extension.ToneAccent},
			{Text: s.Destination + "  ", Bold: true},
			{Text: "from " + s.From + " · enter to check and add", Tone: extension.ToneMuted},
		})
	}
	var tail []extension.Line
	for _, problem := range []string{v.e.machines().issue(), v.problem} {
		if problem != "" {
			tail = append(tail, extension.Line{{Text: problem, Tone: extension.ToneBad}})
		}
	}
	if v.confirm != "" {
		tail = append(tail, extension.Line{{Text: "Press d again to remove " + v.confirm + ". The board puts its mirrors away, and its agents keep running.", Tone: extension.ToneWarn}})
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
		switch v := h.About.Version; {
		case v == "":
		case v != Version:
			parts = append(parts, "machines "+v+", this one "+Version)
			tone = extension.ToneWarn
		default:
			parts = append(parts, "machines "+v)
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
			h := *current.machine
			v.run("checking "+h.Name+"...", func() []extension.Line {
				var out bytes.Buffer
				result := v.e.probe(v.ctx, h)
				result.print(&out, h.Name)
				if result.Snapshot != nil && result.Snapshot.About != nil {
					_, _ = v.e.machines().learn(h.Name, h.Destination, *result.Snapshot.About)
				}
				return textLines(out.String(), result.failed())
			})
		}
	case actionPause:
		if current.machine != nil {
			h := *current.machine
			done := "Resumed " + h.Name + "."
			if !h.Paused {
				done = "Paused " + h.Name + ". The board keeps its mirrors and stops polling it."
			}
			v.run("saving...", func() []extension.Line {
				err := v.e.pauseMachine(h.Name, !h.Paused)
				v.e.wakeLoop(h.Name)
				return outcome(err, done)
			})
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
				name := current.machine.Name
				v.run("removing "+name+"...", func() []extension.Line {
					err := v.e.removeMachine(name)
					v.e.wakeLoop(name)
					return outcome(err, "Removed "+name+".")
				})
			}
		}
	case actionNewSession:
		if current.machine != nil && !current.machine.Paused {
			v.e.openSpawn(v.ctx, current.machine.Name)
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

// outcome is an action's result as a note: its error, or what it did.
func outcome(err error, done string) []extension.Line {
	if err != nil {
		return []extension.Line{{{Text: err.Error(), Tone: extension.ToneBad}}}
	}
	return []extension.Line{{{Text: done, Tone: extension.ToneGood}}}
}

func (v *machinesView) Closed(extension.CloseReason) {
	v.mu.Lock()
	defer v.mu.Unlock()
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

	mu    sync.Mutex
	lines []extension.Line
	busy  bool
}

func (f *addForm) Title() string { return "Add a machine" }

func (f *addForm) Fields() []extension.Field {
	return []extension.Field{
		{ID: "destination", Label: "Destination", Value: f.destination, Placeholder: "a host from ~/.ssh/config, a hostname, or user@host"},
		{ID: "name", Label: "Name", Placeholder: "from its hostname"},
		{ID: "group", Label: "Group", Placeholder: "top level; an existing group on this board"},
		{ID: "gate_inbox", Label: "gate-inbox path", Placeholder: "found on its PATH or through its login shell"},
		{ID: "home", Label: "Board home", Placeholder: "its default; GATE_INBOX_HOME if its board uses one"},
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
		opts := addOptions{Destination: k.Values["destination"], Name: k.Values["name"], Group: k.Values["group"],
			GateInbox: k.Values["gate_inbox"], Home: k.Values["home"]}
		f.mu.Lock()
		if f.busy {
			f.mu.Unlock()
			return false
		}
		f.busy = true
		f.lines = []extension.Line{{{Text: "Checking " + strings.TrimSpace(opts.Destination) + "..."}}}
		f.mu.Unlock()
		go f.add(opts)

	}
	return false
}

func (f *addForm) add(opts addOptions) {
	var out bytes.Buffer
	h, err := f.e.addMachine(f.ctx, opts, &out)
	lines := textLines(out.String(), true)
	f.mu.Lock()
	f.busy = false
	f.mu.Unlock()
	if err != nil {
		lines = append(lines, extension.Line{{Text: err.Error(), Tone: extension.ToneBad}})
	} else {
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

// Closed puts the machines screen back unless another view took its place.
func (f *addForm) Closed(reason extension.CloseReason) {
	if reason != extension.CloseReplaced {
		f.e.openMachines(f.ctx)
	}
}
