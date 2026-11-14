package core

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ContainerRef names a container as `<process>/<compose service>`, e.g.
// "infra/redis". For containers no process owns, the compose project name
// stands in for the process.
type ContainerRef struct {
	Owner   string // the owning process, or the compose project of an unowned container
	Service string
}

func (r ContainerRef) String() string { return r.Owner + "/" + r.Service }

// ParseTarget splits "infra/redis" into a container reference; a plain
// process name returns ok=false.
func ParseTarget(target string) (ref ContainerRef, ok bool) {
	owner, rov, found := strings.Cut(target, "/")
	if !found {
		return ContainerRef{}, false
	}
	return ContainerRef{Owner: owner, Service: rov}, true
}

// FindContainer returns the container ref names in st, and whether its owner
// is a process (false: an unowned compose project).
func FindContainer(st *Status, ref ContainerRef) (c *Container, owned bool, err error) {
	if p := st.Get(ref.Owner); p != nil {
		if p.Docker == "" {
			return nil, false, fmt.Errorf("%s owns no containers (no docker: in rov.yaml)", ref.Owner)
		}
		var services []string
		for i := range p.Containers {
			if p.Containers[i].Service == ref.Service || p.Containers[i].Name == ref.Service {
				return &p.Containers[i], true, nil
			}
			services = append(services, p.Containers[i].Service)
		}
		if len(services) == 0 {
			return nil, true, fmt.Errorf("%s has no containers (compose project %s); start it with rov up %s", ref.Owner, p.Docker, ref.Owner)
		}
		return nil, true, fmt.Errorf("%s has no container for service %q (it has %s)", ref.Owner, ref.Service, strings.Join(services, ", "))
	}
	for gi := range st.Docker.Unowned {
		g := &st.Docker.Unowned[gi]
		if g.Project != ref.Owner {
			continue
		}
		for i := range g.Containers {
			if g.Containers[i].Service == ref.Service || g.Containers[i].Name == ref.Service {
				return &g.Containers[i], false, nil
			}
		}
	}
	return nil, false, fmt.Errorf("unknown container %s (address one as <process>/<service>, e.g. infra/redis; see rov status)", ref)
}

// ContainerStart runs `docker start` on a container of a process
// (`<process>/<service>`). Unowned containers are read-only.
func (e *Engine) ContainerStart(ctx context.Context, target string) (string, error) {
	return e.containerAction(ctx, target, "starting", func(name string) error { return e.Docker.Start(ctx, name) }, "started")
}

// ContainerStop runs `docker stop` on a container of a process.
func (e *Engine) ContainerStop(ctx context.Context, target string) (string, error) {
	return e.containerAction(ctx, target, "stopping", func(name string) error { return e.Docker.Stop(ctx, name) }, "stopped")
}

func (e *Engine) containerAction(ctx context.Context, target, verb string, act func(string) error, done string) (string, error) {
	ref, ok := ParseTarget(target)
	if !ok {
		return "", fmt.Errorf("%q is not a container (use <process>/<service>, e.g. infra/redis)", target)
	}
	return e.locked(ctx, ref.Owner, verb+" "+ref.Service+" of", func() (string, error) {
		st, err := e.Status(ctx)
		if err != nil {
			return "", err
		}
		c, owned, err := FindContainer(st, ref)
		if err != nil {
			return "", err
		}
		if !owned {
			return "", fmt.Errorf("%s belongs to no process: rov leaves unowned containers alone (claim them with docker: in rov.yaml)", target)
		}
		if err := act(c.Name); err != nil {
			return "", err
		}
		return done + " (container " + c.Name + ")", nil
	})
}

// AttachCommand returns the command that attaches the terminal to the
// session of name; the TUI runs it with tea.ExecProcess and gets control
// back on detach. The CLI uses Tmux.ExecAttach instead.
func (e *Engine) AttachCommand(name string) *exec.Cmd { return e.Tmux.AttachCommand(name) }
