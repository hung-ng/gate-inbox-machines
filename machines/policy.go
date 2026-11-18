package machines

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/usestring/gate-inbox/extension"
)

// LaunchEnv refuses the two launches that would turn a mirror into a local
// agent: a mirror relaunched on another tool, which the rename card allows,
// and a migration from a mirror to an agent CLI. Either would start an agent
// on this machine in the place of one that runs on another, with nothing on
// the row to say so. It is the launch hook rather than a MigrationObserver
// because v1.1.0 tells an observer only once the new pane has started.
//
// It is not a SpawnPolicy, though a spawn onto the mirror tool from the
// new-session picker is just as useless: the board fails a policy closed, so
// a mistake in [extensions.machines] would stop every spawn on the board,
// local agents included. That spawn already ends at once with a note saying
// to pick an agent CLI (see runAttach).
func (e *Extension) LaunchEnv(_ context.Context, l extension.Launch) (map[string]string, error) {
	if l.Session.Role == mirrorRole && l.Session.Tool != toolName {
		return nil, fmt.Errorf("a mirror must keep the mirror tool; use N to start an agent on another machine")
	}
	if l.From == "" || l.Session.Tool == toolName {
		return nil, nil
	}
	dir, err := e.conf.DataDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved state
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	for _, rows := range []map[string]string{saved.Mirrors, saved.Dismissed} {
		for _, id := range rows {
			if id == l.From {
				return nil, fmt.Errorf("a mirror cannot move to a local agent; use N to start an agent on that machine")
			}
		}
	}
	return nil, nil
}

var _ extension.LaunchContributor = (*Extension)(nil)
