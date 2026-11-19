package machines

import (
	"strings"
	"sync"
	"time"
)

// machineState is one machine as the last pass found it, for the machines
// screen.
type machineState struct {
	// Err is why the machine could not be read, and Since when that
	// started.
	Err          string
	Since        time.Time
	BoardRunning bool
	// Mirrored is its running sessions a mirror can attach to, and
	// NotMirrored the running ones it can't (adopted panes).
	Mirrored    int
	NotMirrored int
}

// statusBoard is where the poll loop leaves each machine's state, and
// tells the open machines screen to draw again.
type statusBoard struct {
	mu        sync.Mutex
	states    map[string]machineState
	listeners map[int]func()
	next      int
	launches  map[string]time.Time
}

func (b *statusBoard) trackLaunch(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.launches == nil {
		b.launches = map[string]time.Time{}
	}
	b.launches[key] = time.Now().Add(time.Minute)
}

// launchNotices reports the local result of launches confirmed by a worker.
func (b *statusBoard) launchNotices(now time.Time, mirrored map[string]string, failed map[string]time.Time) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var notes []string
	for _, key := range sortedKeys(b.launches) {
		machine, _ := splitKey(key)
		switch {
		case mirrored[key] != "":
			notes = append(notes, "Mirror ready for session on "+machine)
		case failed[key].After(now) || !now.Before(b.launches[key]):
			notes = append(notes, "Session started on "+machine+" · mirror unavailable")
		default:
			continue
		}
		delete(b.launches, key)
	}
	return strings.Join(notes, " · ")
}

func (b *statusBoard) set(states map[string]machineState) {
	b.mu.Lock()
	b.states = states
	listeners := make([]func(), 0, len(b.listeners))
	for _, fn := range b.listeners {
		listeners = append(listeners, fn)
	}
	b.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
}

func (b *statusBoard) get(name string) (machineState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.states[name]
	return st, ok
}

// listen calls fn after every pass until the returned func is called.
func (b *statusBoard) listen(fn func()) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listeners == nil {
		b.listeners = map[int]func(){}
	}
	id := b.next
	b.next++
	b.listeners[id] = fn
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.listeners, id)
	}
}

// publish leaves each observed machine's state for the machines screen.
func (m *mirrors) publish(observed []observation) {
	if m.status == nil {
		return
	}
	states := map[string]machineState{}
	for _, obs := range observed {
		name := obs.Host.Name
		var st machineState
		if obs.Err != nil {
			st.Err = obs.Err.Error()
			st.Since = m.since[name]
		} else {
			st.BoardRunning = obs.Snapshot.boardRunning()
			for _, s := range obs.Snapshot.Sessions {
				switch {
				case !s.Running:
				case s.Attachable:
					st.Mirrored++
				default:
					st.NotMirrored++
				}
			}
		}
		states[name] = st
	}
	m.status.set(states)
}
