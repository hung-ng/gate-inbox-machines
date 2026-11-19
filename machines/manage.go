package machines

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	// GateInbox is this build's path on the machine, for one where neither
	// its PATH nor its login shell finds it.
	GateInbox string
	// Force adds the machine even when a step that keeps it from working
	// failed, for one that is set up later.
	Force bool
}

type addCheckError struct {
	destination string
	step        checkStep
}

func (err *addCheckError) Error() string {
	return fmt.Sprintf("%s was not added: %s failed; fix that and add it again, or pass --force to add it anyway", err.destination, err.step.Name)
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
// out.
func (e *Extension) addMachine(ctx context.Context, opts addOptions, out io.Writer) (host, error) {
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
	opts.Home = strings.TrimSpace(opts.Home)
	for _, h := range existing {
		if h.Destination == opts.Destination && sameBoard(h, opts.Home) {
			return host{}, fmt.Errorf("%s is already on this board as %s%s", opts.Destination, h.Name, otherBoardHint(h))
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
	opts.GateInbox = strings.TrimSpace(opts.GateInbox)
	if opts.GateInbox != "" && !strings.HasPrefix(opts.GateInbox, "/") && !strings.HasPrefix(opts.GateInbox, "~/") {
		return host{}, fmt.Errorf("gate-inbox path %q must be a full path, or start with ~/", opts.GateInbox)
	}
	if target, ok := e.sshTarget(ctx, opts.Destination); ok && !opts.Force {
		for _, h := range existing {
			if other, ok := e.sshTarget(ctx, h.Destination); ok && other == target && sameBoard(h, opts.Home) {
				return host{}, fmt.Errorf("%s reaches the same host as %s (%s), which is already on this board%s", opts.Destination, h.Name, target, otherBoardHint(h))
			}
		}
	}
	settings := hostSettings{Destination: opts.Destination, Group: opts.Group, Home: opts.Home, GateInbox: opts.GateInbox}
	// The probe runs under a stand-in name; the real one may come from the
	// hostname the machine reports.
	probeHost, err := resolveHost("new", settings)
	if err != nil {
		return host{}, err
	}
	if err := ensureControlDir(e.cfg.ControlPath); err != nil {
		return host{}, err
	}
	result := forAdding(e.probe(ctx, probeHost))
	result.print(out, opts.Destination)
	if blocked := result.blocking(); blocked != nil && !opts.Force {
		return host{}, &addCheckError{opts.Destination, *blocked}
	}
	// The LAN address of a machine already on the board passes every
	// check: what it reports about itself is what tells them apart.
	if result.Snapshot != nil && result.Snapshot.About != nil && result.Snapshot.About.Hostname != "" && !opts.Force {
		for _, h := range existing {
			if h.About != nil && strings.EqualFold(h.About.Hostname, result.Snapshot.About.Hostname) && sameBoard(h, opts.Home) {
				return host{}, fmt.Errorf("%s is the same machine as %s: both report hostname %s%s; pass --force to add it anyway", opts.Destination, h.Name, h.About.Hostname, otherBoardHint(h))
			}
		}
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
	err = reg.change(func(store *storeFile) error {
		if _, ok := store.Machines[name]; ok {
			return fmt.Errorf("a machine named %s is already on this board", name)
		}
		for other, s := range store.Machines {
			if s.Destination == opts.Destination && (opts.Home == "" || s.Home == opts.Home) {
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

// sameBoard is whether adding a machine with board home home, on the host
// h is on, would show h's board a second time. One host can run two
// boards, each on a home of its own, so naming another home is a second
// machine here. Naming none is taken as h's board, since a home is easy to
// leave out and a second board on one host is rare.
func sameBoard(h host, home string) bool {
	return home == "" || home == h.Home
}

func otherBoardHint(h host) string {
	if h.Home != "" {
		return fmt.Sprintf(" with board home %s; for another board on that host, give its board home", h.Home)
	}
	return ""
}

// forAdding gives fixes in terms of the add form fields and flags.
func forAdding(result probeResult) probeResult {
	for i, step := range result.Steps {
		if step.Err == nil {
			continue
		}
		switch {
		case step.Name == "gate-inbox" && notFound(step.Err):
			result.Steps[i].Fix = "no gate-inbox there, even through your login shell; give this build's full path there as the gate-inbox path (--gate-inbox)"
		case step.Name == "attachable session":
			result.Steps[i].Fix = notMirroredFix
		}
	}
	return result
}

func addFlags(set *flag.FlagSet) func(destination string) addOptions {
	name := set.String("name", "", "the machine's name on this board; defaults to its hostname")
	group := set.String("group", "", "an existing group to file its rows under")
	home := set.String("home", "", "GATE_INBOX_HOME of the board on that machine, if it uses one")
	gateInbox := set.String("gate-inbox", "", "this build's full path on that machine, if neither its PATH nor its login shell finds it")
	force := set.Bool("force", false, "add it even if a check that keeps it from working fails, or it looks like a machine already added")
	return func(destination string) addOptions {
		return addOptions{Destination: destination, Name: *name, Group: *group, Home: *home, GateInbox: *gateInbox, Force: *force}
	}
}

func (e *Extension) runAdd(ctx context.Context, args []string) error {
	set := cmdline.NewFlagSet(usageAdd)
	options := addFlags(set)
	operands, err := cmdline.Parse(os.Stdout, program, set, args, 1, 1)
	if err != nil {
		return err
	}
	h, err := e.addMachine(ctx, options(operands[0]), os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("Added %s (%s). A running board shows its sessions within a few seconds.\n", h.Name, h.Destination)
	return nil
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
	return e.machines().change(func(store *storeFile) error {
		if _, ok := store.Machines[name]; !ok {
			return fmt.Errorf("no machine %q", name)
		}
		delete(store.Machines, name)
		store.Paused = slices.DeleteFunc(store.Paused, func(n string) bool { return n == name })
		return nil
	})
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
		fmt.Printf("Paused %s. A running board keeps its mirrors and stops polling it.\n", operands[0])
	} else {
		fmt.Printf("Resumed %s.\n", operands[0])
	}
	return nil
}

func (e *Extension) pauseMachine(name string, pause bool) error {
	return e.machines().change(func(store *storeFile) error {
		if _, ok := store.Machines[name]; !ok {
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
		listed = append(listed, listedMachine{Name: h.Name, Destination: h.Destination, Paused: h.Paused, Group: h.Group, About: h.About})
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
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Name, m.Destination, state, about)
		}
		w.Flush()
		text = strings.TrimRight(b.String(), "\n")
	}
	if issue := reg.issue(); issue != "" && !*asJSON {
		text += "\n" + issue
	}
	return cmdline.Emit(os.Stdout, *asJSON, listed, text)
}
