// Package core is rov's engine, shared by the CLI and the TUI: it reads the
// project's state from tmux, /proc and docker, composes it into a Status,
// and runs actions (up, restart, stop, down, spawn, rm, send, wait) under a
// per-process lock.
//
// # State
//
// rov has no state of its own. A Status is composed from four independent
// sources, each cheap enough to poll at its own rate:
//
//	source       call                  cost             TUI rate
//	tmux         Engine.Sessions       one tmux exec    1 s
//	/proc        Engine.ScanPorts      a few ms         2 s
//	docker       Engine.Containers     one docker ps    5 s
//	rov.yaml     Engine.Reload         a stat           on file change
//
// Compose(Engine.Inputs(sessions, snap, containers, dockerErr)) is pure and
// cheap; call it whenever any source changes, keeping the latest value of
// the others. After composing with a fresh port scan, call
// Engine.RecordPorts (it writes @rov_last_ports only when they changed).
// Engine.Status does all of it once, for the CLI.
//
// # Actions
//
// Every action method blocks until done and is safe to call from a
// goroutine (the TUI runs them in a tea.Cmd). Actions on one process are
// serialised across rov processes by a flock on <log_dir>/.lock/proc/<name>; a
// caller that waits longer than Engine.LockTimeout gets a *BusyError.
// Action methods return a short message for "<name>: <msg>". Resolve
// validates names (and expands "all") before a batch runs.
//
// Attach: Engine.AttachCommand returns an *exec.Cmd for tea.ExecProcess;
// the CLI execs tmux instead (Tmux.ExecAttach).
//
// # Processes
//
// Config processes come from rov.yaml in file order; ad-hoc ones (spawned,
// or sessions whose name left rov.yaml) follow, by start time. A
// process's state comes from its session: running, stopped (@rov_stopped),
// exited (0), crashed (ExitCode), dead (killed by a signal), or absent (no
// session). tmux sometimes loses the exit status (see package tmux);
// tmux.Client.Sessions recovers it, so "dead" really means a signal.
// Process.RunID (@rov_run_id, set by every start) tells runs apart, also
// two within one second. An ad-hoc process is Changed while Redefine has
// stored a definition other than the one it runs (kept as @rov_run_def).
// Ports are the listening sockets of the pane's process tree plus the
// published ports of the containers of the compose project it owns
// (`docker:`).
//
// # Starting and stopping
//
// A start is one tmux call: new-session (or respawn-pane) with the real
// command, pipe-pane and the session options, after the logs moved to .prev
// (see Engine.launch). There is no placeholder process: tmux neither reads
// output nor reaps a child mid-chain, so the pipe sees the first byte and a
// failed call leaves nothing that looks like it runs.
//
// A stop flags the session stopped first (stopMarked), so a status read
// while the process dies says stopped, not dead, and then signals the
// pane's process tree (stopTree): its groups and members,
// each only while it is still the process the scan saw (pid + start time).
// What survives SIGKILL is a *StopError. If rov itself or an ancestor is in
// the tree, nothing is signalled and the action returns *InsideTreeError
// (the CLI re-runs it detached). A setsid'd child whose parent exited has
// left the tree; it is stopped as an orphan only when it holds one of the
// process's last ports, started at or after @rov_started and belongs to no
// other session — a conservative match, so a server the user started by
// hand on that port earlier is never killed (Compose warns about such
// orphans too).
package core
