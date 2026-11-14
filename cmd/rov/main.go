// Command rov runs and inspects a project's long-running dev processes in a
// per-project tmux server; see docs/SPEC.md.
package main

import (
	"os"

	"github.com/labsbit/rov/internal/cli"
	"github.com/labsbit/rov/internal/tui"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], tui.Run))
}
