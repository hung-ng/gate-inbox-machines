package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/usestring/gate-inbox/extension"
)

// fakeSessions is the SessionService a CLI command gets, paging the way
// the board does.
type fakeSessions struct {
	extension.SessionService
	pages [][]extension.SessionInfo
}

func (f *fakeSessions) List(_ context.Context, filter extension.SessionFilter) (extension.SessionList, error) {
	i := 0
	if filter.After != "" {
		i = int(filter.After[0] - '0')
	}
	page := extension.SessionList{Sessions: f.pages[i]}
	if i+1 < len(f.pages) {
		page.Truncated, page.Cursor = true, string(rune('0'+i+1))
	}
	return page, nil
}

func TestListSessionsReadsEveryPageAndLeavesOutMirrors(t *testing.T) {
	sessions := &fakeSessions{pages: [][]extension.SessionInfo{
		{
			{ID: "a1", Name: "api", Tool: "claude", Status: "working", Running: true},
			{ID: "b2", Name: "shell", Terminal: true, Running: true},
		},
		{
			{ID: "c3", Name: "mirror of laptop", Tool: "machines", Role: mirrorRole, Running: true},
			{ID: "d4", Name: "docs", Tool: "codex", Group: "web", Status: "waiting", Running: true, ParentID: "a1"},
			{ID: "e5", Name: "machines-e5", Tool: "machines", Running: true},
		},
	}}
	snap, err := listSessions(context.Background(), sessions, map[string]bool{"a1": true})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range snap.Sessions {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "a1,d4" {
		t.Errorf("listed %v, want a1 and d4: no terminal, no mirror, no hand-started machines tool", ids)
	}
	if snap.Version != wireVersion {
		t.Errorf("version = %d", snap.Version)
	}
	if !snap.Sessions[0].Attachable || snap.Sessions[1].Attachable {
		t.Errorf("attachable = %v, %v; want only a1, which has a gi_ session", snap.Sessions[0].Attachable, snap.Sessions[1].Attachable)
	}
	// What one end prints, the other end reads.
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(snap); err != nil {
		t.Fatal(err)
	}
	back, err := parseSnapshot(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if back.Sessions[1] != snap.Sessions[1] {
		t.Errorf("round trip = %+v, want %+v", back.Sessions[1], snap.Sessions[1])
	}
}

func TestParseSnapshotRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":          "gate-inbox: not inside a Gate Inbox session",
		"another version":   `{"version":2,"sessions":[]}`,
		"no version":        `{"sessions":[]}`,
		"id with a quote":   `{"version":1,"sessions":[{"id":"ab'; rm -rf ~"}]}`,
		"id in upper case":  `{"version":1,"sessions":[{"id":"AB12"}]}`,
		"empty id":          `{"version":1,"sessions":[{"id":""}]}`,
		"one session twice": `{"version":1,"sessions":[{"id":"ab"},{"id":"ab"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSnapshot(strings.NewReader(body)); err == nil {
				t.Error("parsed, want an error")
			}
		})
	}
}
