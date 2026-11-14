# rov — specification

**rov** = **Run, Observe, Verify**: a dev process manager with a TUI.
This is the full spec — design, every command, file format and decision. For a
quick tour see the [README](../README.md); for agents, the
[skill](../skills/rov/SKILL.md).

Status: **implemented** (M1–M5, except macOS port detection and the optional hook).

## Install / quick start

```sh
go install github.com/labsbit/rov/cmd/rov@latest   # or ./install.sh, see the README
rov                  # the TUI for the project you are in
rov status           # the same as a table; rov help lists every command
```

- rov finds the project from the current directory (see
  [Project and config](#project-and-config)), so one binary serves every repo.
  Don't put a wrapper such as `exec rov -C ~/myapp "$@"` on PATH: it pins every
  terminal to that one repo and defeats the per-project design.
- After changing rov, reinstall it. Running processes keep going; see
  [Code layout and build](#code-layout-and-build).

## Problem

Coding agents start and stop the processes a test needs — infra, the API, the
gateway, a mock server — all the time. Started as background shells, they
become invisible: nobody knows what is still running, which port it holds,
where its output went, or why the next start fails with "address already in
use".

A shell script that keeps each service in a tmux session, with its output
mirrored to `.logs/<name>.log`, fixes half of this: the sessions outlive the
editor and an agent can read the log. What is still missing:

1. **Ad-hoc processes.** Only the hard-coded services exist. An agent that
   needs a one-off server has no rov-shaped way to start it, so it falls back to
   a background shell.
2. **Ports.** Nothing shows which ports a process — or a docker container it
   started — actually listens on.
3. **One view.** A human has to open one terminal tab per service to see output,
   and adding a service means editing a bash array.
4. **Agent ergonomics.** No machine-readable output and no way to block until a
   service is ready, so agents `sleep` and guess.

## Goals

- One TUI that shows every process rov runs in the project: name, command,
  state, ports on the left; the selected process's output, **in colour**, on the
  right; start, stop, restart and remove from the keyboard. It looks good: dense,
  aligned, technical.
- The default processes are **pre-configured** in `rov.yaml`, like
  `mprocs.yaml`, and can be **added, edited, reordered and removed from the
  TUI**.
- Agents start **anything** long-running through rov (`rov spawn`), and it
  appears in the TUI automatically.
- Ports are **detected**, never declared: from the process tree, and from docker
  containers the project started.
- Agents get context in one call: `rov status --json`, `rov show <name>`,
  `rov logs`, and `rov wait` instead of `sleep`.
- Logs are kept with a generous cap, never without bound.
- A **reusable tool**: any repo with an `rov.yaml` can use it.

## Non-goals

- Discovering processes started outside rov. If it was not started through
  rov, rov does not show it (except as a foreign holder in `rov ports`, and
  docker containers of the project — see [Docker](#docker)).
- Production supervision: no autorestart, health-based restarts or dependency
  ordering between services.
- Typing into a process from the TUI. `rov send` (CLI) and `a` (attach, below)
  cover the rare prompt.
- A machine-wide view across projects. One TUI shows one project.
- Per-line arrival timestamps. Lines are stored as the process printed them.
- An MCP server (possible later; the CLI with `--json` is the agent interface).

## Prior art — why build instead of adopt

| Tool | Close on | Missing for us |
|---|---|---|
| [dekit](https://dekit.run/docs) (successor of [mprocs](https://github.com/pvolok/mprocs)) | Background runner that outlives the terminal, list + output TUI, `dekit spawn` for ad-hoc tasks, `ls --json` | No ports. `dekit screen` returns the visible screen only — no log file or scrollback for agents. v0.10, young. |
| [process-compose](https://f1bonacc1.github.io/process-compose/) | Headless mode + `attach` TUI, REST API, per-process log files | No ports. Ad-hoc processes need a YAML `project update`. |
| [mcproc](https://github.com/kiwagu/mcproc) | Real listening ports per app, agent-first CLI + MCP, wait-for-ready | pm2 + Node runtime; agents may only start manifest apps; TUI undocumented; no adoption. |
| overmind / hivemind | tmux-backed Procfile runner | No TUI list, no ports, no ad-hoc. |

rov's tmux backend already gave what these runners provide (processes survive
the editor, a real PTY per process, attachable panes). The missing parts —
ports, docker, ad-hoc spawns, a TUI with config editing, `--json`, `wait` — were
smaller than migrating the repo, its skill and its terminal tabs to another tool
and then patching the same gaps there.

## Architecture

```
               rov (one Go binary)
   ┌──────────────┬──────────────┬──────────────┐
   │ CLI          │ TUI          │ rov __pipe   │  log writer, one per pane
   └──────┬───────┴──────┬───────┴──────┬───────┘
          └───── core ───┘              │
   ┌──────────────────────────────┐     │
   │ config · supervisor · ports  │     │
   │ docker · logs · configedit   │     │
   └───┬──────────┬──────────┬────┘     │
       │          │          │          ▼
   tmux server   /proc     docker CLI   .logs/<name>.log   plain, for agents
   -L rov-<…>    ports,    containers   .logs/<name>.ansi  colour, for the TUI
   one session   process   of this
   per process   tree      project
```

- **tmux is the supervisor**, as in the old script. rov has no daemon: the tmux
  server holds the processes, and rov state lives in tmux session options. The
  TUI is just another client — quitting it changes nothing.
- **One tmux server per project** (`tmux -L rov-<basename>-<hash8 of root>`),
  instead of the default server. Projects can't collide on names, and rov
  sessions stay out of the user's own `tmux ls`. `rov status --json` reports
  the name as `socket`.
- **CLI and TUI share one core package**, so a restart from the TUI and a
  restart from an agent go through the same code, including locks.
- **`rov __pipe`** (hidden subcommand) replaces the old script's `sed` as the
  `pipe-pane` target. It writes both log files and enforces the size cap
  (see [Logs](#logs)).
- Concurrent callers (two agents, or an agent and the TUI) are serialised per
  process with an `flock` on `<log_dir>/.lock/proc/<name>`; actions on different
  processes run in parallel. Writes to `rov.yaml` take `<log_dir>/.lock/config`
  (a process may be called `config`: the paths don't collide).
  A caller waits up to 30 s for a held lock, then fails with
  `busy: rov (pid N) restarting X; try again in a moment`.

### Where state lives

| What | Where | Lifetime |
|---|---|---|
| Pre-configured processes | `rov.yaml` at the project root (edited by hand, the TUI or `rov config`) | the repo |
| Ad-hoc process definition | session options `@rov_kind=adhoc`, `@rov_cmd`, `@rov_cwd`, `@rov_env` (a JSON array of `"K=V"`) | until `rov rm`, `rov down`, or the tmux server dies (reboot) |
| Command a config process was started with | `@rov_cmd`/`@rov_cwd`/`@rov_env` (as for ad-hoc) | session — compared with `rov.yaml` to flag "changed, restart to apply" |
| Stopped flag | `@rov_stopped` (as in the old script; set before rov signals the tree) | session |
| Start time | `@rov_started` (unix seconds) | session |
| Run identity | `@rov_run_id` (unix nanoseconds of the start) | session — the TUI tells a new run by it |
| Definition an ad-hoc process runs, while a different one is stored | `@rov_run_def` (JSON) | session, set by a TUI edit without restart, cleared by the next start — flags "changed" |
| Last ports seen | `@rov_last_ports` (`8080,8081`) | session — used to explain a port conflict after a stop or crash |
| Output | `<log_dir>/<name>.{log,ansi}` + rotated `.1` + previous run `.prev` | see [Logs](#logs) |
| Output of a detached rov | `<log_dir>/.lock/detached/<name>.log` | appended; see [Processes](#processes) (rov inside the tree it stops) |

Option values are percent-encoded (`%XX` for `%`, `$`, `\` and non-printable
bytes), so any command survives tmux's own escaping. No state file of rov's own exists, so nothing can drift from what tmux,
docker and `rov.yaml` know.

### Processes

- **States:** `running`, `stopped` (`rov stop`; not a failure), `exited`
  (status 0), `crashed` (non-zero status, text `crashed(N)`), `dead` (killed by
  a signal, status unknown), `absent` (no session).
- **Sessions** are created 220×50 so detached output doesn't wrap, with
  `remain-on-exit on`, so a crashed pane stays inspectable.
- **Shell:** commands run with the user's `$SHELL` (tmux's default-shell), as the
  old script did. A shell that isn't POSIX (fish, nu, …) is replaced by
  `/bin/sh`, because stored spawn commands are POSIX-quoted.
- **Stop** kills the whole tree: the pane's process group (`just → go run →
  binary` would otherwise orphan the binary that holds the port), plus children
  that left it with `setsid`/`setpgid` (found by session id and ppid). `TERM`,
  then `KILL` after 5 s. Leftovers of a pane that already died are stopped the
  same way, and a stop or restart reports them (`not running; stopped 1 process
  left over from its last run`). While one still holds a port, `status` warns
  `! worker-rust: :9101 is still held by pid 7712 (worker-search-rust), left over
  from its last run; rov restart worker-rust stops it`.
- A `setsid` child whose parent has exited is no longer in the tree (its parent
  is init now). It still counts as left over — `status` warns `… is still held
  by pid N (comm), started after X did: likely left over from its last run; rov
  restart X stops it`, and stop/restart/down stop it — when it listens on one
  of the process's last ports (`@rov_last_ports`), started at or after the
  process did (`@rov_started`), and is in no other session's tree. Anything else
  on those ports is only named (`is held by pid N (…, not managed by rov)`),
  never signalled.
- Pids are reused: stop signals a pid only while it is still the process it
  scanned (same start time in `/proc/<pid>/stat`), a process group only while
  one of the members it scanned is.
- If anything survives `KILL` (another user's process: permission denied), the
  action fails with `could not stop pid N (comm, permission denied)` and the
  process is not marked stopped.
- **rov inside the tree it stops** (`rov restart web` typed in web's pane, or
  run by an agent in it): stopping the tree would kill that rov halfway. Nothing
  is signalled; `up`/`restart`/`stop`/`down`/`rm` re-run themselves in a
  detached rov (new session) that starts once the first exits —
  ``web: this rov runs inside its process tree, so a detached rov (pid N) runs
  `rov restart web` once this one exits; output: .logs/.lock/detached/web.log``.
  `spawn --replace` and `config rm --stop` refuse instead. A detached rov that
  finds itself in the tree again (a subreaper in it adopted it) fails rather than
  detach twice.
- **Exit status.** tmux linked with libutempter loses some `SIGCHLD`s (it sets
  `SIGCHLD` to `SIG_DFL` while its helper runs for a pane that opened or
  closed), leaving the pane dead with no status and its process an unreaped
  zombie. rov then sends the server a `SIGCHLD`, which makes it reap and record
  the status; failing that it reads the status from the zombie
  (`/proc/<pid>/stat`). So a crash reads `crashed(N)`, not `dead`.

## Project and config

The project root is the nearest ancestor directory holding `rov.yaml`. Without
one, it's the git root, and only ad-hoc processes are available. `-C <dir>`
overrides it. `rov init` writes a commented starter `rov.yaml` at the git root
(or the `-C` dir) and adds the log directory to `.gitignore` if it's missing
there. It refuses when `rov.yaml` exists.

```yaml
# rov.yaml — order here is the order in `rov status` and the TUI
log_dir: .logs                # default; must be gitignored
log_max: 64MiB                # default; per file, see Logs
processes:
  infra:
    cmd: just infra
    description: Redis + ClickHouse (docker)
    docker: search-api-gateway-local     # compose project this process owns
  gateway:
    cmd: just gateway
    description: search gateway
  tracking:    { cmd: just tracking }
  panel:       { cmd: just panel }
  api:         { cmd: just api }
  web:         { cmd: just web }
  worker-go:   { cmd: just worker-go }
  worker-rust: { cmd: just worker-rust }
  fetch-py:    { cmd: 'just fetch-serve "fetch-resi,fetch-mobile"' }
```

| Field | Required | Meaning |
|---|---|---|
| `cmd` | yes | shell command run in the pane (`$SHELL -c`, see [Processes](#processes)) |
| `cwd` | no | relative to the root and inside it; default the root |
| `env` | no | map, passed with `new-session -e` / `respawn-pane -e` |
| `description` | no | shown in `rov list`, `rov show` and the TUI output header |
| `docker` | no | a compose project name. Its containers are shown under this process and their ports count as its ports (see [Docker](#docker)). |

| Top-level | Default | Meaning |
|---|---|---|
| `log_dir` | `.logs` | where logs and locks live; relative to the root, and inside it |
| `log_max` | `64MiB` | cap per log file before rotation; accepts `KiB`/`MiB`/`GiB` |

- Names: `[a-z0-9][a-z0-9_-]*`. They double as tmux session names, so they
  can't contain `:` or `.`. A name used in `rov.yaml` can't be spawned ad-hoc.
- The map's key order is the display order. It's preserved on every write (see
  [Editing the config](#editing-the-config)).
- The config is read on every CLI call and watched by the TUI. A changed `cmd`,
  `cwd` or `env` applies on the next start: a running process is never
  restarted by an edit. Instead it's flagged as changed — `◆ changed` in the TUI,
  `[changed]` in `rov status`, `"changed": true` plus `running_cmd` in `--json`.
- A broken file doesn't break rov: problems become status warnings, an entry
  with problems shows `[invalid rov.yaml entry]` and refuses to start, and
  unparsable YAML keeps the last good config in a running TUI.
- A running process whose name disappeared from the config is shown as ad-hoc,
  with its stored command, until it's removed.

**Environment caveat (unchanged from the old script, now documented):** a
process gets the environment of the tmux server — that is, of whoever started
the first rov session in the project — plus `env`/`--env`. An agent that wants
`PORT=4000` must pass `--env PORT=4000`; exporting it in its own shell does
nothing.

## CLI

All commands of the old script keep their name, arguments and default output.

```
rov                              open the TUI (the old script: status)
rov init                         starter rov.yaml + .gitignore entry
rov up <name...|all>             start if not running
rov restart <name...|all>        stop the process tree, start again
rov stop <name...|all>           stop the tree, keep the session
rov down <name...|all>           kill the session (ad-hoc: same as rm)
rov spawn <name> [--cwd DIR] [--env K=V]... [--replace] [--wait [--timeout D]] -- <cmd...>
                                 start an ad-hoc process
rov rm <name...> [--purge]       stop + remove ad-hoc processes (--purge: delete their logs)
rov status [--json]              one line per process (+ PORTS, UPTIME)
rov show <name> [--json] [-n N]  everything about one process + last N lines
rov logs <name> [-n N] [-f] [--grep RE] [--prev] [--color] [--json]
rov ports [--json]               every listening TCP port on the host, with its owner
rov wait <name> [--port N | --log RE | --exit | --healthy] [--timeout D]
rov config add|set|rm|mv …       edit rov.yaml (same writer as the TUI)
rov list [--json]                names, kind, commands and descriptions
rov attach <name>                interactive tmux session (human only; starts it if needed)
rov send <name> <text...>        type a line into the pane
rov help · rov version           usage (+ the project's process names) · version
```

Global flags go before the command: `-C <dir>`, `--version`/`-V`, `-h`/`--help`.

- `all` means every process in the project — config and ad-hoc — in config
  order, then ad-hoc by start time; `stop all` and `down all` go the other way
  (ad-hoc newest first, then `rov.yaml` bottom up), so dependents stop before
  the infra they use. It includes docker: `stop all` runs
  `compose stop` and `down all` runs `compose down` for every process with
  `docker:` (the old script only killed the sessions; volumes are kept). `rov rm all` is refused.
- `--` ends the flags. For commands that take no command line (all but `spawn`
  and `config add|set`), what follows it is names: `rov stop -- web` stops web.
- Every name is validated before any action runs, so a typo restarts nothing.
  Then each name runs in turn: one that fails is reported as
  `rov: <name>: <error>` on stderr, the others still run, and the command
  exits 1.
- `rov rm` refuses configured names (`rov down` them, or `rov config rm`).
- A docker container is addressed as `<process>/<compose service>`, e.g.
  `infra/redis`, in `show`, `logs` and `wait`.
- Durations (`--timeout`) are `90s`, `2m` or plain seconds.
- JSON is printed without HTML escaping (`&&`, `<` stay as they are).

Actions print `<name>: <message>`:

| Command | Messages |
|---|---|
| `up` | `started` · `already running` |
| `restart` | `restarted` |
| `stop` | `stopped` · `not running` · `not running; stopped N processes left over from its last run` |
| `down` | `session killed` · `no session`; ad-hoc: as `rm` |
| `spawn` | `started` · `replaced` |
| `rm` | `removed` · `removed, logs deleted` · `logs deleted` (no session, `--purge`) |

With `docker:`, `stop` adds `; containers stopped (docker compose stop)` and
`down` adds `; containers removed (docker compose down, volumes kept)`. After
`stop` the row reads `stopped` even when the process had already exited (a
`compose up --wait` one-shot), as its containers are.

### `rov spawn`

```sh
rov spawn mock-stripe -- node scripts/mock-stripe.js
rov spawn api-4000 --env PORT=4000 --cwd apps/api -- bun run dev
rov spawn vite --wait -- npm --prefix apps/web run dev     # returns once a port is open
rov spawn tee-dev -- 'npm run dev 2>&1 | tee /tmp/dev.out'  # one argument: a shell command as is
```

| Option | Meaning |
|---|---|
| `--cwd DIR` | relative to the project root, or absolute inside it. Default: the caller's cwd, which must be inside the root (else `cwd /x is outside the project root /y; pass --cwd`, exit 1). Stored relative to the root. |
| `--env K=V` / `-e` | repeatable |
| `--replace` | if `<name>` is running, stop it and start the new command under the same name |
| `--wait` | after starting, run `rov wait <name>` with the default condition (`--timeout D`, default 60 s) |

- A single argument after `--` is taken as a shell command as it is, so pipes
  and `&&` work. Several arguments are shell-quoted and joined, so
  `-- node "my script.js"` runs exactly that. The result is stored as
  `@rov_cmd`; a restart reruns it with the stored cwd and env.
- `<name>` already taken by a running process → error (exit 1) without
  `--replace`. If it's taken by an exited ad-hoc process, spawn replaces it.
  A name configured in `rov.yaml` is refused.
- Ad-hoc processes are never written to `rov.yaml` by spawn. A human can keep
  one with `s` in the TUI; anyone can with `rov config add`.
- After it exits, an ad-hoc process stays listed (`exited`/`crashed(N)`)
  with its log, until `rov rm`, `x` in the TUI, or `rov down`.

### `rov status`

```
SERVICE      STATE        PID     PORTS        UPTIME  COMMAND
infra        exited       -       6379,8123    -       just infra       [docker 2/2 healthy]
gateway      running      41233   8080         2h13m   just gateway
tracking     running      41390   8090,8091    2h13m   just tracking
worker-rust  crashed(101) -       (9101)       -       just worker-rust
api          running      41502   3001         2h10m   just api         [changed]
...
ad-hoc
mock-stripe  running      51002   12111        4m      node scripts/mock-stripe.js
docker (containers no process owns)
other/db     running      -       5433         -       container other-db-1
! worker-rust: :9101 is held by pid 7712 (worker-search-rust, not managed by rov)
```

- `PORTS`: detected listening TCP ports (see [Ports](#ports)), including the
  published ports of owned containers. For a process that is not running, the
  last ports seen are shown in parentheses.
- Tags after the command: `[docker r/t healthy|up|degraded]` (r of t owned
  containers running; `healthy` when all health checks pass, `up` when running
  without checks), `[changed]`, `[invalid rov.yaml entry]`.
- A `docker (containers no process owns)` section lists compose containers
  under the root that no `docker:` claims.
- With no processes at all, a hint line says how to start (`rov init`,
  `rov spawn`).
- Warning lines (`! …`) follow the table: rov.yaml problems, docker trouble
  (`docker: unavailable` only when some process sets `docker:`), and port
  conflicts. A last-seen port of a non-running process that someone else now
  holds turns "address already in use" into a one-line diagnosis:
  `! worker-rust: :9101 is held by pid 7712 (worker-search-rust, not managed by rov)`.
  A leftover of the process itself gets the `still held by …; rov restart X
  stops it` form (see [Processes](#processes)).
- `--json` emits:

```json
{
  "project": "myapp",
  "root": "/home/dev/myapp",
  "socket": "rov-myapp-44b6b33b",
  "processes": [
    {
      "name": "gateway",
      "kind": "config",
      "state": "running",
      "exit_code": null,
      "pid": 41233,
      "ports": [{ "port": 8080, "addr": "0.0.0.0", "pid": 41301, "comm": "api-gateway", "source": "proc" }],
      "last_ports": [8080],
      "started_at": "2026-11-07T08:01:12Z",
      "cmd": "just gateway",
      "cwd": ".",
      "env_keys": [],
      "description": "search gateway (:8080)",
      "changed": false,
      "containers": [],
      "log": ".logs/gateway.log"
    }
  ],
  "docker": { "available": true, "unowned": [] },
  "warnings": []
}
```

- `state` is the bare word (`"crashed"`); the status is in `exit_code` (set for
  `exited` and `crashed`).
- Fields present only when set: `running_cmd` (when `changed`), `signal` (a
  `dead` process), `docker` and `docker_state` (`up` or `degraded r/t`),
  `problems` (rov.yaml problems of the entry). `docker.error` says why docker
  is unavailable or failed; `docker.unowned` is `[{ "project", "containers" }]`.

A container entry is
`{ "name": "sag-redis", "project": "search-api-gateway-local", "service": "redis", "state": "running", "health": "healthy", "status": "Up 2 hours (healthy)", "ports": [{ "port": 6379, "addr": "127.0.0.1", "source": "docker", "container": "sag-redis" }] }`.

### `rov show <name>`

Everything an agent needs about one process in one call: state (plus docker
state or signal), pid, ports with their holders, uptime and start time, kind,
cmd (and the running cmd when changed), cwd, env keys (never values), log path,
docker project and containers, rov.yaml problems, and the last `-n` (default
30) log lines. `--json` returns the same fields as one `processes[]` entry plus
`lines`.

For a container (`rov show infra/redis`) it shows the container, its compose
project, state, docker's status text, ports and the last `docker logs` lines;
`--json` is the container entry plus `lines`.

### `rov logs`

The old script's `logs` (last 80 lines, `-n N`, `-f`), plus:

- Reads across rotation transparently (`.log.1` then `.log`), so `-n` and
  `--grep` see up to two caps' worth. `-n 0` prints everything.
- `--grep RE` filters lines (Go regexp). It combines with `-n` as "last N
  matching lines".
- `--prev` reads the previous run — the one before the last start. A crash's
  output survives the restart an agent does right after it.
- `--color` prints from the `.ansi` file (for humans; agents get plain text).
- `--json` returns `{ "name", "state", "run": "current|prev", "lines": [...], "total_lines" }`.
- `-f` is still human-only: the skill keeps forbidding it for agents.
- For `infra/redis` it reads `docker logs` of that container; `-n`, `--grep`,
  `-f` and `--json` work, `--prev` doesn't (docker keeps one log).

### `rov ports`

Every listening TCP socket on the host:

```
PORT   ADDR        PID    COMMAND             OWNER
22     0.0.0.0     ?      ?                   -        (not visible: other user)
3001   0.0.0.0     41530  bun                 api
5433   127.0.0.1   -      docker              -        (container other-db-1)
6379   127.0.0.1   -      docker              infra/redis
8080   0.0.0.0     41301  api-gateway         gateway
8090   127.0.0.1   41402  tracking-service    tracking
24100  ::          8812   node                -
```

- `OWNER` is the rov process whose tree holds the socket (leftovers of a dead
  pane included), `<process>/<service>` for an owned container port,
  `<compose project>/<service>` for an unowned container under the root, or `-`.
- Container ports come from docker, host-wide: the root-owned `docker-proxy`
  rows `/proc` shows for them are replaced by the container's data. Ports of
  other compose projects get `(container <name>)`.
- Sockets of other users show `?` (no `/proc/<pid>/fd` access without root).
- `--json` returns the rows as
  `{ "port", "addr", "pid"|null, "comm"|null, "owner"|null, "source": "proc|docker", "container"?, "visible" }`.

### `rov wait`

Blocks until a condition is true. Agents use it instead of `sleep`.

```sh
rov wait gateway                      # default condition, 60 s
rov wait gateway --port 8080
rov wait web --log 'ready in \d+ ms' --timeout 2m
rov wait infra --exit                 # one-shot: wait for exit 0
rov wait infra --healthy              # every owned container healthy
rov wait infra/clickhouse --healthy   # one container's health check passes
```

| Condition | True when |
|---|---|
| default | the process listens on at least one port. If it exits 0 first, `--exit` semantics apply. For a process with `docker:`, all owned containers must be running, and healthy where they have a health check (a one-shot that exited 0 keeps waiting for them); while the process still runs, each container must also have started at or after the process did (docker's `State.StartedAt`), so `rov restart infra; rov wait infra` doesn't return on the old containers while compose recreates them — if the start times can't be read, the process has to exit 0 first. For a container: it is up. |
| `--port N` | the process tree, or an owned container, listens on N |
| `--log RE` | a log line of the current run matches RE (processes only) |
| `--exit` | the process exited with status 0 (processes only) |
| `--healthy` | a container: docker reports `healthy` (one without a health check is an error). A process with `docker:`: every owned container is running, and healthy where it has a check (and, while the process runs, started after it, as for the default). |

Exit codes: `0` condition met · `1` timeout · `3` the process exited or crashed
before the condition was met, or wasn't started (`stopped`/`absent`: exits at
once with `start it first: rov up X`). On `1` and `3` it prints the last 20 log
lines, so the agent sees why without another call. It takes no lock, so it
never delays an action.

### `rov config`

The CLI twin of the TUI's editor, for agents and scripts. Same validation, same
writer, same comment-preserving output.

```sh
rov config add mock-s3 --after api --cwd tools/mock-s3 -- go run .
rov config add docs --last --cmd 'just docs' --desc 'docs server'
rov config set gateway --cmd 'just gateway-dev' --desc 'gateway, dev build'
rov config set api --env PORT=3002 --unset-env DEBUG
rov config set mock-s3 --rename s3            # refused while running
rov config mv worker-rust --after worker-go     # or --first / --last / --up / --down
rov config rm mock-s3                            # refuses while running; --stop stops it first
```

- `add` and `set` take the command either as `--cmd CMD` or after `--` (same
  rules as spawn), not both. `add` positions with `--after P`, `--first`,
  `--last` (default: last).
- `set` merges `--env` into the entry's env and drops `--unset-env` keys. When
  the process is running, it says `rov restart X` applies the change.
- `rm` and `set --rename` also kill the old name's leftover session, which would
  otherwise linger as an ad-hoc process.

Agents may also edit `rov.yaml` directly. The skill prefers `rov config`
because it validates names and positions.

### Exit codes (all commands)

`0` success · `1` the action failed (including timeouts, validation and
"busy") · `2` usage error · `3` process not running when it had to be (`send`,
`wait`) · `130` interrupted (Ctrl-C).

## Logs

### Capture: `rov __pipe`

`pipe-pane` runs `exec <abs path of rov> __pipe <root> <name>`. It reads the raw
pane output and writes two files line by line:

| File | Content | Reader |
|---|---|---|
| `<name>.log` | plain text, every escape sequence removed | agents, `rov logs`, grep |
| `<name>.ansi` | the same lines with **SGR (colour/bold/dim/underline) sequences kept**, everything else removed | the TUI, `rov logs --color` |

Both files always have the same lines in the same order, so line N in one is
line N in the other. Search and scroll positions translate directly.

Line discipline, the same for both:

- `\r\n` and `\n` end a line. A bare `\r` rewinds the line: only the text after
  the last bare `\r` is kept. A progress bar that redraws itself lands as its
  final state, not 400 copies.
- Clear-screen (`ESC[2J`, `ESC c`, Vite's restart clear) writes a marker line
  `── screen cleared ──` (dim in `.ansi`) instead of losing the boundary.
- Cursor movement, erase-line, alternate screen, OSC (titles; OSC 8 hyperlinks
  keep their text), DCS and charset selection are dropped.
- An unterminated line is flushed after 500 ms without output, so a prompt
  such as `Continue? [y/N]` is visible to agents and the TUI.
- Invalid UTF-8 is replaced with U+FFFD.

Programs see a real TTY in the pane, so they emit colour on their own. rov also
sets `FORCE_COLOR=1` and `CLICOLOR_FORCE=1` in the pane environment, unless the
config sets them, for tools that check other signals.

### Size and lifetime

- **Cap:** each file rotates when it reaches `log_max` (default **64 MiB**):
  `<name>.log` → `<name>.log.1`, replacing an older `.1`. The `.ansi` file
  rotates at the same line, so the pair stays aligned. Worst case per process:
  2 × 64 MiB plain + 2 × 64 MiB colour. At typical dev-server rates that's days
  of output.
- **Per run:** on every start the current files move to `<name>.prev.{log,ansi}`,
  replacing the older previous run, and `.1` files are dropped. The current run
  starts empty, as in the old script, but the run before it is one `--prev`
  away.
- **Cleanup:** `rov rm --purge` deletes an ad-hoc process's logs. Any rov
  command deletes logs in `log_dir` whose name is neither configured nor a live
  session and that were last written over 7 days ago. This only runs in a
  project with a `rov.yaml`, and only for names that have a `.ansi` file (which
  only `rov __pipe` writes), so other tools' files in `.logs` are safe.

## Ports

Ports are **detected only**. Nothing is declared in `rov.yaml`.

### Process tree (`/proc`)

1. **Tree.** Start from the pane pid, which tmux makes a session leader. The
   tree is every process whose session id equals the pane pid, plus every
   descendant by ppid (catches children that call `setsid`). It comes from
   `/proc/*/stat`.
2. **Sockets.** Listening sockets (`st == 0A`) come from `/proc/net/tcp` and
   `/proc/net/tcp6`, keyed by inode.
3. **Join.** Map the inodes to pids through the `/proc/<pid>/fd/*` →
   `socket:[inode]` symlinks of the tree.
4. **Refresh:** on every CLI call, and every 2 s in the TUI. One `/proc` scan
   serves all processes.

- Only TCP listeners are shown. UDP is not.
- `last_ports` is updated whenever the detected set is non-empty, and just
  before a stop.
- macOS (not implemented): the same result through
  `lsof -nP -iTCP -sTCP:LISTEN -p <pids>`, behind the same interface.

### Docker

Ports published by containers are held by `dockerd`/`docker-proxy`, outside any
process tree, so they come from docker instead.

- **Discovery:**
  `docker ps --all --no-trunc --format '{{json .}}' --filter label=com.docker.compose.project.working_dir`.
  It's kept when the `com.docker.compose.project.working_dir` label is the
  project root or below it. Each container gives `Names`, `State`,
  `HealthStatus`, the `com.docker.compose.project` / `.service` labels and
  `Ports`. Ports are parsed from `127.0.0.1:6379->6379/tcp`, host side only:
  unpublished container ports like ClickHouse's `9000/tcp` don't count.
- **Ownership:** a config process with `docker: <compose project>` owns that
  project's containers. They nest under it in the TUI, their ports are its
  ports, and its default `wait` waits for them. Containers of compose projects
  under the root that no process claims are listed in a read-only **docker**
  section, so they are never invisible.
- **State of an owning process:** `infra` runs `docker compose up --wait` and
  exits 0, but its containers keep running. With `docker:` set, the
  containers' state is shown next to the process state:
  - `rov status` keeps the process state (`exited`) and adds
    `[docker 2/2 healthy]`; `--json` has `docker_state`: `up` when all
    containers are running and healthy where checked, `degraded 1/2` when some
    are not, absent when the project has no containers.
  - The TUI row shows `up 2/2` / `degraded 1/2` instead of the bare `exited`
    (see [Layout](#layout)).
- **Actions on an owning process:**
  - `stop` runs `docker compose -p <project> stop`.
  - `down` runs `docker compose -p <project> down`. Volumes are kept; rov never
    passes `-v`.
  - Both also apply through `stop all` / `down all`.
  - Why `stop` keeps the containers: it mirrors rov's own split (`stop` keeps
    the session, `down` removes it), and the next `up --wait` only restarts the
    existing containers. It can't leave a stale container behind: compose
    compares each container's config hash on `up` and recreates the ones whose
    compose config changed. `just infra-down` stays as it is and equals
    `rov down infra`.
  - `up` and `restart` run `cmd` as usual.
  - Single containers (sub-rows in the TUI) support `space`, which runs
    `docker start` / `docker stop`.
- **Cost:** one `docker ps` per CLI call (2 s timeout), and every 5 s in the TUI.
  No docker binary or daemon means the feature is off. `status` says
  `docker: unavailable` only if some process sets `docker:`.

## TUI

`rov` with no arguments opens the TUI for the current project.

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

### Visual design

The look is that of a dense, technical instrument: everything aligned to a
grid, one accent colour, and state shown by glyph and colour together.

- **Grid and type.** Monospace columns throughout: names left-aligned, ports
  right-aligned and padded to a shared column, numbers in tabular alignment.
  Thin rounded borders (`╭╮╰╯`), `├ └` for container sub-rows, `·` as the field
  separator in header lines. No emoji.
- **Palette.** Each colour is defined as an adaptive light/dark pair (Lip Gloss
  adaptive colours), in truecolor where the terminal supports it, falling back
  to 256 and then 16 colours.

  | Role | Use | Dark / light |
  |---|---|---|
  | accent | focused border, selection bar `▌`, ports, mode pill | cyan `#5fd7d7` / `#00788a` |
  | running | `●`, `up` | green `#87d787` / `#2e7d32` |
  | warn | `▲ degraded`, `starting…`, `◆ changed`, conflict warnings | amber `#ffd75f` / `#a66a00` |
  | failed | `✗`, `crashed N` | red `#ff8787` / `#c62828` |
  | done | `✓ exited` | blue-grey `#8fa6c8` / `#4a6285` |
  | idle | `○`, secondary text, line numbers, unfocused borders | grey `#6c7086` / `#8a8f98` |
  | selection | background of the selected row | `#262a33` / `#e8ecf2` |

- **Glyphs.** `●` running/up · `▲` degraded · `○` stopped/absent ·
  `✓` exited 0 · `✗` crashed/dead · `◆` changed config ·
  braille spinner `⠋⠙⠹…` while starting or stopping. Colour never carries
  meaning on its own: a monochrome terminal still reads correctly.
- **Top bar.** `rov ▸ <project>`, root path (`~`-shortened), the tmux socket
  name (useful when attaching by hand), state counts, a clock. A problem
  (`✗ rov.yaml: …`, `✗ tmux: …`) follows the project name in red; a failed
  docker poll shows dimly as `docker: <error>` (the containers listed are
  from the last poll that worked). When it doesn't all fit, the path is
  shortened in the middle with `…` (down to 12 cells), then the docker note,
  the socket and the path are dropped, in that order.
- **Mode pill.** The bottom bar starts with the current mode in reverse accent —
  `LIST`, `OUTPUT`, `SEARCH`, `FORM`, `CONFIRM` — followed by the key hints for
  that mode, with keys in normal weight and labels dimmed. Transient messages
  (`gateway restarted`, errors in red) replace the hints for 3 s.
- **Motion.** Uptime ticks every second. Nothing else animates except spinners.

### Layout

- **Left pane** (≈ 38 columns, at most 40 % of the width): two rows per process.
  - Row 1: glyph, name, a short state word when it isn't simply running
    (`up 2/2`, `crashed 101`, `stopped`, `absent`), and ports right-aligned.
  - Row 2: the command, dimmed and truncated with `…`, plus `◆ changed`, uptime
    for ad-hoc processes, or last ports in parentheses when the process isn't
    running.
  - Owned containers follow as indented sub-rows: service name, health, port.
  - Order: config processes in config order, then the `ad-hoc` section by start
    time, then a `docker` section for unowned containers (only when there are
    any).
  - The selected row has the selection background and an accent `▌` bar. The
    focused pane has the accent border.
- **Right pane:** two header lines, then the output.
  - Line 1: state · pid · uptime · ports · cwd · cmd. An absent process
    whose last run left output says `absent · last run output`; a changed
    definition adds `◆ changed, r applies it`.
  - Line 2: the description, if any.
  - For a container sub-row, the output is `docker logs` streamed for that
    container.
- **Narrow terminals:** below 100 columns the top bar drops the path and the
  socket name. Below 80, the left pane drops row 2. Below 50, only one pane
  shows at a time, `h`/`l` switch between them, and the bottom bar always
  shows `h/l pane`.
- **Cell widths.** Every width (wrapping, truncation, padding, borders) is
  counted the way the renderer counts it: wcwidth, or grapheme clusters
  once the terminal reports Unicode core mode (2027; the TUI asks). tmux 3.4
  and later lay out emoji as graphemes without answering, so inside them
  (`TERM_PROGRAM=tmux`, `TERM_PROGRAM_VERSION` ≥ 3.4) the TUI counts
  graphemes from the start. A cluster wider than the row shows as `…`.

### Keys

| Key | List focused | Output focused |
|---|---|---|
| `j` / `k`, `↓` / `↑` | next / previous row (containers included) | scroll one line |
| `ctrl-d` / `ctrl-u` | — | half page down / up |
| `ctrl-f` / `ctrl-b`, `PgDn` / `PgUp` | — | page down / up |
| `g` / `G` | first / last row | top / bottom (`G` resumes follow) |
| `l`, `→`, `enter` | focus output | — |
| `h`, `←`, `esc` | — | focus list |
| `tab` | toggle focus | toggle focus |
| `space` | start if not running, stop if running | same, for the selected process |
| `r` | restart | restart |
| `x` | remove (ad-hoc only); confirm `remove mock-stripe? y/n` | same |
| `/` | search the log (focuses the output) | search the log; `n` / `N` next / previous match |
| `#` | toggle line numbers (on by default) | same |
| `w` | toggle soft wrap (on by default) | same |
| `p` | toggle between the current and previous run | same |
| `a` | attach: suspend the TUI, `tmux attach` the process, return on detach (`ctrl-b d`) | same |
| `n` | new configured process (form, inserted below the selected row) | — |
| `e` | edit the selected process (form) | — |
| `J` / `K` | move the selected config process down / up | — |
| `D` | delete the selected process from `rov.yaml`; confirm | — |
| `s` | save the selected ad-hoc process into `rov.yaml` (form, prefilled) | — |
| `?` | help overlay | help overlay |
| `q`, `ctrl-c` | quit the TUI — processes keep running | same |

- Mouse: the wheel over the output scrolls it (3 lines a notch); over the
  list it moves the selection. A click selects a row, or focuses the output.
- Actions never block the UI. They run in the background, and the row shows a
  spinner with `starting…` / `stopping…` until the next poll confirms the change.

### Output pane

- **Source:** `<log_dir>/<name>.ansi` — the same lines agents read in `.log`,
  with their colours. The TUI renders the SGR sequences itself, so ANSI-aware
  width, wrapping and truncation keep columns intact.
- **Follow mode:** while scrolled to the bottom, new lines appear live (the
  file is polled every 200 ms, across rotation). Scrolling up pauses follow; a
  `↓ N new lines` marker shows; `G` jumps back and resumes.
- **Scrollback:** the last 50 000 lines are held in memory for the selected
  process, read across `.ansi.1` and `.ansi`. Older lines are still on disk
  (`rov logs`). A paused view stays on its line while newer lines push older
  ones out; once its own line has left, it shows the oldest line kept with
  `↑ older lines left the scrollback`.
- **Line numbers** are those of the run's log, counted across `.ansi.1` and
  `.ansi` as `rov logs -n 0` prints them (also when only the last 50 000 are
  loaded). For a container they count the lines shown.
- **Lines:** trailing blanks are dropped (a TUI's output padded to the
  220-column pane doesn't wrap into empty rows). With wrap off a long line
  ends in `…`; there is no horizontal scrolling.
- **Search:** `/` matches against the plain text of each line and highlights
  matches in reverse video over the coloured line. The bottom bar shows
  `3/17`.
- Each process keeps its own scroll position, follow state and search while you
  move between processes.
- A restart moves the old run to `prev`. The pane clears and follows the new
  run; `p` shows the previous run. A new run is told by the process's run id
  (`@rov_run_id`, set at every start, so two starts within a second differ;
  for a session an older rov started: its pane pid and start second), so a
  start after `down` clears too, and a process restarted while another row
  was selected shows its new run, following, when selected again. A run
  that ends keeps its output; so does a process taken `down`.
- **Containers:** `docker logs --follow` ends when the container stops; the
  pane then shows `── stream ended (container stopped) ──` and keeps the
  lines. Once a docker poll after that sees the container running, the
  stream picks up again from the moment it ended (`--since`). Lines longer
  than 1 MiB are cut there.

### Editing the config

New, edit and save open a centred form over the TUI, built with
[huh](https://github.com/charmbracelet/huh):

```
╭─ new process ─────────────────────────────────────────────╮
│ Name         mock-s3                                      │
│ Command      go run ./tools/mock-s3                       │
│ Working dir  .                                            │
│ Env          AWS_REGION=eu-central-1                      │
│              (one KEY=VALUE per line)                     │
│ Description  local S3 for upload tests                    │
│ Docker       — none —                          ▾          │
│ Position     after api                         ▾          │
│ [x] start after saving                                    │
│                                                           │
│                               esc cancel   ctrl-s save    │
╰───────────────────────────────────────────────────────────╯
```

- **Fields:**
  - **Position** defaults to "after the selected row" and offers `first` plus
    "after <each process>".
  - **Working dir** is empty for the root (the placeholder says `.`); a path
    relative to the root, or absolute inside it, is stored relative.
  - **Docker** offers `none`, the current value (even without containers),
    and the compose projects found under the root — every container docker
    lists, stopped ones included — plus those other entries name.
- **Validation is inline**, shown under the field in the failed colour:
  - name pattern, and unique among config and ad-hoc names
  - command not empty
  - working dir exists inside the root (one line: `no such directory: x`)
  - env lines are `KEY=VALUE` with a valid key
- **Edit rules:**
  - A running process can't be renamed: the form says "stop it first". This
    is checked again when saving, in case it was started meanwhile.
  - A changed `cmd`/`cwd`/`env` on a running process is saved and the row shows
    `◆ changed` until `r`.
  - Editing an ad-hoc process changes its stored definition. If it's running,
    the form offers `restart now`; without it the row shows `◆ changed`
    until a restart (the definition it runs is kept as `@rov_run_def`).
- **`J`/`K`** reorder in place and write immediately. **`D`** asks `delete api
  from rov.yaml? y/n`. If the process is running, it asks
  `api is running — stop and delete? y/n`.
- **Writing `rov.yaml`:**
  - The file is edited as text, guided by its YAML node tree
    (`gopkg.in/yaml.v3`): yaml.v3's encoder re-indents the document and drops
    comment and value alignment, so it can't round-trip byte for byte. The
    tree only locates the entry and field an edit touches; that text is
    spliced, and every other line is copied unchanged. Comments, key order and
    the style of untouched entries survive. A new entry uses the flow style
    `{ cmd: … }` when it only has `cmd`, and block style otherwise.
  - The write is atomic (temp file + rename) and runs under the config lock.
  - If the file changed on disk since the TUI last read it, the edit is
    re-applied to the fresh file. It's refused only if the same entry changed,
    in which case the form stays open with `rov.yaml changed on disk —
    reloaded` and the user's input kept.
- The CLI `rov config` and the TUI share this writer, so an agent's
  `rov config add` appears in an open TUI within a second, through the file
  watch.

### Live updates

The TUI polls the tmux server every 1 s (states, new and removed sessions),
`/proc` every 2 s (ports) and docker every 5 s, and watches `rov.yaml`. A
process an agent spawns appears within a second, at the end of the ad-hoc
section. Selection never jumps: if the selected row is removed, the selection
moves to its neighbour. A poll that fails keeps the last result (tmux: the
top bar says `✗ tmux: …`; docker: `docker: …`, dimly) — rows don't vanish
over one slow `docker ps`; only docker becoming unavailable clears them.
While rov stops a process (stop, restart, down, rm) the session is flagged
stopped before the signal, so a poll in between reads `stopped`, not a
crash.

## Agent adoption

The value depends on agents actually using rov. Three layers, from soft to hard:

1. **Skill** — [`skills/rov/SKILL.md`](../skills/rov/SKILL.md) (copy it into a project's `.claude/skills/rov/`):
   - Any process that keeps running (a server, a watcher, a mock) goes through
     `rov spawn` or `rov up`, never a background shell or `&`.
   - Use `rov wait` instead of `sleep`.
   - Read state with `rov status` / `rov show` / `rov logs` (`--prev` after a
     crash and restart); `rov ports` for "address already in use".
   - Clean up your own spawns with `rov rm` when the task is done.
   - Add a permanent service with `rov config add` only when the user asks.
   - Never `attach`, `logs -f` or a bare `rov` (the TUI); never `stop all` /
     `down all` casually — they stop or remove the docker containers.
2. **CLAUDE.md / AGENTS.md** — one line next to the rebuild/restart rule: long-running
   processes start through `rov`.
3. **Optional hook** (not implemented; decide after v1): a `PreToolUse` hook on
   Bash that warns when a command looks like a long-running server started in
   the background (`run_in_background` with `just <service>`, `npm run dev`,
   `bun run dev`, `go run`), pointing at `rov spawn`.

## Code layout and build

```
rov/                        module github.com/labsbit/rov
├── cmd/rov/main.go
├── internal/config/        rov.yaml load/validate + project-root resolution, rov init
├── internal/configedit/    text edits guided by the yaml node tree (add/set/rm/mv), atomic write, conflict re-apply
├── internal/tmux/          tmux CLI wrapper: sessions, options (percent-encoded), pipe-pane, attach
├── internal/pipe/          __pipe: line discipline, SGR filter, rotation, prev runs
├── internal/ports/         /proc scan (linux); other systems: no ports yet
├── internal/docker/        docker ps / compose stop|down / logs, label filter, port parse
├── internal/logs/          tail, grep, follow across rotation, stale-log GC
├── internal/shell/         POSIX quoting of spawn arguments
├── internal/core/          the engine: status composition, up/stop/restart/spawn/rm/wait + flock; shared by cli and tui
├── internal/cli/           commands, text + --json output, exit codes
├── internal/tui/           Bubble Tea model, viewport, huh forms, Lip Gloss theme
└── internal/testenv/       integration tests: throwaway tmux server, temp project, helper server
```

- Go with [Bubble Tea](https://github.com/charmbracelet/bubbletea),
  [Bubbles](https://github.com/charmbracelet/bubbles) (viewport, spinner),
  [Lip Gloss](https://github.com/charmbracelet/lipgloss),
  [huh](https://github.com/charmbracelet/huh) (forms) and
  [x/ansi](https://github.com/charmbracelet/x/tree/main/ansi) (escape parsing,
  ANSI-aware width and wrap). The docker integration shells out to the `docker`
  CLI: no SDK dependency.
- tmux ≥ 3.2 is required (`-e` on `new-session` / `respawn-pane`).
- `make install` → `go install ./cmd/rov` into `$(go env GOBIN)` or
  `~/go/bin`. A bare `rov` then works in any terminal and any project.
- A rebuild replaces the binary that running `__pipe` writers were started
  from. They keep running the old code until the next start of their process,
  which is harmless because the file formats are versioned by this spec.

### Migrating from `svc`

rov started as `svc` inside one monorepo. Moving a project over:

1. With the old binary, stop everything it runs: `svc down all` (rov uses its
   own tmux server, `rov-…`, and can't see `svc-…` sessions).
2. `git mv svc.yaml rov.yaml` — the format is unchanged.
3. `rov up …`. Old logs in `<log_dir>` are picked up as they are.

## Testing

```sh
go test ./...                    # unit + integration (needs tmux; skipped without it)
go test -tags dockertest ./...   # also the docker tests (needs a docker daemon)
go test ./internal/cli -update   # rewrite golden files (also internal/configedit)
```

- **Unit:**
  - `rov.yaml` parsing and validation; project-root resolution.
  - `configedit` round-trips against golden files: comments, order and style
    kept; add/set/rm/mv; conflict re-apply. Fuzzed.
  - `/proc` parsing against fixture files (`tcp`, `tcp6`, `stat`, fd links),
    including IPv6 and `setsid` children.
  - `docker ps` JSON and `Ports` string parsing, including unpublished ports
    and IPv6 binds.
  - `__pipe` line discipline: byte streams in, both files out — CR rewinds,
    clear-screen markers, SGR kept, everything else stripped, the 500 ms partial
    flush, line alignment across rotation. Fuzzed.
  - Shell-quoting of spawn arguments; JSON and text output golden files.
- **Integration** (a real tmux on a throwaway socket `-L rov-test-<rand>`, with
  a tiny Go test helper that listens on a port and prints coloured lines):
  - spawn → status shows the port → wait returns → stop frees the port
  - restart while a grandchild holds the port (the `go run` case)
  - crash → `crashed(N)` plus last-ports warning → restart → `logs --prev` shows
    the crash
  - two concurrent restarts serialise
  - an exited ad-hoc process survives until `rm`
  - rotation at a tiny `log_max`
  - docker tests behind the `dockertest` build tag, run only where docker
    exists: a one-service compose project, ownership, `stop`, `--healthy`
- **TUI:** [teatest](https://github.com/charmbracelet/x/tree/main/exp/teatest)
  golden snapshots at 140×35 and 70×20, in both a dark and a light profile:
  - selection, focus switching, follow/pause, search highlight over colour
  - a spawned process appearing, container sub-rows
  - the remove confirmation
  - the new/edit form with a validation error
  - `J`/`K` reorder reflected in `rov.yaml`

## Milestones

| | Scope | Done when |
|---|---|---|
| **M1 — parity** | Go port of the old bash CLI; `rov.yaml`; per-project tmux server; `__pipe` with colour file, rotation and `prev` | every old command behaves as before against the real services |
| **M2 — agents** | `spawn`, `rm`, `show`, `/proc` ports, docker discovery and ownership, `PORTS`/`UPTIME` columns, `ports`, `wait`, `--json`, `--grep`, `--prev`, `rov config`, locks; skill + CLAUDE.md update | an agent can spawn a server, wait for its port, read its log and remove it using only rov; `rov status` shows infra's container ports |
| **M3 — TUI** | visual design, layout, keys, coloured output with follow/scroll/search, container sub-rows, live updates, attach | the human workflow needs no per-service terminal tabs |
| **M4 — TUI config** | new/edit/move/delete/save forms; file watch | a new service is added, positioned and started without leaving the TUI, and `rov.yaml` keeps its comments |
| **M5 — polish** | mouse, narrow layouts; macOS `lsof` and the optional hook not done | — |

## Decisions log

| Question | Decision |
|---|---|
| Build or adopt (dekit, process-compose, mcproc) | Build: extend the bash script, keep tmux as the supervisor |
| Scope | Reusable tool, configured per project by `rov.yaml` |
| Ports | Detected only — `/proc` for process trees, `docker ps` for containers |
| Stack | Go + Bubble Tea |
| Ad-hoc lifetime | Stays listed until removed; never written to config by `spawn` |
| View | Current project only |
| Agent interface | CLI with `--json`, `rov wait`, updated skill; no MCP |
| Output colours | Yes — a separate `.ansi` log with SGR kept |
| Log cap | 64 MiB per file, one rotation, previous run kept |
| Docker | Yes — discovered by compose labels, owned via `docker:` |
| Config editing | In the TUI and with `rov config`, comment-preserving |
| Arrival timestamps | Not needed |
| `rov stop` on a docker-owning process | `compose stop`; `rov down` → `compose down`; never `-v` |
| `stop all` / `down all` | Include docker-owning processes, so they stop / remove the containers |
| Spawn command after `--` | One argument: a shell command as is; several: quoted and joined |
| Shell | The user's `$SHELL`, as before; `/bin/sh` for non-POSIX shells |
| JSON `state` | The bare word; the status is in `exit_code` |
| A batch where one name fails | The others still run; exit 1 |
