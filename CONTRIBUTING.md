# Contributing to rov

What rov does and why, and every command and file format:
[docs/SPEC.md](docs/SPEC.md). Agent usage: [skills/rov/SKILL.md](skills/rov/SKILL.md).

## Build and test

```sh
make install                     # go install ./cmd/rov → $(go env GOBIN) or ~/go/bin
make test                        # unit + integration (real tmux on a throwaway socket; skipped without tmux)
make test-docker                 # also docker: a one-service compose project (needs a docker daemon)
make lint                        # gofmt + go vet
make update-golden               # rewrite golden files
go test ./internal/pipe -fuzz FuzzDiscipline           # also: ./internal/configedit -fuzz FuzzApply
```

Reinstall after a change. Processes that are already running keep going; their
`rov __pipe` log writers switch to the new binary on their next start.

Try a dev build with read-only commands only against a project you use
(`go run ./cmd/rov -C ~/myapp status`, also `list`, `ports`, `show`, `logs`):
actions there (`up`, `stop`, `down`, `restart`, `rm`, `spawn`, `config`) hit the
services people are using. Use the tests or `-C <temp dir>` for those.

## Packages

| Package | Role |
|---|---|
| `cmd/rov` | `main`: wires `cli.Main` to `tui.Run` |
| `internal/cli` | commands, flags, text and `--json` output, exit codes; golden files in `testdata/` |
| `internal/core` | the engine shared by CLI and TUI: composes a `Status` from tmux, `/proc`, docker and `rov.yaml`; actions under per-process flocks; `wait`; housekeeping (log GC). Start at `doc.go` |
| `internal/config` | `rov.yaml` parse/validate, project-root resolution, socket name, `rov init` |
| `internal/configedit` | comment-preserving edits of `rov.yaml` (text spliced, located through the yaml.v3 node tree), atomic write, conflict re-apply |
| `internal/tmux` | thin tmux CLI wrapper bound to one `-L` socket; session options (percent-encoded), pipe-pane, respawn, attach |
| `internal/pipe` | `rov __pipe`: terminal byte stream → aligned `.log` / `.ansi`, rotation, `.prev` runs |
| `internal/logs` | tail / grep / follow across rotation; stale-log GC |
| `internal/ports` | `/proc` scan: listening TCP sockets → pids → process trees (Linux only) |
| `internal/docker` | `docker ps` discovery by compose labels, port parsing, `compose stop`/`down`, `docker logs` |
| `internal/lock` | `flock(2)` file locks |
| `internal/shell` | POSIX quoting of spawn arguments |
| `internal/tui` | Bubble Tea TUI (`rov` with no arguments) |
| `internal/testenv` | integration-test support: throwaway tmux server, temp project, the real rov binary, a helper server |

## Releasing

Bump `Version` in `internal/cli/help.go`, then tag and push:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The `release` workflow runs GoReleaser (`.goreleaser.yaml`): linux/darwin ×
amd64/arm64 tarballs plus `checksums.txt`, which `install.sh` downloads.
