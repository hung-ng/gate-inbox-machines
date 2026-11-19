package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
			{ID: "d4", Name: "docs", Tool: "codex", Group: "web", Directory: "/home/user/docs project", Status: "waiting", Running: true, ParentID: "a1"},
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
	if snap.Sessions[1].Directory != "/home/user/docs project" {
		t.Fatal(snap.Sessions[1])
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
		"missing sessions":  `{"version":1}`,
		"null sessions":     `{"version":1,"sessions":null}`,
		"extra json":        `{"version":1,"sessions":[]}{}`,
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

func TestWireAdditionsAndLegacyExclusion(t *testing.T) {
	s, err := listSessions(context.Background(), &fakeSessions{pages: [][]extension.SessionInfo{{
		{ID: "a1", Tool: "machines"}, {ID: "a2", Tool: "mirror"}, {ID: "a3", Role: mirrorRole}, {ID: "a4", Tool: "agent"},
	}}}, nil)
	if err != nil || len(s.Sessions) != 1 || s.Sessions[0].ID != "a4" {
		t.Fatal(s, err)
	}
	s.Tools = []string{"agent"}
	s.BoardRunning = boolPtr(true)
	data, _ := json.Marshal(s)
	back, err := parseSnapshot(strings.NewReader(string(data)))
	if err != nil || !back.boardRunning() || len(back.Tools) != 1 || back.Version != 1 {
		t.Fatal(back, err)
	}
	old, err := parseSnapshot(strings.NewReader(`{"version":1,"sessions":[]}`))
	if err != nil || !old.boardRunning() {
		t.Fatal(old, err)
	}
	ids := parseTmuxSessions("gi_a1 0\ngi_poll-anchor 1\n")
	if !ids["a1"] || ids["poll-anchor"] || parseTmuxSessions("gi_poll-anchor 0")["poll-anchor"] {
		t.Fatal(ids)
	}
}

func TestBoardSingletonPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.lock")
	if boardRunning(dir) {
		t.Fatal("missing file")
	}
	for _, text := range []string{"", "garbage", "0", "-1", "4294967297"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if boardRunning(dir) {
			t.Fatalf("running for %q", text)
		}
	}
	os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\nextra line"), 0600)
	if !boardRunning(dir) {
		t.Fatal("own PID stopped")
	}
	child := exec.Command("/bin/sh", "-c", "exit 0")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(strconv.Itoa(pid)), 0600)
	if boardRunning(dir) {
		t.Fatal("reaped child running")
	}
}

func TestListSessionsRejectsIncompletePages(t *testing.T) {
	failed := errors.New("list failed")
	for _, outcome := range []string{"missing cursor", "repeated cursor", "later error", "skipped row"} {
		t.Run(outcome, func(t *testing.T) {
			calls := 0
			list := func(_ context.Context, filter extension.SessionFilter) (extension.SessionList, error) {
				calls++
				if filter.Limit != 500 {
					t.Fatalf("limit = %d", filter.Limit)
				}
				page := extension.SessionList{
					Sessions:  []extension.SessionInfo{{ID: fmt.Sprintf("%x", calls)}},
					Truncated: true,
					Cursor:    "cursor",
				}
				if outcome == "missing cursor" {
					page.Cursor = ""
				}
				if outcome == "skipped row" {
					page.Matched = 3
					if filter.After != "" {
						page.Truncated, page.Cursor = false, ""
					}
				}
				if outcome == "later error" && filter.After != "" {
					return extension.SessionList{}, failed
				}
				return page, nil
			}
			snap, err := listSessions(context.Background(), listingService{list: list}, nil)
			if err == nil || snap.Sessions != nil {
				t.Fatal(snap, err)
			}
		})
	}
}

type listingService struct {
	extension.SessionService
	list func(context.Context, extension.SessionFilter) (extension.SessionList, error)
}

func (s listingService) List(ctx context.Context, filter extension.SessionFilter) (extension.SessionList, error) {
	return s.list(ctx, filter)
}
