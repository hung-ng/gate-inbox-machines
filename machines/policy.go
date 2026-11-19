package machines

import (
	"context"
	"fmt"

	"github.com/usestring/gate-inbox/extension"
)

// LaunchEnv checks the mirror before the board creates its pane.
func (e *Extension) LaunchEnv(_ context.Context, l extension.Launch) (map[string]string, error) {
	mirror := l.Session.Role == mirrorRole
	if l.Session.Tool == toolName && !mirror {
		return nil, fmt.Errorf("mirror connects to an existing remote session; press Shift+n to start one")
	}
	if mirror && l.Session.Tool != toolName {
		return nil, fmt.Errorf("a mirror must keep the mirror tool; use N to start an agent on another machine")
	}
	if mirror && l.Reason == extension.LaunchRelaunch {
		if _, err := e.savedMirrorTarget(l.Session.ID); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

var _ extension.LaunchContributor = (*Extension)(nil)
