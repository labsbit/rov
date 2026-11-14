package core

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/ports"
)

// HostPort is one listening TCP socket on the host, with its owner: a row of
// `rov ports`.
type HostPort struct {
	Port int
	Addr string
	// PID and Comm name the holder; 0 and "" when it isn't visible (another
	// user's process) or the port is a container's.
	PID  int
	Comm string
	// Owner is the rov process whose tree holds the socket, or
	// "<process>/<service>" for a container port ("" if none). For a
	// container of no process it is "<compose project>/<service>".
	Owner string
	// Source is "proc" or "docker".
	Source string
	// Container is the container publishing a docker port.
	Container string
	// Visible is false when /proc shows the socket but not its holder.
	Visible bool
}

// MarshalJSON writes nulls for an unknown pid, comm and owner.
func (h HostPort) MarshalJSON() ([]byte, error) {
	j := struct {
		Port      int     `json:"port"`
		Addr      string  `json:"addr"`
		PID       *int    `json:"pid"`
		Comm      *string `json:"comm"`
		Owner     *string `json:"owner"`
		Source    string  `json:"source"`
		Container string  `json:"container,omitempty"`
		Visible   bool    `json:"visible"`
	}{Port: h.Port, Addr: h.Addr, Source: h.Source, Container: h.Container, Visible: h.Visible}
	if h.PID > 0 {
		j.PID = &h.PID
	}
	if h.Comm != "" {
		j.Comm = &h.Comm
	}
	if h.Owner != "" {
		j.Owner = &h.Owner
	}
	return marshal(j)
}

// HostPorts lists every listening TCP socket on the host with its owner.
// Container ports of other projects are labelled with their container too
// (one host-wide `docker ps`).
func (e *Engine) HostPorts(ctx context.Context) ([]HostPort, *Status, error) {
	var (
		wg        sync.WaitGroup
		all       []docker.Container
		dockerErr error
	)
	wg.Add(1)
	go func() { defer wg.Done(); all, dockerErr = e.Docker.List(ctx, "/") }()
	snap, snapErr := e.ScanPorts()
	sessions, err := e.Sessions(ctx)
	wg.Wait()
	if err != nil {
		return nil, nil, err
	}
	if snapErr != nil {
		return nil, nil, snapErr
	}
	var mine, others []docker.Container
	for _, c := range all {
		if docker.Within(c.WorkingDir, e.root) {
			mine = append(mine, c)
		} else {
			others = append(others, c)
		}
	}
	st := Compose(e.Inputs(sessions, snap, mine, dockerErr))
	_ = e.RecordPorts(ctx, st)
	return BuildHostPorts(st, snap, convertContainers(others)), st, nil
}

// BuildHostPorts joins a port scan with a composed status: /proc rows get
// the rov process whose tree holds them, and container ports come from
// docker — /proc only shows root's docker-proxy for them (no visible pid),
// or nothing at all with some network setups. Rows are sorted by port, then
// address. others are containers outside the project: their ports are
// listed without an owner.
func BuildHostPorts(st *Status, snap *ports.Snapshot, others []Container) []HostPort {
	type key struct {
		port int
		addr string
	}
	var rows []HostPort
	dockerPorts := map[int]bool{}
	seen := map[key]bool{}
	addContainers := func(owner string, cs []Container) {
		for _, c := range cs {
			for _, p := range c.Ports {
				k := key{p.Port, p.Addr}
				if seen[k] {
					continue
				}
				seen[k] = true
				dockerPorts[p.Port] = true
				h := HostPort{Port: p.Port, Addr: p.Addr, Comm: "docker", Source: SourceDocker, Container: c.Name, Visible: true}
				if owner != "" {
					h.Owner = owner + "/" + c.Service
				}
				rows = append(rows, h)
			}
		}
	}
	for _, p := range st.Processes {
		addContainers(p.Name, p.Containers)
	}
	for _, g := range st.Docker.Unowned {
		addContainers(g.Project, g.Containers)
	}
	addContainers("", others)

	var owner map[int]string
	if snap != nil {
		owner = ownerIndex(st, snap)
		// Leftovers of dead panes belong to their process too.
		for _, p := range st.Processes {
			for _, pt := range p.Ports {
				if pt.PID > 0 {
					owner[pt.PID] = p.Name
				}
			}
		}
	}
	for _, l := range snap.Listeners() {
		if dockerPorts[l.Port] && (l.PID == 0 || isDockerProxy(l.Comm)) {
			continue // shown from docker
		}
		k := key{l.Port, l.Addr}
		h := HostPort{Port: l.Port, Addr: l.Addr, PID: l.PID, Comm: l.Comm, Owner: owner[l.PID], Source: SourceProc, Visible: l.PID > 0}
		// One row per socket and holder process; a forked server holds the
		// same socket in several processes: keep the first (lowest pid).
		if seen[k] && h.PID > 0 && rowHas(rows, l.Port, l.Addr, owner[l.PID], l.Comm) {
			continue
		}
		seen[k] = true
		rows = append(rows, h)
	}
	slices.SortStableFunc(rows, func(a, b HostPort) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		return strings.Compare(a.Addr, b.Addr)
	})
	return rows
}

func rowHas(rows []HostPort, port int, addr, owner, comm string) bool {
	for _, r := range rows {
		if r.Port == port && r.Addr == addr && r.Owner == owner && r.Comm == comm {
			return true
		}
	}
	return false
}

func isDockerProxy(comm string) bool {
	switch comm {
	case "docker-proxy", "rootlesskit", "slirp4netns", "dockerd", "pasta", "vpnkit":
		return true
	}
	return false
}
