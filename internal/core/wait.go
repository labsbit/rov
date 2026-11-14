package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/logs"
	"github.com/labsbit/rov/internal/ports"
)

// DefaultWaitTimeout is how long rov wait waits by default.
const DefaultWaitTimeout = 60 * time.Second

// Poll intervals of Wait.
const (
	waitPoll       = 200 * time.Millisecond
	waitDockerPoll = time.Second
)

// WaitCondition is what Wait waits for. At most one field may be set; the
// zero value is the default condition: the process listens on at least one
// port (exiting 0 first also counts), or, for a process with `docker:`,
// all its containers are up; for a container, it is up.
//
// For a process with `docker:` that is still running, its containers count
// (default condition and Healthy) only once each started at or after the
// process did (docker inspect's State.StartedAt): right after `rov restart
// infra` the old containers are still up while compose recreates them.
// Once the process has exited 0 its containers count as they are. When the
// start times can't be read, a running process has to exit 0 first.
type WaitCondition struct {
	Port    int            // the tree or an owned container listens on Port
	Log     *regexp.Regexp // a line of the current run matches
	Exit    bool           // the process exited with status 0
	Healthy bool           // docker reports the container(s) healthy
}

func (c WaitCondition) count() int {
	n := 0
	for _, b := range []bool{c.Port > 0, c.Log != nil, c.Exit, c.Healthy} {
		if b {
			n++
		}
	}
	return n
}

// String describes the condition: "a listening port", "port 8080", ….
func (c WaitCondition) String() string {
	switch {
	case c.Port > 0:
		return "port " + strconv.Itoa(c.Port)
	case c.Log != nil:
		return "a log line matching /" + c.Log.String() + "/"
	case c.Exit:
		return "exit 0"
	case c.Healthy:
		return "healthy"
	}
	return "the default condition"
}

// WaitOutcome is how a Wait ended.
type WaitOutcome int

const (
	WaitMet      WaitOutcome = iota // the condition became true (exit 0)
	WaitTimedOut                    // the timeout passed first (exit 1)
	WaitEnded                       // the process or container ended first (exit 3)
)

// WaitResult is the outcome of Wait.
type WaitResult struct {
	Outcome WaitOutcome
	// Message says what happened: "listening on :8080", "crashed(1) before
	// it listened on a port", "timed out after 1m0s waiting for …".
	Message string
	// Process is the last state seen of a process target; Container of a
	// container target.
	Process   *Process
	Container *Container
}

// Wait blocks until cond is true for target (a process, or a container as
// `<process>/<service>`), the target ends, timeout passes or ctx is done
// (then it returns ctx's error). It polls tmux and /proc every 200 ms and
// docker every second; it takes no lock, so it never delays an action.
// Errors are for bad targets or conditions, and for failures to poll.
func (e *Engine) Wait(ctx context.Context, target string, cond WaitCondition, timeout time.Duration) (WaitResult, error) {
	_, _ = e.Reload()
	if cond.count() > 1 {
		return WaitResult{}, errors.New("give one condition: --port, --log, --exit or --healthy")
	}
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}
	ref, isContainer := ParseTarget(target)
	w := waiter{ctx: ctx, e: e, target: target, ref: ref, isContainer: isContainer, cond: cond, timeout: timeout}
	if isContainer {
		if cond.Log != nil || cond.Exit {
			return WaitResult{}, errors.New("--log and --exit wait for processes; a container waits for up (default), --healthy or --port")
		}
	} else if cp, ok := e.Config().Get(target); ok && cp.Docker != "" {
		w.docker = true
	}
	if isContainer || cond.Healthy {
		w.docker = true
	}

	if cond.Log != nil && !isContainer {
		hits := make(chan string, 1)
		fctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = logs.Follow(fctx, logs.Query{Dir: e.LogDir(), Name: target, N: 1, Grep: cond.Log}, hits)
		}()
		defer func() { cancel(); <-done }()
		w.logHits = hits
	}

	deadline := time.Now().Add(timeout)
	for first := true; ; first = false {
		st, err := w.poll(ctx)
		if err != nil {
			return WaitResult{}, err
		}
		res, done, err := w.evaluate(st, first)
		if err != nil || done {
			return res, err
		}
		if !time.Now().Before(deadline) {
			res.Outcome = WaitTimedOut
			res.Message = fmt.Sprintf("timed out after %s waiting for %s (%s)", timeout, w.wanted(), w.stateNote(res))
			return res, nil
		}
		t := time.NewTimer(min(waitPoll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			return res, ctx.Err()
		case <-t.C:
		}
	}
}

type waiter struct {
	ctx         context.Context
	e           *Engine
	target      string
	ref         ContainerRef
	isContainer bool
	cond        WaitCondition
	timeout     time.Duration
	docker      bool

	logHits    <-chan string
	logLine    string
	containers []docker.Container
	dockerErr  error
	dockerAt   time.Time

	// fresh caches freshContainers for one docker poll (dockerAt).
	freshAt   time.Time
	fresh     bool
	freshNote string // why not, when !fresh
}

func (w *waiter) poll(ctx context.Context) (*Status, error) {
	sessions, err := w.e.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	var snap *ports.Snapshot
	if w.cond.Log == nil && !w.cond.Exit {
		snap, _ = w.e.ScanPorts()
	}
	if w.docker && time.Since(w.dockerAt) >= waitDockerPoll {
		w.containers, w.dockerErr = w.e.Containers(ctx)
		w.dockerAt = time.Now()
	}
	st := Compose(w.e.Inputs(sessions, snap, w.containers, w.dockerErr))
	if snap != nil {
		_ = w.e.RecordPorts(ctx, st)
	}
	return st, nil
}

// wanted describes the condition for this target.
func (w *waiter) wanted() string {
	switch {
	case w.cond.count() > 0:
		return w.cond.String()
	case w.isContainer:
		return "the container to be up"
	case w.docker:
		return "its containers to be up"
	}
	return "a listening port"
}

func (w *waiter) stateNote(r WaitResult) string {
	switch {
	case r.Container != nil:
		return "container " + r.Container.State + healthNote(r.Container)
	case r.Process != nil:
		note := r.Process.StateText()
		if r.Process.DockerState != "" {
			note += ", docker " + r.Process.DockerState
		}
		if !w.fresh && w.freshNote != "" && r.Process.Running() {
			note += ", " + w.freshNote
		}
		if ps := r.Process.PortNumbers(); len(ps) > 0 {
			note += ", ports " + joinInts(ps)
		} else if r.Process.Running() && w.cond.Log == nil && !w.cond.Exit {
			note += ", no ports yet"
		}
		return note
	}
	return "not found"
}

func healthNote(c *Container) string {
	if c.Health == "" {
		return ""
	}
	return ", " + c.Health
}

func (w *waiter) evaluate(st *Status, first bool) (WaitResult, bool, error) {
	if w.isContainer {
		return w.evaluateContainer(st, first)
	}
	p := st.Get(w.target)
	if p == nil {
		if first {
			return WaitResult{}, false, unknownError(w.target)
		}
		return WaitResult{Outcome: WaitEnded, Message: "was removed while waiting"}, true, nil
	}
	res := WaitResult{Process: p}
	if first && w.cond.Healthy && p.Docker == "" {
		return res, false, fmt.Errorf("%s owns no containers: --healthy needs a container (<process>/<service>) or a process with docker:", w.target)
	}
	if first && w.docker && w.dockerErr != nil && errors.Is(w.dockerErr, docker.ErrUnavailable) {
		return res, false, fmt.Errorf("can't wait for %s's containers: %w", w.target, w.dockerErr)
	}
	met, msg := w.met(p)
	if met {
		res.Outcome, res.Message = WaitMet, msg
		return res, true, nil
	}
	if p.Running() {
		return res, false, nil
	}
	// A one-shot that exited 0 may own containers still coming up.
	if p.State == StateExited && w.docker && !w.cond.Exit {
		return res, false, nil
	}
	if w.cond.Log != nil {
		// The line may have landed just before the exit.
		if lines, _, err := logs.Tail(logs.Query{Dir: w.e.LogDir(), Name: w.target, N: 1, Grep: w.cond.Log}); err == nil && len(lines) > 0 {
			res.Outcome, res.Message = WaitMet, "log matched: "+lines[0]
			return res, true, nil
		}
	}
	res.Outcome = WaitEnded
	res.Message = fmt.Sprintf("%s before %s", endText(p), w.wanted())
	if p.State == StateAbsent || p.State == StateStopped {
		res.Message = fmt.Sprintf("is %s (start it first: rov up %s)", p.StateText(), w.target)
	}
	return res, true, nil
}

func endText(p *Process) string {
	switch p.State {
	case StateExited:
		return "exited 0"
	case StateDead:
		if p.Signal > 0 {
			return fmt.Sprintf("was killed by signal %d", p.Signal)
		}
		return "died"
	}
	return p.StateText()
}

// met reports whether the condition holds for process p.
func (w *waiter) met(p *Process) (bool, string) {
	switch {
	case w.cond.Port > 0:
		if slices.Contains(p.PortNumbers(), w.cond.Port) {
			return true, "listening on :" + strconv.Itoa(w.cond.Port)
		}
	case w.cond.Log != nil:
		select {
		case l := <-w.logHits:
			w.logLine = l
		default:
		}
		if w.logLine != "" {
			return true, "log matched: " + w.logLine
		}
	case w.cond.Exit:
		if p.State == StateExited {
			return true, "exited 0"
		}
	case w.cond.Healthy:
		if allHealthy(p.Containers) && w.freshContainers(p) {
			return true, fmt.Sprintf("containers healthy (%d/%d)", len(p.Containers), len(p.Containers))
		}
	case w.docker:
		if p.DockerState == "up" && w.freshContainers(p) {
			msg := fmt.Sprintf("containers up (%d/%d)", len(p.Containers), len(p.Containers))
			if ps := p.PortNumbers(); len(ps) > 0 {
				msg += ", ports " + joinInts(ps)
			}
			return true, msg
		}
	default:
		if ps := p.PortNumbers(); len(ps) > 0 {
			return true, "listening on " + portList(ps)
		}
		if p.State == StateExited {
			return true, "exited 0"
		}
	}
	return false, ""
}

// freshContainers reports whether p's containers belong to its current run:
// p is no longer running, or every container started at or after p did. The
// answer is cached per docker poll; a "not fresh" reason is kept for the
// timeout message.
func (w *waiter) freshContainers(p *Process) bool {
	if !p.Running() || p.StartedAt.IsZero() {
		return true
	}
	if w.freshAt.IsZero() || !w.freshAt.Equal(w.dockerAt) {
		w.freshAt = w.dockerAt
		w.fresh, w.freshNote = w.checkFresh(p)
	}
	return w.fresh
}

func (w *waiter) checkFresh(p *Process) (bool, string) {
	names := make([]string, len(p.Containers))
	for i, c := range p.Containers {
		names[i] = c.Name
	}
	started, err := w.e.Docker.StartedAt(w.ctx, names)
	if err != nil {
		return false, fmt.Sprintf("waiting for %s to exit (can't read when its containers started: %v)", p.Name, err)
	}
	for _, n := range names {
		t, ok := started[n]
		if !ok || t.Before(p.StartedAt) {
			return false, fmt.Sprintf("container %s started before %s did (waiting for it to be recreated, or for %s to exit 0)", n, p.Name, p.Name)
		}
	}
	return true, ""
}

func allHealthy(cs []Container) bool {
	if len(cs) == 0 {
		return false
	}
	for _, c := range cs {
		if c.State != "running" || c.Health != "" && c.Health != "healthy" {
			return false
		}
	}
	return true
}

func (w *waiter) evaluateContainer(st *Status, first bool) (WaitResult, bool, error) {
	if !st.Docker.Available {
		return WaitResult{}, false, fmt.Errorf("can't wait for %s: docker is unavailable (%s)", w.target, st.Docker.Error)
	}
	c, _, err := FindContainer(st, w.ref)
	if err != nil {
		if first {
			return WaitResult{}, false, err
		}
		return WaitResult{Outcome: WaitEnded, Message: "disappeared while waiting"}, true, nil
	}
	res := WaitResult{Container: c}
	if first && w.cond.Healthy && c.Health == "" {
		return res, false, fmt.Errorf("%s (%s) has no health check; wait for it to be up instead: rov wait %s", w.target, c.Name, w.target)
	}
	switch {
	case w.cond.Port > 0:
		for _, p := range c.Ports {
			if p.Port == w.cond.Port {
				return withOutcome(res, WaitMet, "publishing :"+strconv.Itoa(w.cond.Port)), true, nil
			}
		}
	case w.cond.Healthy:
		if c.Health == "healthy" {
			return withOutcome(res, WaitMet, "healthy"), true, nil
		}
	default:
		if c.Up() {
			return withOutcome(res, WaitMet, "up ("+c.Status+")"), true, nil
		}
	}
	if c.State == "exited" || c.State == "dead" {
		return withOutcome(res, WaitEnded, fmt.Sprintf("container %s is %s (%s)", c.Name, c.State, c.Status)), true, nil
	}
	return res, false, nil
}

func withOutcome(r WaitResult, o WaitOutcome, msg string) WaitResult {
	r.Outcome, r.Message = o, msg
	return r
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

func portList(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = ":" + strconv.Itoa(n)
	}
	return strings.Join(s, " ")
}
