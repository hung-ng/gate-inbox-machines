package machines

import (
	"slices"
	"strings"

	"github.com/usestring/gate-inbox/extension"
)

// state is what the board keeps between runs: which local row mirrors which
// session on another machine, and which mirrors the operator has put away, with the row
// they put away so a restore from the archive can pick it up again. Keys
// are "<machine>/<session id on that machine>".
type state struct {
	Mirrors   map[string]string `json:"mirrors"`
	Dismissed map[string]string `json:"dismissed"`
}

func newState() state {
	return state{Mirrors: map[string]string{}, Dismissed: map[string]string{}}
}

func (s state) clone() state {
	next := newState()
	for k, v := range s.Mirrors {
		next.Mirrors[k] = v
	}
	for k, v := range s.Dismissed {
		next.Dismissed[k] = v
	}
	return next
}

func mirrorKey(hostName, id string) string { return hostName + "/" + id }

func splitKey(key string) (hostName, id string) {
	hostName, id, _ = strings.Cut(key, "/")
	return hostName, id
}

// observation is one host's poll: its sessions, or why it could not be read.
type observation struct {
	Host     host
	Snapshot snapshot
	Err      error
}

type actionKind int

const (
	actLaunch actionKind = iota
	actRelaunch
	actPin
	actArchive
)

type action struct {
	Kind    actionKind
	Key     string
	Host    host
	Session machineSession
	// LocalID is the mirror a pin, a relaunch or an archive is for.
	LocalID string
	Status  string
	// Why is a relaunch's or an archive's reason, for the board's log.
	Why string
}

// plan is one reconcile: what to launch, pin and archive so the board's
// mirrors match what the hosts reported, and the state to keep once that is
// done. A launch's local id is only known once it has happened, so the
// caller records it in the returned state.
//
// It decides from what the hosts said and what the board shows, and never
// from a host it could not read: a host that drops off the network keeps
// its mirrors as they were until it answers again, rather than having
// them archived and launched again around a blip.
//
// pinned is the status each mirror was last pinned to, so an unchanged
// status is not pinned again on every pass. current is the mirrors this
// board's run launched or reconnected: a mirror from an earlier run is
// reconnected once, so its pane runs this build with the settings the board
// started with. That is what makes a restart the way to apply a changed
// setting, whichever kill and archive keys the board's release has.
func plan(prev state, configured []host, observed []observation, local map[string]extension.SessionInfo, pinned map[string]string, current map[string]bool) ([]action, state) {
	next := prev.clone()
	var actions []action

	// A row the operator archived (or deleted) is no longer on the board.
	// Remember that they put it away, so the next pass does not launch it
	// again while its session still runs on its machine.
	for _, key := range sortedKeys(prev.Mirrors) {
		if _, ok := local[prev.Mirrors[key]]; !ok {
			delete(next.Mirrors, key)
			next.Dismissed[key] = prev.Mirrors[key]
		}
	}
	// A row they restored from the archive is theirs again: follow it rather
	// than archive it as a row nothing names.
	for _, key := range sortedKeys(prev.Dismissed) {
		if id := prev.Dismissed[key]; id != "" {
			if _, ok := local[id]; ok {
				delete(next.Dismissed, key)
				next.Mirrors[key] = id
			}
		}
	}

	// A mirror row the state does not name was left by a state file that
	// was lost or replaced. Nothing will ever update it, so it goes.
	referenced := map[string]bool{}
	for _, id := range next.Mirrors {
		referenced[id] = true
	}
	for _, id := range sortedKeys(local) {
		if !referenced[id] {
			actions = append(actions, action{Kind: actArchive, LocalID: id, Why: "no saved state names this mirror"})
		}
	}

	// A host taken out of the config takes its mirrors with it.
	known := map[string]bool{}
	for _, h := range configured {
		known[h.Name] = true
	}
	for _, key := range sortedKeys(next.Mirrors) {
		if hostName, _ := splitKey(key); !known[hostName] {
			actions = append(actions, action{Kind: actArchive, Key: key, LocalID: next.Mirrors[key], Why: "its machine is no longer configured"})
			delete(next.Mirrors, key)
		}
	}
	for _, key := range sortedKeys(next.Dismissed) {
		if hostName, _ := splitKey(key); !known[hostName] {
			delete(next.Dismissed, key)
		}
	}

	for _, obs := range observed {
		if obs.Err != nil || !obs.Snapshot.boardRunning() {
			// Errored keeps a transport failure visible in every layout. It is a
			// stand-in for unavailable status, not a claim about the agent.
			for _, key := range sortedKeys(next.Mirrors) {
				id := next.Mirrors[key]
				name, _ := splitKey(key)
				if name == obs.Host.Name && local[id].Running && pinned[id] != "errored" {
					actions = append(actions, action{Kind: actPin, Key: key, LocalID: id, Status: "errored"})
				}
			}
			continue
		}
		live := map[string]machineSession{}
		for _, s := range obs.Snapshot.Sessions {
			if s.Running && s.Attachable {
				live[s.ID] = s
			}
		}
		for _, id := range sortedKeys(live) {
			s := live[id]
			key := mirrorKey(obs.Host.Name, id)
			if localID, ok := next.Mirrors[key]; ok {
				// A mirror's attach never ends on its own, so a pane that has
				// ended while its session still runs was killed from the board,
				// on a release whose kill keeps the row. It comes back in its
				// own place, as does a mirror from an earlier run of the board.
				why := ""
				switch {
				case !local[localID].Running:
					why = "its pane ended"
				case !current[localID]:
					why = "the board started"
				}
				if why != "" {
					actions = append(actions, action{Kind: actRelaunch, Key: key, Host: obs.Host, Session: s, LocalID: localID, Status: pinStatus(s.Status), Why: why})
					continue
				}
				if want := pinStatus(s.Status); pinned[localID] != want {
					actions = append(actions, action{Kind: actPin, Key: key, LocalID: localID, Status: want})
				}
				continue
			}
			if _, dismissed := next.Dismissed[key]; dismissed {
				continue
			}
			actions = append(actions, action{Kind: actLaunch, Key: key, Host: obs.Host, Session: s, Status: pinStatus(s.Status)})
		}
		prefix := obs.Host.Name + "/"
		for _, key := range sortedKeys(next.Mirrors) {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if !isLive(key, live) {
				actions = append(actions, action{Kind: actArchive, Key: key, LocalID: next.Mirrors[key], Why: "its session ended on its machine"})
				delete(next.Mirrors, key)
			}
		}
		// Once the session it dismissed stops running, a dismissal has done
		// its job: a revive on the same row is mirrored again.
		for _, key := range sortedKeys(next.Dismissed) {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if !isLive(key, live) {
				delete(next.Dismissed, key)
			}
		}
	}
	return actions, next
}

func isLive(key string, live map[string]machineSession) bool {
	_, id := splitKey(key)
	_, ok := live[id]
	return ok
}

// pinStatus is the status a mirror is pinned to for its session's status on
// its machine.
// PinStatus takes five; the rest of the board's statuses fold into the one
// triage treats them as. Delegating and background rows sort as finishes
// on the board itself, so they pin as finished.
func pinStatus(status string) string {
	switch status {
	case "starting", "working":
		return "working"
	case "waiting":
		return "waiting"
	case "finished", "delegating", "background":
		return "finished"
	case "errored":
		return "errored"
	default:
		return "idle"
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
