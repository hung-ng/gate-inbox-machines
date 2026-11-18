// Command gate-inbox is Gate Inbox built with the machines extension: the
// board's own app package, plus the sessions of the hosts named under
// [extensions.machines.<name>]. Run the same build on every machine; a host's
// board is read through this build's `machines sessions`.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hung-ng/gate-inbox-machines/machines"
	"github.com/usestring/gate-inbox/app"
	"github.com/usestring/gate-inbox/extension"
)

// version is stamped in with -ldflags "-X main.version=..."; left alone, the
// app falls back to the module version, then to "dev".
var version = "dev"

func main() {
	err := app.Run(context.Background(), os.Args[1:], app.Options{
		Extensions:     []extension.Extension{machines.New()},
		BuildInfo:      app.BuildInfo{Version: version},
		ConfigDefaults: machines.ConfigDefaults,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, app.Name+":", err)
		os.Exit(app.ExitCode(err))
	}
}
