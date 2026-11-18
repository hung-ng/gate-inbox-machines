package machines

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/usestring/gate-inbox/extension"
)

var mini = host{Name: "mini", Destination: "mini", GateInbox: "gate-inbox", Tmux: "tmux", Group: "mini"}
var box = host{Name: "box", Destination: "box", GateInbox: "gate-inbox", Tmux: "tmux", Group: "box"}

func seen(h host, sessions ...machineSession) observation {
	return observation{Host: h, Snapshot: snapshot{BoardRunning: boolPtr(true), Version: wireVersion, Sessions: sessions}}
}

func unreachable(h host) observation {
	return observation{Host: h, Err: errors.New("ssh: connect to host mini port 22: Operation timed out")}
}

func live(id, status string) machineSession {
	return machineSession{ID: id, Name: "agent " + id, Tool: "claude", Status: status, Running: true, Attachable: true}
}

func mirrorRow(id string, running bool) extension.SessionInfo {
	return extension.SessionInfo{ID: id, Role: mirrorRole, Running: running}
}

func rows(infos ...extension.SessionInfo) map[string]extension.SessionInfo {
	m := map[string]extension.SessionInfo{}
	for _, info := range infos {
		m[info.ID] = info
	}
	return m
}

// describe is a plan's actions in a line each, for comparing.
func describe(actions []action) []string {
	var out []string
	for _, a := range actions {
		switch a.Kind {
		case actLaunch:
			line := "launch " + a.Key
			if a.Status != "" {
				line += " as " + a.Status
			}
			out = append(out, line)
		case actPin:
			out = append(out, fmt.Sprintf("pin %s %s", a.LocalID, a.Status))
		case actArchive:
			out = append(out, "archive "+a.LocalID)
		}
	}
	return out
}

func withMirrors(pairs ...string) state {
	st := newState()
	for i := 0; i+1 < len(pairs); i += 2 {
		st.Mirrors[pairs[i]] = pairs[i+1]
	}
	return st
}

func TestPlan(t *testing.T) {
	for _, tc := range []struct {
		name          string
		prev          state
		observed      []observation
		local         map[string]extension.SessionInfo
		pinned        map[string]string
		want          []string
		wantMirrors   map[string]string
		wantDismissed []string
	}{{
		name:     "a new session on a machine gets a mirror",
		prev:     newState(),
		observed: []observation{seen(mini, live("a1", "waiting"))},
		want:     []string{"launch mini/a1 as waiting"},
	}, {
		name:     "a session that is not running gets none",
		prev:     newState(),
		observed: []observation{seen(mini, machineSession{ID: "a1", Status: "dead"})},
	}, {
		name:     "an adopted pane, with no gi_ session to attach to, gets none",
		prev:     newState(),
		observed: []observation{seen(mini, machineSession{ID: "a1", Status: "waiting", Running: true})},
	}, {
		name:     "a mirrored session that can no longer be attached to loses its mirror",
		prev:     withMirrors("mini/a1", "l1"),
		observed: []observation{seen(mini, machineSession{ID: "a1", Status: "waiting", Running: true})},
		local:    rows(mirrorRow("l1", true)),
		want:     []string{"archive l1"},
	}, {
		name:        "a changed status is pinned",
		prev:        withMirrors("mini/a1", "l1"),
		observed:    []observation{seen(mini, live("a1", "waiting"))},
		local:       rows(mirrorRow("l1", true)),
		pinned:      map[string]string{"l1": "working"},
		want:        []string{"pin l1 waiting"},
		wantMirrors: map[string]string{"mini/a1": "l1"},
	}, {
		name:        "an unchanged status is not pinned again",
		prev:        withMirrors("mini/a1", "l1"),
		observed:    []observation{seen(mini, live("a1", "delegating"))},
		local:       rows(extension.SessionInfo{ID: "l1", Running: true, Status: "finished"}),
		pinned:      map[string]string{"l1": "finished"},
		wantMirrors: map[string]string{"mini/a1": "l1"},
	}, {
		name:        "an acknowledged finish stays idle",
		prev:        withMirrors("mini/a1", "l1"),
		observed:    []observation{seen(mini, live("a1", "finished"))},
		local:       rows(extension.SessionInfo{ID: "l1", Running: true, Status: "idle"}),
		pinned:      map[string]string{"l1": "finished"},
		wantMirrors: map[string]string{"mini/a1": "l1"},
	}, {
		name:     "a session that ended on its machine takes its mirror with it",
		prev:     withMirrors("mini/a1", "l1"),
		observed: []observation{seen(mini)},
		local:    rows(mirrorRow("l1", true)),
		want:     []string{"archive l1"},
	}, {
		name:        "an unreachable host keeps its mirrors visible",
		prev:        withMirrors("mini/a1", "l1"),
		observed:    []observation{unreachable(mini)},
		local:       rows(mirrorRow("l1", true)),
		pinned:      map[string]string{"l1": "waiting"},
		wantMirrors: map[string]string{"mini/a1": "l1"},
	}, {
		name:          "a mirror the operator archived is not launched again",
		prev:          withMirrors("mini/a1", "l1"),
		observed:      []observation{seen(mini, live("a1", "waiting"))},
		local:         rows(),
		wantDismissed: []string{"mini/a1"},
	}, {
		name: "a dismissal ends once its session stops running",
		prev: func() state {
			st := newState()
			st.Dismissed["mini/a1"] = "gone1"
			return st
		}(),
		observed: []observation{seen(mini)},
	}, {
		name:        "a mirror killed from the board stays dead",
		prev:        withMirrors("mini/a1", "l1"),
		observed:    []observation{seen(mini, live("a1", "waiting"))},
		local:       rows(mirrorRow("l1", false)),
		wantMirrors: map[string]string{"mini/a1": "l1"},
	}, {
		name:        "an existing running mirror keeps its local row",
		prev:        withMirrors("mini/a1", "l1"),
		observed:    []observation{seen(mini, live("a1", "waiting"))},
		local:       rows(extension.SessionInfo{ID: "l1", Running: true, Status: "waiting"}),
		pinned:      map[string]string{"l1": "waiting"},
		wantMirrors: map[string]string{"mini/a1": "l1"},
	}, {
		name: "a mirror restored from the archive is followed again",
		prev: func() state {
			st := newState()
			st.Dismissed["mini/a1"] = "l1"
			return st
		}(),
		observed:    []observation{seen(mini, live("a1", "waiting"))},
		local:       rows(mirrorRow("l1", true)),
		want:        []string{"pin l1 waiting"},
		wantMirrors: map[string]string{"mini/a1": "l1"},
	}, {
		name:     "a mirror row nothing names is archived",
		prev:     newState(),
		observed: []observation{seen(mini)},
		local:    rows(mirrorRow("stray", true)),
		want:     []string{"archive stray"},
	}, {
		name:     "a host taken out of the config takes its mirrors",
		prev:     withMirrors("gone/a1", "l1"),
		observed: []observation{seen(mini)},
		local:    rows(mirrorRow("l1", true)),
		want:     []string{"archive l1"},
	}, {
		name:        "one host's poll does not touch another's mirrors",
		prev:        withMirrors("box/b1", "l2"),
		observed:    []observation{seen(mini, live("a1", "idle")), unreachable(box)},
		local:       rows(mirrorRow("l2", true)),
		pinned:      map[string]string{"l2": "working"},
		want:        []string{"launch mini/a1 as idle"},
		wantMirrors: map[string]string{"box/b1": "l2"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.local == nil {
				tc.local = rows()
			}
			actions, next := plan(tc.prev, []host{mini, box}, tc.observed, tc.local, tc.pinned)
			if got := describe(actions); !slices.Equal(got, tc.want) {
				t.Errorf("actions = %q, want %q", got, tc.want)
			}
			if tc.wantMirrors == nil {
				tc.wantMirrors = map[string]string{}
			}
			if fmt.Sprint(next.Mirrors) != fmt.Sprint(tc.wantMirrors) {
				t.Errorf("mirrors = %v, want %v", next.Mirrors, tc.wantMirrors)
			}
			if got := strings.Join(sortedKeys(next.Dismissed), ","); got != strings.Join(tc.wantDismissed, ",") {
				t.Errorf("dismissed = %s, want %v", got, tc.wantDismissed)
			}
		})
	}
}

func TestPinStatusCoversEveryBoardStatus(t *testing.T) {
	allowed := map[string]bool{"working": true, "waiting": true, "finished": true, "idle": true, "errored": true}
	for _, s := range []string{"starting", "working", "waiting", "delegating", "background", "finished", "idle", "errored", "dead", "something new"} {
		if got := pinStatus(s); !allowed[got] {
			t.Errorf("pinStatus(%q) = %q, which PinStatus refuses", s, got)
		}
	}
	if pinStatus("waiting") != "waiting" {
		t.Error("a waiting session must stay waiting, or triage never hands it over")
	}
}
