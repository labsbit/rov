package core

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
)

// Kind says where a process is defined.
type Kind string

const (
	// KindConfig is a process listed in rov.yaml.
	KindConfig Kind = "config"
	// KindAdhoc is a process started with spawn, or a session whose name
	// left rov.yaml. Its definition lives in the session's options.
	KindAdhoc Kind = "adhoc"
)

// State is the life-cycle state of a process.
type State string

const (
	StateRunning State = "running" // the pane's process is alive
	StateStopped State = "stopped" // rov stop: not a failure
	StateExited  State = "exited"  // finished on its own with status 0
	StateCrashed State = "crashed" // finished with a non-zero status (ExitCode)
	StateDead    State = "dead"    // finished, status unknown (killed by a signal)
	StateAbsent  State = "absent"  // no session: never started, or rov down
)

// Process is everything rov knows about one process. The JSON form is one
// entry of `rov status --json` "processes".
type Process struct {
	Name  string
	Kind  Kind
	State State
	// ExitCode is set for exited (0) and crashed processes.
	ExitCode *int
	// Signal is the signal that killed a dead process, when tmux knows it.
	Signal int
	// PID is the pane's pid while running, else 0.
	PID int
	// Ports are the listening TCP sockets of the process tree and of owned
	// containers, sorted by port then address. The same port on 0.0.0.0 and
	// :: gives two entries; see PortNumbers.
	Ports []Port
	// LastPorts are the port numbers last seen, kept in the session so a
	// conflict after a stop or crash can be explained.
	LastPorts []int
	// StartedAt is the last start, zero without a session.
	StartedAt time.Time
	// RunID identifies the current run: it changes with every start, also
	// twice within one second, and stays the same when the run ends. ""
	// without a session.
	RunID string
	// Cmd, Cwd and EnvKeys describe the definition: rov.yaml for config
	// processes, the stored one for ad-hoc ones. Cwd is relative to the
	// root ("." for the root itself) unless it lies outside it.
	Cmd     string
	Cwd     string
	EnvKeys []string
	// Description is the rov.yaml description ("" for ad-hoc processes).
	Description string
	// Changed reports that the definition — rov.yaml's cmd, cwd or env, or
	// for an ad-hoc process the one Redefine stored — differs from what the
	// session was started with; a restart applies it. RunningCmd is then the
	// command the session runs.
	Changed    bool
	RunningCmd string
	// Docker is the compose project the process owns ("" if none), and
	// Containers are that project's containers.
	Docker     string
	Containers []Container
	// DockerState summarises the containers: "up", "degraded 1/2", or ""
	// without docker or containers.
	DockerState string
	// Log is the plain log file, relative to the root when inside it.
	Log string
	// Problems lists what is wrong with the rov.yaml entry; a process with
	// problems can't be started.
	Problems []string

	// PaneID is the tmux pane ("%3"), "" without a session.
	PaneID string
	// HasSession reports whether a tmux session exists.
	HasSession bool

	storedLastPorts []int // @rov_last_ports as stored, for RecordPorts
}

// Running reports whether the process is alive.
func (p *Process) Running() bool { return p.State == StateRunning }

// StateText is the state as `rov status` prints it: "crashed(101)" for a
// crash, the plain state otherwise.
func (p *Process) StateText() string {
	if p.State == StateCrashed && p.ExitCode != nil {
		return "crashed(" + strconv.Itoa(*p.ExitCode) + ")"
	}
	return string(p.State)
}

// Uptime is how long a running process has run at now, 0 otherwise.
func (p *Process) Uptime(now time.Time) time.Duration {
	if !p.Running() || p.StartedAt.IsZero() || now.Before(p.StartedAt) {
		return 0
	}
	return now.Sub(p.StartedAt)
}

// PortNumbers returns the distinct port numbers of p.Ports, sorted.
func (p *Process) PortNumbers() []int { return uniquePorts(p.Ports) }

// Port is one listening TCP socket.
type Port struct {
	Port int    `json:"port"`
	Addr string `json:"addr"`
	// PID and Comm name the holder; empty for docker ports.
	PID  int    `json:"pid,omitempty"`
	Comm string `json:"comm,omitempty"`
	// Source is "proc" (found in the process tree) or "docker" (published
	// by an owned container).
	Source string `json:"source"`
	// Container is the container publishing a docker port.
	Container string `json:"container,omitempty"`
}

// Port sources.
const (
	SourceProc   = "proc"
	SourceDocker = "docker"
)

// Container is one compose container.
type Container struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Service string `json:"service"`
	// State is docker's: running, exited, restarting, paused, created, dead.
	State string `json:"state"`
	// Health is healthy, unhealthy, starting, or "" without a health check.
	Health string `json:"health"`
	// Status is docker's text, e.g. "Up 2 hours (healthy)".
	Status string `json:"status"`
	// Ports are the published TCP ports, host side.
	Ports []Port `json:"ports"`
}

// Up reports whether the container is running and, if it has a health
// check, healthy.
func (c *Container) Up() bool {
	return c.State == "running" && (c.Health == "" || c.Health == "healthy")
}

// ContainerGroup is the containers of one compose project.
type ContainerGroup struct {
	Project    string      `json:"project"`
	Containers []Container `json:"containers"`
}

// DockerStatus is the docker part of a Status.
type DockerStatus struct {
	// Available is false when there is no usable docker on this host.
	Available bool `json:"available"`
	// Error says why docker is unavailable or failed, "" otherwise.
	Error string `json:"error,omitempty"`
	// Unowned are the project's compose containers no process claims with
	// `docker:`, grouped by compose project.
	Unowned []ContainerGroup `json:"unowned"`
}

// Status is the full model of a project: what `rov status` and the TUI show.
type Status struct {
	Project string `json:"project"`
	Root    string `json:"root"`
	// Socket is the tmux server's socket name (tmux -L).
	Socket string `json:"socket"`
	// Processes come in config order, then ad-hoc ones by start time.
	Processes []Process    `json:"processes"`
	Docker    DockerStatus `json:"docker"`
	// Warnings are one-line diagnoses: rov.yaml problems, port conflicts,
	// docker trouble.
	Warnings []string `json:"warnings"`
	// Time is when the status was composed.
	Time time.Time `json:"-"`
}

// Get returns the process called name, or nil.
func (s *Status) Get(name string) *Process {
	for i := range s.Processes {
		if s.Processes[i].Name == name {
			return &s.Processes[i]
		}
	}
	return nil
}

// Names returns the process names in display order.
func (s *Status) Names() []string {
	out := make([]string, len(s.Processes))
	for i, p := range s.Processes {
		out[i] = p.Name
	}
	return out
}

// processJSON is the JSON form of Process: the field names of the spec,
// nulls for "none".
type processJSON struct {
	Name        string      `json:"name"`
	Kind        Kind        `json:"kind"`
	State       State       `json:"state"`
	ExitCode    *int        `json:"exit_code"`
	Signal      int         `json:"signal,omitempty"`
	PID         *int        `json:"pid"`
	Ports       []Port      `json:"ports"`
	LastPorts   []int       `json:"last_ports"`
	StartedAt   *time.Time  `json:"started_at"`
	Cmd         string      `json:"cmd"`
	RunningCmd  string      `json:"running_cmd,omitempty"`
	Cwd         string      `json:"cwd"`
	EnvKeys     []string    `json:"env_keys"`
	Description string      `json:"description"`
	Changed     bool        `json:"changed"`
	Docker      string      `json:"docker,omitempty"`
	DockerState string      `json:"docker_state,omitempty"`
	Containers  []Container `json:"containers"`
	Log         string      `json:"log"`
	Problems    []string    `json:"problems,omitempty"`
}

// MarshalJSON writes the spec's process object.
func (p Process) MarshalJSON() ([]byte, error) {
	j := processJSON{
		Name: p.Name, Kind: p.Kind, State: p.State, ExitCode: p.ExitCode, Signal: p.Signal,
		Ports: nonNil(p.Ports), LastPorts: nonNil(p.LastPorts),
		Cmd: p.Cmd, RunningCmd: p.RunningCmd, Cwd: p.Cwd, EnvKeys: nonNil(p.EnvKeys),
		Description: p.Description, Changed: p.Changed, Docker: p.Docker, DockerState: p.DockerState,
		Containers: nonNil(p.Containers), Log: p.Log, Problems: p.Problems,
	}
	if p.PID > 0 {
		pid := p.PID
		j.PID = &pid
	}
	if !p.StartedAt.IsZero() {
		t := p.StartedAt.UTC()
		j.StartedAt = &t
	}
	return marshal(j)
}

// MarshalJSON writes empty lists as [] rather than null.
func (c Container) MarshalJSON() ([]byte, error) {
	type plain Container
	c.Ports = nonNil(c.Ports)
	return marshal(plain(c))
}

// MarshalJSON writes empty lists as [] rather than null.
func (s Status) MarshalJSON() ([]byte, error) {
	type plain Status
	s.Processes = nonNil(s.Processes)
	s.Warnings = nonNil(s.Warnings)
	s.Docker.Unowned = nonNil(s.Docker.Unowned)
	return marshal(plain(s))
}

// marshal is json.Marshal without HTML escaping: commands are full of "&&"
// and "<", and agents read this JSON raw.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Marshal is the JSON encoding rov prints (no HTML escaping).
func Marshal(v any) ([]byte, error) { return marshal(v) }

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
