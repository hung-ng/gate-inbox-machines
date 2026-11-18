package machines

import (
	"context"
	"fmt"

	"github.com/usestring/gate-inbox/extension"
)

// LaunchEnv keeps an explicitly marked mirror on its own tool.
func (e *Extension) LaunchEnv(_ context.Context, l extension.Launch) (map[string]string, error) {
	if l.Session.Role == mirrorRole && l.Session.Tool != toolName {
		return nil, fmt.Errorf("a mirror must keep the mirror tool; use N to start an agent on another machine")
	}
	return nil, nil
}

var _ extension.LaunchContributor = (*Extension)(nil)
