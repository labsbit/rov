package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/pipe"
)

// runPipe is `rov __pipe <root> <name>`: the pipe-pane target that writes
// a pane's output to <log_dir>/<name>.{log,ansi}.
//
// It must never stop reading before EOF: tmux writes the pane's output into
// this pipe, and a full pipe would block the program in the pane. So
// whatever goes wrong — bad arguments, a broken rov.yaml, a full disk, a
// panic — it reports to stderr (which tmux discards) and keeps draining
// stdin. A broken rov.yaml means the default log_dir and log_max.
func runPipe(argv []string, stdin io.Reader, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "rov __pipe: panic: %v\n", r)
			_, _ = io.Copy(io.Discard, stdin)
			code = 0
		}
	}()
	// A hangup or ^C of the pane is not ours to act on; tmux closes the pipe
	// when the writer should go. SIGTERM ends it after flushing.
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	if len(argv) != 2 {
		fmt.Fprintln(stderr, "rov __pipe: usage: rov __pipe <root> <name>")
		_, _ = io.Copy(io.Discard, stdin)
		return 0
	}
	root, name := argv[0], argv[1]
	opt := pipeOptions(root, name)
	if err := pipe.Run(ctx, opt, stdin); err != nil {
		fmt.Fprintf(stderr, "rov __pipe %s: %v (output from here on is dropped)\n", name, err)
		_, _ = io.Copy(io.Discard, stdin)
	}
	return 0
}

// pipeOptions resolves log_dir and log_max from the project's rov.yaml,
// falling back to the defaults for anything it can't read.
func pipeOptions(root, name string) pipe.Options {
	p := &config.Project{Root: root, Config: config.Default(filepath.Join(root, config.FileName))}
	if data, err := os.ReadFile(p.ConfigPath()); err == nil {
		if cfg, _ := config.Parse(p.ConfigPath(), data); cfg != nil {
			p.Config = cfg
		}
	}
	max := p.Config.LogMax
	if max <= 0 {
		max = config.DefaultLogMax
	}
	return pipe.Options{Dir: p.LogDir(), Name: name, Max: max}
}
