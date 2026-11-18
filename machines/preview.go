package machines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/usestring/gate-inbox/extension"
)

type previewRoute struct {
	host host
	id   string
}

// The reconcile loop publishes routes for the UI's selected-row reads.
type previewRoutes struct {
	mu   sync.RWMutex
	rows map[string]previewRoute
	// stopped is the mirrors whose machine's board is stopped, by machine,
	// so their preview can say why it is empty.
	stopped map[string]string
}

func (r *previewRoutes) update(state state, observed []observation) {
	rows := make(map[string]previewRoute, len(state.Mirrors))
	stopped := map[string]string{}
	for _, obs := range observed {
		if obs.Err == nil && !obs.Snapshot.boardRunning() {
			for _, session := range obs.Snapshot.Sessions {
				if id := state.Mirrors[mirrorKey(obs.Host.Name, session.ID)]; id != "" {
					stopped[id] = obs.Host.Name
				}
			}
		}
		if obs.Err != nil || !obs.Snapshot.boardRunning() {
			continue
		}
		for _, session := range obs.Snapshot.Sessions {
			if id := state.Mirrors[mirrorKey(obs.Host.Name, session.ID)]; id != "" && session.Running && session.Attachable {
				rows[id] = previewRoute{host: obs.Host, id: session.ID}
			}
		}
	}
	r.mu.Lock()
	r.rows, r.stopped = rows, stopped
	r.mu.Unlock()
}

// unavailable is why a mirror has no preview.
func (r *previewRoutes) unavailable(id string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if machine := r.stopped[id]; machine != "" {
		return fmt.Errorf("no preview until %s's board is running", machine)
	}
	return fmt.Errorf("the remote session is unavailable")
}

func (r *previewRoutes) lookup(id string) (previewRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.rows[id]
	return route, ok
}

func (e *Extension) readPreview(ctx context.Context, localID string) (extension.SessionPreview, error) {
	var preview extension.SessionPreview
	route, ok := e.previews.lookup(localID)
	if !ok {
		return preview, e.previews.unavailable(localID)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := machineCommand(route.host, route.host.GateInbox, "machines", "preview", route.id)
	out, err := e.run(ctx, sshArgv(e.cfg, false, route.host.Destination, command))
	if err != nil {
		return preview, fmt.Errorf("could not read the preview on %s: %w", route.host.Name, err)
	}
	if err := json.Unmarshal(out, &preview); err != nil {
		return extension.SessionPreview{}, fmt.Errorf("could not read the preview on %s: %w", route.host.Name, err)
	}
	if current, ok := e.previews.lookup(localID); !ok || current != route {
		return extension.SessionPreview{}, fmt.Errorf("the remote session is unavailable")
	}
	return preview, nil
}

func writePreview(ctx context.Context, id string, host extension.Host, out io.Writer) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("%q is not a session id", id)
	}
	if host == nil || host.Caller() != "" {
		return fmt.Errorf("only the operator can read a session preview")
	}
	reader, ok := host.Sessions().(extension.PreviewReader)
	if !ok {
		return fmt.Errorf("the board lent this command no preview reader")
	}
	preview, err := reader.Preview(ctx, id)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(preview)
}
