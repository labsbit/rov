// Package tui is rov's terminal UI (`rov` with no arguments): the process
// list on the left, the selected process's coloured output on the right,
// actions and rov.yaml editing from the keyboard. It is a client like the
// CLI: everything it does goes through package core, and quitting it leaves
// every process running.
//
// The model polls its sources on their own clocks (tmux 1 s, /proc 2 s,
// docker 5 s, rov.yaml on change) in tea.Cmds and composes a core.Status
// whenever one of them changes; actions run in tea.Cmds too, so the UI
// never waits on I/O. The output pane keeps the selected row's last 50 000
// lines and renders only the visible window, caching laid-out rows per
// width.
package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/labsbit/rov/internal/core"
)

// Run shows the TUI for the engine's project until the user quits.
func Run(e *core.Engine) error {
	m := New(NewBackend(e), Options{})
	defer m.close()
	_, err := tea.NewProgram(m).Run()
	return err
}
