package cli

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
)

// Version is rov's version; `rov --version` prints it with the module path.
// Release builds set it with -ldflags "-X github.com/labsbit/rov/internal/cli.Version=…".
var Version = "0.1.0"

// releaseTag matches a plain release version such as v1.2.3.
var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// ModulePath identifies this rov in --version.
const ModulePath = "github.com/labsbit/rov"

// VersionString is the --version line, e.g.
// "rov 0.1.0 (github.com/labsbit/rov, rev 1a2b3c4d)".
func VersionString() string {
	rev, version := "", Version
	if bi, ok := debug.ReadBuildInfo(); ok {
		// `go install …@vX.Y.Z` stamps the release tag; prefer it. Local
		// builds get a pseudo-version (v0.0.0-<date>-<hash>) and keep Version.
		if v := bi.Main.Version; releaseTag.MatchString(v) {
			version = strings.TrimPrefix(v, "v")
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 8 {
				rev = ", rev " + s.Value[:8]
			}
			if s.Key == "vcs.modified" && s.Value == "true" && rev != "" {
				rev += "+dirty"
			}
		}
	}
	return fmt.Sprintf("rov %s (%s%s)", version, ModulePath, rev)
}

const usageText = `rov — Run, Observe, Verify: a dev process manager (tmux-backed; one server per project)

  rov                              open the TUI
  rov init                         write a starter rov.yaml (+ .gitignore entry)
  rov up <name...|all>             start if not running
  rov restart <name...|all>        stop the process tree, start again
  rov stop <name...|all>           stop the tree, keep the session
  rov down <name...|all>           kill the session (ad-hoc: same as rm)
  rov spawn <name> [--cwd DIR] [--env K=V]... [--replace] [--wait] -- <cmd...>
                                   start an ad-hoc process
  rov rm <name...> [--purge]       stop + remove ad-hoc processes (--purge: delete logs)
  rov status [--json]              one line per process
  rov show <name> [--json] [-n N]  everything about one process + last N lines
  rov logs <name> [-n N] [-f] [--grep RE] [--prev] [--color] [--json]
  rov ports [--json]               every listening TCP port, with its owner
  rov wait <name> [--port N | --log RE | --exit | --healthy] [--timeout D]
                                   block until ready (exit 0 met, 1 timeout, 3 ended)
  rov config add <name> [--after P | --first | --last] [--cwd DIR] [--env K=V]...
                 [--desc TEXT] [--docker PROJECT] -- <cmd...>
  rov config set <name> [--cmd CMD] [--cwd DIR] [--env K=V]... [--unset-env K]...
                 [--desc TEXT] [--docker PROJECT] [--rename NEW]
  rov config mv <name> --after P | --first | --last | --up | --down
  rov config rm <name> [--stop]
  rov list [--json]                names, kind and commands
  rov attach <name>                interactive tmux session (human only)
  rov send <name> <text...>        type a line into the pane
  rov skill                        print the agent skill (SKILL.md)
  rov skill install|uninstall [--global] [--agent claude,cursor,codex] [--force]
                                   into this project (or ~): .claude/skills, .agents/skills

Global: -C <dir> (project directory), --version.
A container is addressed as <process>/<service> (e.g. infra/redis) in show, logs and wait.
Exit codes: 0 ok · 1 failed · 2 usage · 3 not running (send, wait).
`

func (a *App) help(_ []string) error {
	fmt.Fprint(a.Stdout, usageText)
	if a.engine == nil {
		dir := a.dir
		if dir == "" {
			dir, _ = a.Getwd()
		}
		a.engine, _ = a.Open(dir)
	}
	if e := a.engine; e != nil && len(e.Config().Processes) > 0 {
		fmt.Fprintf(a.Stdout, "\nProcesses: %s\n", strings.Join(e.Config().Names(), " "))
	}
	return nil
}
