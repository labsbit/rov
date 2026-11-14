package core

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/pipe"
	"github.com/labsbit/rov/internal/ports"
	"github.com/labsbit/rov/internal/tmux"
)

// Inputs are the independent sources a Status is built from. The TUI
// refreshes each at its own rate and composes whenever one changes; the CLI
// gathers all of them once (Engine.Status). Engine.Inputs fills the
// project fields.
type Inputs struct {
	Root, Project string
	Socket        string
	// Config is the rov.yaml in effect and ConfigErr its problems.
	Config    *config.Config
	ConfigErr error
	// LogDir is the absolute log directory.
	LogDir string
	// Sessions come from Engine.Sessions (tmux, cheap: poll every 1 s).
	Sessions []tmux.Session
	// Ports comes from Engine.ScanPorts (/proc, every 2 s). nil means
	// unknown: processes then show no ports.
	Ports *ports.Snapshot
	// Containers and DockerErr come from Engine.Containers (docker, every
	// 5 s). An error wrapping docker.ErrUnavailable means no docker.
	Containers []docker.Container
	DockerErr  error
	// Now is the time the status describes.
	Now time.Time
}

// Compose builds the Status from its inputs. It is pure: it reads nothing and
// writes nothing, so it is cheap to call on every poll and easy to test.
// Persisting newly seen ports is a separate step (Engine.RecordPorts).
func Compose(in Inputs) *Status {
	st := &Status{Project: in.Project, Root: in.Root, Socket: in.Socket, Time: in.Now}
	cfg := in.Config
	if cfg == nil {
		cfg = config.Default("")
	}
	st.Warnings = append(st.Warnings, configWarnings(in.ConfigErr)...)

	var listeners []ports.Listener
	if in.Ports != nil {
		listeners = in.Ports.Listeners()
	}
	sessions := map[string]*tmux.Session{}
	for i := range in.Sessions {
		s := &in.Sessions[i]
		if config.ValidName(s.Name) == nil {
			sessions[s.Name] = s
		}
	}
	containers := convertContainers(in.Containers)
	owned := map[string]bool{}
	for _, p := range cfg.Processes {
		if p.Docker != "" {
			owned[p.Docker] = true
		}
	}

	c := composer{in: in, listeners: listeners, containers: containers}
	for _, cp := range cfg.Processes {
		s := sessions[cp.Name]
		p := c.process(configDefinition(cp), s)
		p.Description = cp.Description
		p.Docker = cp.Docker
		p.Problems = problemsFor(in.ConfigErr, cp.Name)
		if s != nil && s.Option(OptCmd) != "" {
			if run := storedDefinition(*s); !sameDefinition(run, configDefinition(cp)) {
				p.Changed = true
				p.RunningCmd = run.Cmd
			}
		}
		if cp.Docker != "" {
			c.addContainers(&p, cp.Docker)
		}
		c.finish(&p)
		st.Processes = append(st.Processes, p)
		delete(sessions, cp.Name)
	}

	var adhoc []tmux.Session
	for _, s := range sessions {
		adhoc = append(adhoc, *s)
	}
	slices.SortFunc(adhoc, func(a, b tmux.Session) int {
		if c := startedAt(a).Compare(startedAt(b)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	for i := range adhoc {
		s := &adhoc[i]
		def := storedDefinition(*s)
		def.Kind = KindAdhoc
		p := c.process(def, s)
		if run, ok := decodeRunDef(s.Name, s.Option(OptRunDef)); ok && !sameDefinition(run, def) {
			p.Changed = true
			p.RunningCmd = run.Cmd
		}
		c.finish(&p)
		st.Processes = append(st.Processes, p)
	}

	st.Docker = dockerStatus(in, containers, owned)
	if !st.Docker.Available && len(owned) > 0 {
		st.Warnings = append(st.Warnings, "docker: unavailable — owned containers can't be shown ("+st.Docker.Error+")")
	} else if st.Docker.Error != "" && st.Docker.Available {
		st.Warnings = append(st.Warnings, "docker: "+st.Docker.Error)
	}
	st.Warnings = append(st.Warnings, conflictWarnings(st, in.Ports, listeners)...)
	return st
}

type composer struct {
	in         Inputs
	listeners  []ports.Listener
	containers []Container
}

// process builds the parts of a Process common to both kinds.
func (c *composer) process(def definition, s *tmux.Session) Process {
	p := Process{
		Name:    def.Name,
		Kind:    def.Kind,
		Cmd:     def.Cmd,
		Cwd:     def.Cwd,
		EnvKeys: envKeys(def.Env),
		Log:     relTo(c.in.Root, pipe.Paths(c.in.LogDir, def.Name).Log),
		State:   StateAbsent,
	}
	if s == nil {
		return p
	}
	p.HasSession = true
	p.PaneID = s.PaneID
	p.StartedAt = startedAt(*s)
	p.RunID = runID(*s)
	p.storedLastPorts = decodePorts(s.Option(OptLastPorts))
	switch {
	case !s.Dead:
		p.State = StateRunning
		p.PID = s.PanePID
	case s.Option(OptStopped) == "1":
		p.State = StateStopped
	case s.DeadStatus == nil:
		p.State = StateDead
		p.Signal = s.DeadSignal
	case *s.DeadStatus == 0:
		p.State = StateExited
		p.ExitCode = s.DeadStatus
	default:
		p.State = StateCrashed
		p.ExitCode = s.DeadStatus
	}
	if c.in.Ports != nil {
		p.Ports = portsOf(c.listeners, pidSet(sessionTree(c.in.Ports, s)))
	}
	return p
}

// addContainers attaches the containers of a compose project to p.
func (c *composer) addContainers(p *Process, project string) {
	for _, ct := range c.containers {
		if ct.Project == project {
			p.Containers = append(p.Containers, ct)
		}
	}
	if len(p.Containers) == 0 {
		return
	}
	running, up := 0, true
	for _, ct := range p.Containers {
		if ct.State == "running" {
			running++
		}
		if !ct.Up() {
			up = false
		}
		p.Ports = append(p.Ports, ct.Ports...)
	}
	sortPorts(p.Ports)
	if up {
		p.DockerState = "up"
	} else {
		p.DockerState = fmt.Sprintf("degraded %d/%d", running, len(p.Containers))
	}
}

// finish sets LastPorts: what is listening now, else what was stored.
func (c *composer) finish(p *Process) {
	if now := p.PortNumbers(); len(now) > 0 {
		p.LastPorts = now
	} else {
		p.LastPorts = p.storedLastPorts
	}
}

// sessionTree returns the pids of the process tree of a session's pane (see
// ports.Snapshot.Tree). For a dead pane it returns what is left of the tree
// (processes that outlived the pane), without the pane pid itself: tmux has
// reaped it, so a live process with that pid is someone else.
func sessionTree(snap *ports.Snapshot, s *tmux.Session) []int {
	if snap == nil || s.PanePID <= 0 {
		return nil
	}
	if !s.Dead {
		return snap.Tree(s.PanePID)
	}
	if snap.Alive(s.PanePID) {
		// The pid was reused; leftovers would have kept it reserved (their
		// session id), so there are none.
		return nil
	}
	return slices.DeleteFunc(snap.Tree(s.PanePID), func(pid int) bool { return pid == s.PanePID })
}

func pidSet(pids []int) map[int]bool {
	m := make(map[int]bool, len(pids))
	for _, p := range pids {
		m[p] = true
	}
	return m
}

// portsOf returns the listeners held by pids, one per port+addr (the lowest
// pid), as proc ports.
func portsOf(ls []ports.Listener, pids map[int]bool) []Port {
	if len(pids) == 0 {
		return nil
	}
	var out []Port
	for _, l := range ls { // sorted by port, addr, pid
		if !pids[l.PID] {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Port == l.Port && out[n-1].Addr == l.Addr {
			continue
		}
		out = append(out, Port{Port: l.Port, Addr: l.Addr, PID: l.PID, Comm: l.Comm, Source: SourceProc})
	}
	return out
}

func sortPorts(ps []Port) {
	slices.SortStableFunc(ps, func(a, b Port) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		return strings.Compare(a.Addr, b.Addr)
	})
}

func uniquePorts(ps []Port) []int {
	var out []int
	for _, p := range ps {
		out = append(out, p.Port)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// convertContainers turns docker's containers into the model's, keeping
// published TCP ports only.
func convertContainers(cs []docker.Container) []Container {
	out := make([]Container, 0, len(cs))
	for _, c := range cs {
		ct := Container{Name: c.Name, Project: c.Project, Service: c.Service, State: c.State, Health: c.Health, Status: c.Status}
		seen := map[string]bool{}
		for _, p := range docker.TCP(c.Ports) {
			key := p.Addr + "|" + strconv.Itoa(p.Port)
			if seen[key] {
				continue
			}
			seen[key] = true
			ct.Ports = append(ct.Ports, Port{Port: p.Port, Addr: p.Addr, Source: SourceDocker, Container: c.Name})
		}
		sortPorts(ct.Ports)
		out = append(out, ct)
	}
	return out
}

func dockerStatus(in Inputs, cs []Container, owned map[string]bool) DockerStatus {
	ds := DockerStatus{Available: true}
	if in.DockerErr != nil {
		ds.Error = in.DockerErr.Error()
		if errors.Is(in.DockerErr, docker.ErrUnavailable) {
			ds.Available = false
		}
	}
	var groups []ContainerGroup
	for _, c := range cs {
		if owned[c.Project] {
			continue
		}
		if n := len(groups); n == 0 || groups[n-1].Project != c.Project {
			groups = append(groups, ContainerGroup{Project: c.Project})
		}
		groups[len(groups)-1].Containers = append(groups[len(groups)-1].Containers, c)
	}
	ds.Unowned = groups
	return ds
}

// conflictWarnings explains ports a process can't get back: for each process
// that isn't running, each last-seen port someone now holds.
func conflictWarnings(st *Status, snap *ports.Snapshot, ls []ports.Listener) []string {
	if snap == nil {
		return nil
	}
	owner := ownerIndex(st, snap)
	var out []string
	for i := range st.Processes {
		p := &st.Processes[i]
		if p.Running() {
			continue
		}
		own := map[int]bool{}
		for _, pt := range p.Ports {
			if pt.Source == SourceProc {
				// Left over from its own tree.
				if !own[pt.Port] {
					out = append(out, fmt.Sprintf("%s: :%d is still held by pid %d (%s), left over from its last run; rov restart %s stops it", p.Name, pt.Port, pt.PID, pt.Comm, p.Name))
				}
			}
			own[pt.Port] = true
		}
		for _, port := range p.LastPorts {
			if own[port] {
				continue
			}
			if pid, comm := orphanHolder(p, port, ls, owner, snap); pid > 0 {
				// A setsid'd child of its last run whose parent exited.
				out = append(out, fmt.Sprintf("%s: :%d is still held by pid %d (%s), started after %s did: likely left over from its last run; rov restart %s stops it", p.Name, port, pid, comm, p.Name, p.Name))
				continue
			}
			if msg := holderText(port, ls, owner, st); msg != "" {
				out = append(out, fmt.Sprintf("%s: :%d is held by %s", p.Name, port, msg))
			}
		}
	}
	return out
}

// orphanHolder returns the holder of port that stopTree would stop as an
// orphan of p: not in any session's tree, started at or after p did.
func orphanHolder(p *Process, port int, ls []ports.Listener, owner map[int]string, snap *ports.Snapshot) (int, string) {
	if !p.HasSession || p.StartedAt.IsZero() {
		return 0, ""
	}
	for _, l := range ls {
		if l.Port != port || l.PID <= 1 {
			continue
		}
		if _, owned := owner[l.PID]; owned {
			continue
		}
		if t, ok := snap.StartedAt(l.PID); ok && !t.Before(p.StartedAt) && snap.Alive(l.PID) {
			return l.PID, l.Comm
		}
	}
	return 0, ""
}

// holderText describes who listens on port, "" if nobody does.
func holderText(port int, ls []ports.Listener, owner map[int]string, st *Status) string {
	for _, l := range ls {
		if l.Port != port {
			continue
		}
		if l.PID == 0 {
			if c := containerWithPort(st, port); c != "" {
				return "docker container " + c
			}
			return "a process rov can't see (another user's; try: sudo ss -ltnp 'sport = :" + strconv.Itoa(port) + "')"
		}
		if name, ok := owner[l.PID]; ok {
			return fmt.Sprintf("pid %d (%s, rov process %s)", l.PID, l.Comm, name)
		}
		return fmt.Sprintf("pid %d (%s, not managed by rov)", l.PID, l.Comm)
	}
	if c := containerWithPort(st, port); c != "" {
		return "docker container " + c
	}
	return ""
}

func containerWithPort(st *Status, port int) string {
	check := func(cs []Container) string {
		for _, c := range cs {
			for _, p := range c.Ports {
				if p.Port == port {
					return c.Name
				}
			}
		}
		return ""
	}
	for _, p := range st.Processes {
		if n := check(p.Containers); n != "" {
			return n
		}
	}
	for _, g := range st.Docker.Unowned {
		if n := check(g.Containers); n != "" {
			return n
		}
	}
	return ""
}

// ownerIndex maps every pid in a session's tree to the process name.
func ownerIndex(st *Status, snap *ports.Snapshot) map[int]string {
	owner := map[int]string{}
	for _, p := range st.Processes {
		if !p.HasSession {
			continue
		}
		for _, pt := range p.Ports {
			if pt.PID > 0 {
				owner[pt.PID] = p.Name
			}
		}
		if p.PID > 0 {
			for _, pid := range snap.Tree(p.PID) {
				owner[pid] = p.Name
			}
		}
	}
	return owner
}
