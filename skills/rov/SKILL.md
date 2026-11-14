---
name: rov
description: Run, observe and verify long-running or interactive terminal programs through rov — the project's dev services (rov.yaml, including docker compose infra), ad-hoc servers, watchers and mocks, and CLI programs that keep running or prompt for input (a REPL, a CLI under test, an installer asking questions). Start them, read their output, type into them, wait until they are ready, and find out who holds a port. Use whenever you would otherwise start something in the background or need a terminal that stays open — starting or restarting a service, running anything that keeps running, driving an interactive CLI, checking whether something is running, picking up a rebuild, reading a service's log after a failed request, or diagnosing "address already in use".
---

# rov — Run, Observe, Verify

rov runs every long-running process of the project in its own tmux session
(a per-project tmux server), so processes outlive the editor and their output
lands in `.logs/<name>.log`. Spec: https://github.com/labsbit/rov/blob/main/docs/SPEC.md

Configured names: `rov list`. Not installed? `go install github.com/labsbit/rov/cmd/rov@latest`.

## Rules

- **Anything that keeps running goes through rov** — never a background shell,
  `&`, `nohup`, or your tool's background/async mode. Configured service: `rov up <name>`
  (not its `cmd` directly: invisible to rov, and it fights the rov one for
  the port). Anything else: `rov spawn <name> -- <cmd>`.
- **Wait, don't sleep:** `rov wait <name>` (or `rov spawn … --wait`).
- **Clean up your own spawns** with `rov rm <name>` when the task is done. Leave
  configured services in the state you found them unless asked.
- **Never** run `rov attach`, `rov logs -f` or a bare `rov` (the TUI) — they
  block forever; they are for the human.
- **Never `rov stop all` / `rov down all` casually.** They include processes
  that own docker containers (`docker:` in rov.yaml): `stop` runs
  `docker compose stop`, `down` runs `docker compose down` (containers removed,
  volumes kept). Stop the names you need.
- `rov config add` (a permanent entry in `rov.yaml`) only when the user asks.
- **Env:** a process gets the tmux server's environment plus `--env`/`env:`.
  Variables exported in your shell do **not** reach it: `rov spawn api-4000
  --env PORT=4000 -- bun run dev`.
- After a rebuild of compiled code, `rov restart <name...>`: services don't pick
  up changes on their own.

## Actions

| Action | Command |
|---|---|
| What is running (+ ports, warnings) | `rov status` · `rov status --json` |
| Everything about one process + last 30 lines | `rov show api` (`--json`, `-n N`) |
| Start / restart (pick up a rebuild) | `rov up api` · `rov restart api worker` |
| Stop, keep the session / remove the session | `rov stop api` · `rov down api` |
| Start an ad-hoc process | `rov spawn mock-stripe -- node scripts/mock-stripe.js` |
| … in a dir, with env, and wait for its port | `rov spawn web2 --cwd apps/web --env PORT=4199 --wait -- npm run dev` |
| Replace a running ad-hoc process | `rov spawn mock-stripe --replace -- node scripts/mock-stripe.js --v2` |
| Remove it (and its logs) | `rov rm mock-stripe` (`--purge`) |
| Wait until ready | `rov wait api` · `--port 8080` · `--log 'ready in'` · `--timeout 2m` |
| Wait for a docker-owning process's containers | `rov wait infra` · `rov wait infra/postgres --healthy` |
| Last 80 lines / last N / matching | `rov logs api` · `-n 200` · `--grep 'panic:'` (Go regexp) |
| Output of the run before the last start | `rov logs api --prev` |
| A container's log | `rov logs infra/redis -n 50` |
| Who holds a port ("address already in use") | `rov ports` · `rov ports --json` |
| Answer a prompt in a running process | `rov send api y` |
| Names, commands, descriptions | `rov list` |

- `spawn`: one argument after `--` is run as a shell command as is
  (`-- 'npm run dev | tee out.txt'`); several are quoted and joined. `--cwd` is
  relative to the repo root (an absolute path inside it works too); default
  your cwd, which must then be inside the repo. A taken running name fails
  without `--replace`; configured names can't be spawned.
- `up` is idempotent (`already running`); use `restart` for a fresh process.
- Multi-name actions check every name first (a typo runs nothing), then run
  each; one failure doesn't stop the others, but the exit is 1.
- After a crash, read `rov logs <name> --prev` if you already restarted it — the
  restart moved the crash's output there.
- `status` warnings (`! …`) explain port conflicts, e.g. `:9101 is held by pid
  7712 (… not managed by rov)` or `still held by … ; rov restart X stops it`.

## Interactive CLIs (a terminal that stays open)

Your shell tool runs one command and waits for it to end. A program that keeps
running or asks questions goes in a rov session instead — it is a real
terminal (tmux pane) you can type into and read back:

```sh
rov spawn repl -- python3 -i            # or: node, psql "$DATABASE_URL", ./mycli interactive
rov wait repl --log '>>> '              # until the prompt shows (Go regexp)
rov send repl 'print(6*7)'              # types the line + Enter
rov logs repl -n 20                     # read what it printed
rov rm repl                             # done: stop it and remove the session
```

- `send` types one line followed by Enter; it can't send bare control keys.
  To interrupt, `rov stop <name>` (or `rov restart` for a fresh run).
- `logs` is the pane's output as text (colour stripped; `--color` keeps it).
  Line-based programs read well; full-screen TUIs (vim, htop) don't — use their
  non-interactive flags instead.
- A CLI that finishes on its own still works: `rov wait <name> --exit` blocks
  until it ends (exit 0 if it succeeded), then read `rov logs`.

## States (`status`)

| State | Meaning |
|---|---|
| `running` | alive; `PID` is the pane's pid |
| `exited` | finished with status 0 — normal for a process like `docker compose up --wait`, which returns once the containers are healthy (see its `[docker 2/2 healthy]` tag) |
| `crashed(N)` | finished with status N — read the log (JSON: `"state": "crashed"`, `"exit_code": N`) |
| `dead` | killed by a signal |
| `stopped` | `rov stop` was used; not a failure |
| `absent` | no session: never started, or `rov down` |

Tags: `[changed]` (rov.yaml differs from what runs: restart to apply),
`[invalid rov.yaml entry]`, `[docker r/t healthy|up|degraded]`.

## Exit codes

`0` ok · `1` failed (incl. `wait` timeout, and `busy:` — another rov is acting
on that process; retry) · `2` usage · `3` not running (`send`; `wait` when the
process ended or was never started) · `130` interrupted. A failed `wait` prints
the last 20 log lines.
