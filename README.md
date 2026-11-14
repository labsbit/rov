# rov — Run, Observe, Verify

A dev process manager for humans **and** coding agents. rov runs every
long-running process of a project — API servers, web dev servers, workers,
docker compose infra, a one-off mock — in its own tmux session, so they outlive
your editor, show up in one TUI, and can be inspected and awaited from the CLI.

- **Run** — `rov up api`, `rov restart api worker`, or start anything ad-hoc with
  `rov spawn mock -- node mock.js`. No more invisible background shells.
- **Observe** — `rov status` shows state, PID, **detected** ports and uptime for
  every process (and the containers it owns); `rov logs`, `rov show`,
  `rov ports` ("who holds :8080?"). Logs keep colour, rotate, and keep the
  previous run.
- **Verify** — `rov wait api --port 8080` (or `--log 'ready'`, `--healthy`)
  blocks until it's actually ready instead of `sleep 5`; every command has
  `--json` and meaningful exit codes.

```
 rov ▸ myapp       ~/myapp       tmux rov-myapp-3f9a1c2e              ● 7  ▲ 0  ✗ 1  ○ 2   10:42:17
╭─ processes ──────────────────────────╮╭─ gateway ───────────────────────────────────────────────────╮
│▌● gateway                     :8080  ││ running · pid 41233 · up 2h13m · :8080 · cwd . · just gateway│
│▌  just gateway                       ││ search gateway                                                │
│ ● tracking              :8090 :8091  ││───────────────────────────────────────────────────────────────│
│   just tracking                      ││  1204  08:01:12 INF listening addr=:8080                      │
│ ● infra    up 2/2       :6379 :8123  ││  1205  08:01:13 INF provider ready name=brave                 │
│   just infra                         ││  1206  08:01:13 INF provider ready name=serper                │
│   ├ redis        healthy      :6379  ││  1207  10:14:55 WRN upstream slow provider=serper p95=812ms   │
│   └ clickhouse   healthy      :8123  ││  1208  10:15:02 INF search q="rust tui" took=212ms            │
│ ● api                         :3001  ││  …                                                            │
│   just api               ◆ changed   ││                                                               │
│ ○ panel    absent                    ││                                                               │
│   just panel                         ││                                                               │
│ ✗ worker-rust          crashed 101   ││                                                               │
│   just worker-rust          (:9101)  ││                                                               │
│ ── ad-hoc ────────────────────────── ││                                                               │
│ ● mock-stripe                :12111  ││                                                               │
│   node scripts/mock-stripe.js    4m  ││                                              ↓ 12 new lines   │
╰──────────────────────────────────────╯╰───────────────────────────────────────────────────────────────╯
 LIST  space start/stop  r restart  n new  e edit  J/K move  x remove  / search  a attach  ? help  q quit
```

## Install

Requirements: **tmux ≥ 3.2**. Linux gets full port detection (`/proc`); macOS
builds and runs, but doesn't detect ports yet. Docker is optional.

**With Go** (1.26+):

```sh
go install github.com/labsbit/rov/cmd/rov@latest
```

While the repo is private, tell Go not to use the public proxy:
`GOPRIVATE=github.com/labsbit/* go install github.com/labsbit/rov/cmd/rov@latest`.

**Prebuilt binary** (linux/macOS, amd64/arm64, from GitHub Releases):

```sh
curl -fsSL https://raw.githubusercontent.com/labsbit/rov/main/install.sh | sh
```

Installs to `~/.local/bin` (override with `ROV_INSTALL_DIR=…`, pin a version
with `ROV_VERSION=v0.1.0`).

**From a checkout:**

```sh
git clone git@github.com:labsbit/rov.git && cd rov
./install.sh            # builds from source when run inside the repo (or: make install)
```

Check it: `rov --version` → `rov 0.1.0 (github.com/labsbit/rov, rev …)`.

## Quick start

```sh
cd ~/myapp
rov init                 # writes a starter rov.yaml and gitignores .logs/
```

```yaml
# rov.yaml — order here is the order in `rov status` and the TUI
processes:
  infra:
    cmd: docker compose up --wait
    docker: myapp            # the compose project this process owns
  api:
    cmd: go run ./cmd/api
    description: API (:8080)
  web:
    cmd: npm run dev
    cwd: web
    env: { PORT: "3000" }
```

```sh
rov up all               # start everything
rov                      # the TUI: list + coloured live output, start/stop/edit from the keyboard
rov status               # the same as a table
rov wait api --port 8080 && curl localhost:8080/health
rov logs api --grep 'panic:'
rov restart api          # after a rebuild
rov spawn mock --wait -- node scripts/mock.js   # ad-hoc: shows up everywhere, removed with rov rm mock
```

rov finds the project by walking up from the current directory to the nearest
`rov.yaml` (else the git root), so one binary serves every repo, each with its
own tmux server. `-C <dir>` points it elsewhere.

## Commands

| | |
|---|---|
| `rov` | the TUI |
| `rov init` | write a starter `rov.yaml` |
| `rov up` / `restart` / `stop` / `down <name…\|all>` | start, restart, stop the process tree, remove the session |
| `rov spawn <name> [--cwd D] [--env K=V] [--replace] [--wait] -- <cmd…>` | start an ad-hoc process |
| `rov rm <name…> [--purge]` | remove ad-hoc processes (and their logs) |
| `rov status` · `show <name>` · `list` | what runs, everything about one, what's configured |
| `rov logs <name> [-n N] [-f] [--grep RE] [--prev]` | output; `--prev` = the run before the last start |
| `rov ports` | every listening TCP port and who owns it |
| `rov wait <name> [--port N \| --log RE \| --exit \| --healthy] [--timeout D]` | block until ready |
| `rov config add\|set\|mv\|rm …` | edit `rov.yaml`, keeping comments |
| `rov attach <name>` · `send <name> <text>` | interactive session · type a line into it |

Every read command takes `--json`. Exit codes: `0` ok, `1` failed or timed out,
`2` usage, `3` not running, `130` interrupted. `rov help` has the details.

## For coding agents

rov exists so agents stop starting servers as background shells. Give your
agent [`skills/rov/SKILL.md`](skills/rov/SKILL.md) (for Claude Code: copy it to
`.claude/skills/rov/SKILL.md`) and add one line to your `CLAUDE.md` /
`AGENTS.md`:

> Long-running processes (servers, watchers, mocks) start through `rov`
> (`rov spawn` / `rov up`), never a background shell. Use `rov wait`, not `sleep`.

## Docs

- [docs/SPEC.md](docs/SPEC.md) — the full spec: architecture, every command and
  output format, logs, ports, docker ownership, the TUI, decisions.
- [CONTRIBUTING.md](CONTRIBUTING.md) — build, test, package layout, releasing.
