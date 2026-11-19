// Command gate-inbox builds Gate Inbox with the machines extension.
// Controllers use it to run the board. Workers can run a stock board and
// use this build to answer SSH requests for session data.
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
