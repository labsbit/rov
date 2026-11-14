package core

import (
	"context"
	"os"
	"time"

	"github.com/labsbit/rov/internal/logs"
	"github.com/labsbit/rov/internal/pipe"
	"github.com/labsbit/rov/internal/tmux"
)

// LogRetention is how long the logs of a name that is neither configured
// nor a live session are kept after their last write.
const LogRetention = 7 * 24 * time.Hour

// CollectLogs deletes stale logs: those of names that are neither in
// rov.yaml nor a live session, last written more than LogRetention ago.
// sessions is a fresh listing (nil means: list them). Every CLI call runs it;
// it is cheap (one directory read) and its errors are not worth failing a
// command for.
//
// Two guards keep it to files rov owns: it does nothing in a project without
// rov.yaml (whose log_dir may be anyone's .logs), and it only touches names
// with a .ansi file, which only `rov __pipe` writes.
func (e *Engine) CollectLogs(ctx context.Context, sessions []tmux.Session) ([]string, error) {
	if !e.HasConfig() {
		return nil, nil
	}
	if sessions == nil {
		var err error
		if sessions, err = e.Sessions(ctx); err != nil {
			return nil, err
		}
	}
	keep := map[string]bool{}
	for _, n := range e.Config().Names() {
		keep[n] = true
	}
	for _, s := range sessions {
		keep[s.Name] = true
	}
	dir := e.LogDir()
	return logs.GC(dir, func(name string) bool { return keep[name] || !rovWrote(dir, name) }, LogRetention, e.Now())
}

// rovWrote reports whether name's logs were written by rov __pipe: it
// always writes a .ansi next to the .log.
func rovWrote(dir, name string) bool {
	p := pipe.Paths(dir, name)
	for _, f := range []string{p.ANSI, p.ANSI1, p.PrevANSI} {
		if _, err := os.Lstat(f); err == nil {
			return true
		}
	}
	return false
}
