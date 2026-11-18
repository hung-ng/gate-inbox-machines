package machines

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/usestring/gate-inbox/extension/cmdline"
)

// addOptions is what `machines add` and the machines screen's add form
// take.
type addOptions struct {
	Destination string
	Name        string
	Group       string
	Home        string
	// Force adds the machine even when a step that keeps it from working
	// failed, for one that is set up later.
	Force bool
}

// terminal is the operator at a terminal, for the fixes that need one
// there: a password for ssh-copy-id, a host key to accept. Nil is no
// terminal, as on the machines screen, which opens a setup row instead.
type terminal struct {
	in  *bufio.Reader
	out io.Writer
	// run runs a command on this terminal and returns its exit code.
	run attachRunner
}

func (t *terminal) ask(question string) bool {
	fmt.Fprintf(t.out, "%s [Y/n] ", question)
	line, err := t.in.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "" || answer == "y" || answer == "yes"
}

// errNeedsTerminal is an add that stopped at a step only a terminal can fix.
type errNeedsTerminal struct {
	Fix  string
	Step checkStep
}

func (e errNeedsTerminal) Error() string {
	return fmt.Sprintf("%s: %v; %s", e.Step.Name, e.Step.Err, e.Step.Fix)
}

// validDestination refuses a destination ssh would read as something else.
func validDestination(d string) error {
	switch {
	case d == "":
		return errors.New("name the machine's ssh destination: a host from ~/.ssh/config, a hostname, or user@host")
	case strings.HasPrefix(d, "-"):
		return fmt.Errorf("destination %q starts with '-'", d)
	case strings.ContainsAny(d, " \t\x00\n\r"):
		return fmt.Errorf("destination %q holds a space or a line break", d)
	}
	return nil
}

// addMachine checks a machine by its destination and saves it, under the
// name given or one made from its hostname. Each step it ran is written to
// out. With a terminal it offers the ssh fixes that need one, then checks
// again; without one it stops at them with errNeedsTerminal.
func (e *Extension) addMachine(ctx context.Context, opts addOptions, out io.Writer, term *terminal) (host, error) {
	opts.Destination = strings.TrimSpace(opts.Destination)
	opts.Name = strings.TrimSpace(opts.Name)
	if err := validDestination(opts.Destination); err != nil {
		return host{}, err
	}
	reg := e.machines()
	if reg.dir == "" {
		return host{}, errNoStore
	}
	existing := reg.all()
	for _, h := range existing {
		if h.Destination == opts.Destination {
			return host{}, fmt.Errorf("%s is already on this board as %s", opts.Destination, h.Name)
		}
		if opts.Name != "" && h.Name == opts.Name {
			return host{}, fmt.Errorf("a machine named %s is already on this board", opts.Name)
		}
	}
	if opts.Name != "" {
		if err := checkName(opts.Name); err != nil {
			return host{}, err
		}
	}
	settings := hostSettings{Destination: opts.Destination, Group: opts.Group, Home: opts.Home}
	// The probe runs under a stand-in name; the real one may come from the
	// hostname the machine reports.
	probeHost, err := resolveHost("new", settings)
	if err != nil {
		return host{}, err
	}
	if err := ensureControlDir(e.cfg.ControlPath); err != nil {
		return host{}, err
	}
	var result probeResult
	for tries := 0; ; tries++ {
		result = e.probe(ctx, probeHost)
		result.print(out, opts.Destination)
		blocked := result.blocking()
		// A host key is checked before the login, so a machine can need
		// both fixes, one after the other.
		if blocked == nil || blocked.Fixable == "" || tries == 2 {
			break
		}
		if term == nil {
			if !opts.Force {
				return host{}, errNeedsTerminal{Fix: blocked.Fixable, Step: *blocked}
			}
			break
		}
		if !e.fixAtTerminal(ctx, term, opts.Destination, blocked.Fixable) {
			break
		}
	}
	if blocked := result.blocking(); blocked != nil && !opts.Force {
		return host{}, fmt.Errorf("%s was not added: %s failed; fix that and add it again, or pass --force to add it anyway", opts.Destination, blocked.Name)
	}
	name := opts.Name
	if name == "" {
		hostname := ""
		if result.Snapshot != nil && result.Snapshot.About != nil {
			hostname = result.Snapshot.About.Hostname
		}
		taken := func(n string) bool {
			return slices.ContainsFunc(existing, func(h host) bool { return h.Name == n })
		}
		name = suggestName(hostname, opts.Destination, taken)
	}
	err = reg.change(func(store *storeFile, base []host) error {
		for _, h := range base {
			if h.Name == name {
				return fmt.Errorf("config.toml already has a machine named %s", name)
			}
		}
		if _, ok := store.Machines[name]; ok {
			return fmt.Errorf("a machine named %s is already on this board", name)
		}
		for other, s := range store.Machines {
			if s.Destination == opts.Destination {
				return fmt.Errorf("%s is already on this board as %s", opts.Destination, other)
			}
		}
		store.Machines[name] = settings
		return nil
	})
	if err != nil {
		return host{}, err
	}
	if result.Snapshot != nil && result.Snapshot.About != nil {
		if _, err := reg.learn(name, opts.Destination, *result.Snapshot.About); err != nil {
			fmt.Fprintf(out, "could not save what %s said about itself: %v\n", name, err)
		}
	}
	h, _ := reg.host(name)
	return h, nil
}

// fixAtTerminal runs the fix a failed ssh step needs, on the operator's
// terminal, once they agree to it. It says whether it ran.
func (e *Extension) fixAtTerminal(ctx context.Context, term *terminal, destination, fix string) bool {
	var argv []string
	switch fix {
	case fixKey:
		if !term.ask("Copy your SSH key to " + destination + " with ssh-copy-id? You type that machine's password once.") {
			return false
		}
		// ssh-copy-id runs its own ssh, so options set in the ssh setting
		// do not reach it; a destination from ~/.ssh/config does.
		argv = []string{"ssh-copy-id", destination}
	case fixHostKey:
		if !term.ask("Connect to " + destination + " once to see its host key and accept it?") {
			return false
		}
		argv = append(slices.Clone(e.cfg.SSH), "-o", "BatchMode=no", "-o", "ControlPath=none", "--", destination, "true")
	default:
		return false
	}
	fmt.Fprintf(term.out, "$ %s\n", strings.Join(argv, " "))
	code, err := term.run(ctx, argv)
	if err != nil {
		fmt.Fprintf(term.out, "could not run %s: %v\n", argv[0], err)
		return false
	}
	if code != 0 {
		fmt.Fprintf(term.out, "%s exited %d\n", argv[0], code)
	}
	return true
}

func (e *Extension) runAdd(ctx context.Context, args []string) error {
	set := cmdline.NewFlagSet(usageAdd)
	name := set.String("name", "", "the machine's name on this board; defaults to its hostname")
	group := set.String("group", "", "an existing group to file its rows under")
	home := set.String("home", "", "GATE_INBOX_HOME of the board on that machine, if it uses one")
	force := set.Bool("force", false, "add it even if a check that keeps it from working fails")
	operands, err := cmdline.Parse(os.Stdout, program, set, args, 1, 1)
	if err != nil {
		return err
	}
	var term *terminal
	if isTerminal(os.Stdin) {
		term = &terminal{in: bufio.NewReader(os.Stdin), out: os.Stdout, run: e.attach}
	}
	h, err := e.addMachine(ctx, addOptions{Destination: operands[0], Name: *name, Group: *group, Home: *home, Force: *force}, os.Stdout, term)
	if err != nil {
		var needs errNeedsTerminal
		if errors.As(err, &needs) {
			return fmt.Errorf("%w; run this at a terminal to fix it there", err)
		}
		return err
	}
	fmt.Printf("Added %s (%s). A running board shows its sessions within a few seconds.\n", h.Name, h.Destination)
	return nil
}

// setupFlag leads a setup row's arguments to `machines attach`, which is
// what the mirror tool block runs.
const setupFlag = "--add"

const (
	setupRoleName = "setup"
	setupRole     = extensionID + "/" + setupRoleName
)

// runSetupPane is `machines add` in a setup row's pane, which the machines
// screen opens for a machine whose ssh setup needs the operator to type.
// It waits for Enter before it ends, so the result stays on screen.
func (e *Extension) runSetupPane(ctx context.Context, args []string, caller string) error {
	set := cmdline.NewFlagSet(usageAdd)
	name := set.String("name", "", "")
	group := set.String("group", "", "")
	home := set.String("home", "", "")
	operands, err := cmdline.Parse(os.Stdout, program, set, args, 1, 1)
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	term := &terminal{in: in, out: os.Stdout, run: e.attach}
	fmt.Printf("Adding %s to this board.\n\n", operands[0])
	h, err := e.addMachine(ctx, addOptions{Destination: operands[0], Name: *name, Group: *group, Home: *home}, os.Stdout, term)
	if err != nil {
		fmt.Printf("\n%v\n", err)
	} else {
		fmt.Printf("\nAdded %s (%s). Its sessions show on the board within a few seconds.\n", h.Name, h.Destination)
	}
	fmt.Print("\nPress Enter to close this row.")
	_, _ = in.ReadString('\n')
	// A pane whose command ends is left on a shell, so the row would read
	// as running for good. The marker tells the board to archive it.
	if caller != "" && e.machines().dir != "" {
		if err := markSetupDone(e.machines().dir, caller); err != nil {
			fmt.Printf("could not mark this row done (%v); archive it with x\n", err)
		}
	}
	return nil
}

// setupDoneDir holds a file for each setup row whose pane has finished.
const setupDoneDir = "setup-done"

func markSetupDone(dataDir, id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("%q is not a session id", id)
	}
	dir := filepath.Join(dataDir, setupDoneDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id), nil, 0o600)
}

// setupDone says whether a setup row's pane has marked itself finished.
func setupDone(dataDir, id string) bool {
	if dataDir == "" || !sessionIDPattern.MatchString(id) {
		return false
	}
	_, err := os.Stat(filepath.Join(dataDir, setupDoneDir, id))
	return err == nil
}

func clearSetupDone(dataDir, id string) {
	if dataDir != "" && sessionIDPattern.MatchString(id) {
		_ = os.Remove(filepath.Join(dataDir, setupDoneDir, id))
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (e *Extension) runRemove(args []string) error {
	set := cmdline.NewFlagSet(usageRemove)
	operands, err := cmdline.Parse(os.Stdout, program, set, args, 1, 1)
	if err != nil {
		return err
	}
	if err := e.removeMachine(operands[0]); err != nil {
		return err
	}
	fmt.Printf("Removed %s. A running board puts its mirrors away within a few seconds.\n", operands[0])
	return nil
}

func (e *Extension) removeMachine(name string) error {
	return e.machines().change(func(store *storeFile, base []host) error {
		if inConfig(base, name) {
			return fmt.Errorf("%s is written in config.toml; delete its [extensions.machines.%s] table there, or pause it", name, name)
		}
		if _, ok := store.Machines[name]; !ok {
			return fmt.Errorf("no machine %q", name)
		}
		delete(store.Machines, name)
		store.Paused = slices.DeleteFunc(store.Paused, func(n string) bool { return n == name })
		return nil
	})
}

func inConfig(base []host, name string) bool {
	return slices.ContainsFunc(base, func(h host) bool { return h.Name == name })
}

func (e *Extension) runRename(args []string) error {
	set := cmdline.NewFlagSet(usageRename)
	operands, err := cmdline.Parse(os.Stdout, program, set, args, 2, 2)
	if err != nil {
		return err
	}
	if err := e.renameMachine(operands[0], operands[1]); err != nil {
		return err
	}
	fmt.Printf("Renamed %s to %s. A running board brings its mirrors back under the new name.\n", operands[0], operands[1])
	return nil
}

// renameMachine renames an added machine. Mirrors are keyed by their
// machine's name, so a running board puts the old ones away and starts
// them again under the new name.
func (e *Extension) renameMachine(from, to string) error {
	if err := checkName(to); err != nil {
		return err
	}
	reg := e.machines()
	old, _ := reg.host(from)
	err := reg.change(func(store *storeFile, base []host) error {
		if inConfig(base, from) {
			return fmt.Errorf("%s is written in config.toml; rename its table there", from)
		}
		s, ok := store.Machines[from]
		if !ok {
			return fmt.Errorf("no machine %q", from)
		}
		if _, taken := store.Machines[to]; taken || inConfig(base, to) {
			return fmt.Errorf("a machine named %s is already on this board", to)
		}
		delete(store.Machines, from)
		store.Machines[to] = s
		for i, n := range store.Paused {
			if n == from {
				store.Paused[i] = to
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if old.About != nil {
		_, _ = reg.learn(to, old.Destination, *old.About)
	}
	return nil
}

func (e *Extension) runPause(args []string, pause bool) error {
	usage := usageResume
	if pause {
		usage = usagePause
	}
	set := cmdline.NewFlagSet(usage)
	operands, err := cmdline.Parse(os.Stdout, program, set, args, 1, 1)
	if err != nil {
		return err
	}
	if err := e.pauseMachine(operands[0], pause); err != nil {
		return err
	}
	if pause {
		fmt.Printf("Paused %s. A running board puts its mirrors away and stops polling it.\n", operands[0])
	} else {
		fmt.Printf("Resumed %s.\n", operands[0])
	}
	return nil
}

func (e *Extension) pauseMachine(name string, pause bool) error {
	return e.machines().change(func(store *storeFile, base []host) error {
		if _, ok := store.Machines[name]; !ok && !inConfig(base, name) {
			return fmt.Errorf("no machine %q", name)
		}
		store.Paused = slices.DeleteFunc(store.Paused, func(n string) bool { return n == name })
		if pause {
			store.Paused = append(store.Paused, name)
		}
		return nil
	})
}

// listedMachine is one line of `machines list --json`.
type listedMachine struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Source      string `json:"source"`
	Paused      bool   `json:"paused,omitempty"`
	Group       string `json:"group,omitempty"`
	About       *about `json:"about,omitempty"`
}

func (e *Extension) runList(args []string) error {
	set := cmdline.NewFlagSet(usageList)
	asJSON := cmdline.JSONFlag(set)
	if _, err := cmdline.Parse(os.Stdout, program, set, args, 0, 0); err != nil {
		return err
	}
	reg := e.machines()
	listed := []listedMachine{}
	for _, h := range reg.all() {
		listed = append(listed, listedMachine{Name: h.Name, Destination: h.Destination, Source: h.Source, Paused: h.Paused, Group: h.Group, About: h.About})
	}
	text := "no machines: add one with `gate-inbox machines add <destination>`"
	if len(listed) > 0 {
		var b strings.Builder
		w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, m := range listed {
			state := ""
			if m.Paused {
				state = "paused"
			}
			about := "not reached yet"
			if m.About != nil {
				about = describeAbout(*m.About)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.Name, m.Destination, m.Source, state, about)
		}
		w.Flush()
		text = strings.TrimRight(b.String(), "\n")
	}
	if issue := reg.issue(); issue != "" && !*asJSON {
		text += "\n" + issue
	}
	return cmdline.Emit(os.Stdout, *asJSON, listed, text)
}
