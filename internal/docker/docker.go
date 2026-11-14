// Package docker discovers the compose containers of a project and acts on
// them through the docker CLI (no SDK).
//
// Discovery uses the compose labels docker attaches to every container:
// a container belongs to a project root when its
// com.docker.compose.project.working_dir label is the root or below it.
//
// Every failure that means "there is no usable docker here" — no binary,
// daemon down or unreachable, permission denied on the socket, a hung daemon
// — is reported as an error wrapping [ErrUnavailable], so callers can turn
// the feature off silently with errors.Is.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable is wrapped by every error that means docker can't be used
// at all on this host right now.
var ErrUnavailable = errors.New("docker not available")

// Compose labels.
const (
	LabelProject    = "com.docker.compose.project"
	LabelService    = "com.docker.compose.service"
	LabelWorkingDir = "com.docker.compose.project.working_dir"
)

// Default timeouts.
const (
	DefaultTimeout        = 2 * time.Second
	DefaultComposeTimeout = 60 * time.Second
)

// How long Available trusts its last answer.
const (
	negativeTTL = 10 * time.Second
	positiveTTL = 30 * time.Second
)

// Port is one published port, host side.
type Port struct {
	Port  int    `json:"port"`
	Addr  string `json:"addr"`  // host bind address: "127.0.0.1", "0.0.0.0", "::", …
	Proto string `json:"proto"` // "tcp", "udp", "sctp"
}

// Container is one compose container.
type Container struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Image      string `json:"image"`
	Project    string `json:"project"`
	Service    string `json:"service"`
	WorkingDir string `json:"working_dir"`
	// State is docker's state: created, running, paused, restarting,
	// removing, exited or dead.
	State string `json:"state"`
	// Health is healthy, unhealthy, starting, or "" when the container has
	// no health check.
	Health string `json:"health"`
	// Status is docker's human text, e.g. "Up 2 hours (healthy)".
	Status string `json:"status"`
	Ports  []Port `json:"ports"`
}

// Client runs the docker CLI. The zero value is usable: Bin defaults to
// "docker", Timeout to [DefaultTimeout] and ComposeTimeout to
// [DefaultComposeTimeout]. A Client is safe for concurrent use.
type Client struct {
	// Bin is the docker executable, looked up on PATH if it has no slash.
	Bin string
	// Timeout bounds quick queries: Available and List.
	Timeout time.Duration
	// ComposeTimeout bounds actions: ComposeStop, ComposeDown, Start, Stop.
	ComposeTimeout time.Duration

	mu      sync.Mutex
	known   bool // a cached answer exists
	avail   bool
	availAt time.Time
	why     error // cause of the last negative answer
}

// Default returns a Client with the default binary and timeouts.
func Default() *Client {
	return &Client{Bin: "docker", Timeout: DefaultTimeout, ComposeTimeout: DefaultComposeTimeout}
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "docker"
	}
	return c.Bin
}

func (c *Client) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

func (c *Client) composeTimeout() time.Duration {
	if c.ComposeTimeout <= 0 {
		return DefaultComposeTimeout
	}
	return c.ComposeTimeout
}

// Available reports whether the docker binary exists and its daemon answers.
// A negative answer is cached for 10 s and a positive one for 30 s; any
// command that finds docker unavailable (or available) refreshes the cache.
func (c *Client) Available(ctx context.Context) bool {
	if ok, cached := c.cached(); cached {
		return ok
	}
	_, err := c.run(ctx, c.timeout(), "version", "--format", "{{.Server.Version}}")
	if err != nil && !errors.Is(err, ErrUnavailable) && !callerGaveUp(err) {
		// The daemon answered something odd; it's not usable either.
		c.remember(false, err)
	}
	return err == nil
}

func (c *Client) cached() (ok, cached bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.known {
		return false, false
	}
	ttl := positiveTTL
	if !c.avail {
		ttl = negativeTTL
	}
	if time.Since(c.availAt) > ttl {
		return false, false
	}
	return c.avail, true
}

// cachedUnavailable returns the cached reason docker is unavailable, or nil.
func (c *Client) cachedUnavailable() error {
	if ok, cached := c.cached(); cached && !ok {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.why
	}
	return nil
}

func (c *Client) remember(ok bool, why error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.known, c.avail, c.availAt, c.why = true, ok, time.Now(), why
}

// psFormat makes docker ps print, per container, its full JSON plus the
// three compose labels as exact strings. The Labels field of the JSON is a
// comma-joined list, ambiguous when a value contains a comma (several
// config_files do), so the labels List needs are asked for one by one.
const psFormat = `{"c":{{json .}},` +
	`"project":{{json (.Label "` + LabelProject + `")}},` +
	`"service":{{json (.Label "` + LabelService + `")}},` +
	`"working_dir":{{json (.Label "` + LabelWorkingDir + `")}}}`

// List returns the compose containers, running or not, whose working_dir
// label is root or a directory below it, sorted by project, service, name.
// root is made absolute and cleaned; a label that matches neither root nor
// root with its symlinks resolved is resolved itself as a last resort.
func (c *Client) List(ctx context.Context, root string) ([]Container, error) {
	if err := c.cachedUnavailable(); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, c.timeout(), "ps", "--all", "--no-trunc",
		"--filter", "label="+LabelWorkingDir, "--format", psFormat)
	if err != nil {
		return nil, err
	}
	all, err := ParsePS(out)
	if err != nil {
		return nil, err
	}
	m := newRootMatcher(root)
	var cs []Container
	for _, ct := range all {
		if m.match(ct.WorkingDir) {
			cs = append(cs, ct)
		}
	}
	SortContainers(cs)
	return cs, nil
}

// SortContainers sorts cs by project, then service, then name.
func SortContainers(cs []Container) {
	slices.SortStableFunc(cs, func(a, b Container) int {
		return cmpStrings(a.Project, b.Project, a.Service, b.Service, a.Name, b.Name)
	})
}

func cmpStrings(pairs ...string) int {
	for i := 0; i+1 < len(pairs); i += 2 {
		if c := strings.Compare(pairs[i], pairs[i+1]); c != 0 {
			return c
		}
	}
	return 0
}

// rootMatcher tests working_dir labels against a project root.
type rootMatcher struct{ roots []string }

func newRootMatcher(root string) rootMatcher {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	root = filepath.Clean(root)
	m := rootMatcher{roots: []string{root}}
	if r, err := filepath.EvalSymlinks(root); err == nil && r != root {
		m.roots = append(m.roots, r)
	}
	return m
}

func (m rootMatcher) match(dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	dir = filepath.Clean(dir)
	if m.within(dir) {
		return true
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil && r != dir {
		return m.within(r)
	}
	return false
}

func (m rootMatcher) within(dir string) bool {
	for _, root := range m.roots {
		if Within(dir, root) {
			return true
		}
	}
	return false
}

// Within reports whether the clean absolute path dir is root or below it,
// comparing whole path elements ("/a/bc" is not within "/a/b").
func Within(dir, root string) bool {
	if dir == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(dir, prefix)
}

// psLine is one line of List's docker ps output. A line printed with a plain
// {{json .}} (no wrapper) decodes with C empty and is handled too.
type psLine struct {
	C          *psJSON `json:"c"`
	Project    string  `json:"project"`
	Service    string  `json:"service"`
	WorkingDir string  `json:"working_dir"`
}

// psJSON is the subset of docker ps's {{json .}} that rov uses.
type psJSON struct {
	ID           string `json:"ID"`
	Names        string `json:"Names"`
	Image        string `json:"Image"`
	Labels       string `json:"Labels"`
	State        string `json:"State"`
	Status       string `json:"Status"`
	HealthStatus string `json:"HealthStatus"`
	Health       string `json:"Health"`
	Ports        string `json:"Ports"`
}

// ParsePS parses docker ps output, one JSON object per line, in either List's
// wrapped format or plain --format '{{json .}}'. Blank lines are skipped; a
// line that isn't JSON is an error (docker never prints one on stdout).
func ParsePS(out []byte) ([]Container, error) {
	var cs []Container
	for n, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var w psLine
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, fmt.Errorf("docker ps: line %d: %w", n+1, err)
		}
		if w.C == nil { // plain {{json .}}
			var p psJSON
			if err := json.Unmarshal(line, &p); err != nil {
				return nil, fmt.Errorf("docker ps: line %d: %w", n+1, err)
			}
			labels := ParseLabels(p.Labels)
			w = psLine{C: &p, Project: labels[LabelProject], Service: labels[LabelService], WorkingDir: labels[LabelWorkingDir]}
		}
		cs = append(cs, w.container())
	}
	return cs, nil
}

func (w psLine) container() Container {
	p := w.C
	name, _, _ := strings.Cut(p.Names, ",") // linked containers list several
	health := p.HealthStatus
	if health == "" {
		health = p.Health
	}
	wd := w.WorkingDir
	if wd != "" {
		wd = filepath.Clean(wd)
	}
	return Container{
		ID:         p.ID,
		Name:       name,
		Image:      p.Image,
		Project:    w.Project,
		Service:    w.Service,
		WorkingDir: wd,
		State:      normState(p.State, p.Status),
		Health:     normHealth(health, p.Status),
		Status:     p.Status,
		Ports:      ParsePorts(p.Ports),
	}
}

// normState returns state lowercased, or, for docker versions whose ps JSON
// lacks State, derives it from the Status text.
func normState(state, status string) string {
	if state != "" {
		return strings.ToLower(state)
	}
	s := strings.ToLower(status)
	switch {
	case strings.HasPrefix(s, "up") && strings.Contains(s, "(paused)"):
		return "paused"
	case strings.HasPrefix(s, "up"):
		return "running"
	case strings.HasPrefix(s, "exited"):
		return "exited"
	case strings.HasPrefix(s, "created"):
		return "created"
	case strings.HasPrefix(s, "restarting"):
		return "restarting"
	case strings.HasPrefix(s, "removal"):
		return "removing"
	case strings.HasPrefix(s, "dead"):
		return "dead"
	}
	return ""
}

// normHealth maps docker's health value to healthy|unhealthy|starting|"",
// falling back to the "(healthy)" / "(health: starting)" suffix of Status
// for docker versions without a HealthStatus field.
func normHealth(health, status string) string {
	switch h := strings.ToLower(strings.TrimSpace(health)); h {
	case "none": // docker's word for "no health check"
		return ""
	case "":
		// Older docker: only the Status text tells.
	default:
		return h
	}
	s := strings.ToLower(status)
	switch {
	case strings.Contains(s, "(healthy)"):
		return "healthy"
	case strings.Contains(s, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(s, "(health: starting)"):
		return "starting"
	}
	return ""
}

// ParseLabels parses the Labels field of docker ps ("a=b,c=d").
//
// Docker joins labels with commas without escaping, so the format is
// ambiguous. A segment without '=' is taken as the continuation of the
// previous value (which recovers comma-separated paths like several
// config_files); a value containing ",x=y" is still split wrongly. List does
// not depend on this: it asks docker for the labels it needs one by one.
func ParseLabels(s string) map[string]string {
	m := map[string]string{}
	last := ""
	for _, seg := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(seg, "=")
		if !ok || k == "" {
			if last != "" {
				m[last] += "," + seg
			}
			continue
		}
		m[k] = v
		last = k
	}
	return m
}

// Projects returns the distinct non-empty compose projects of cs, sorted.
func Projects(cs []Container) []string {
	var ps []string
	for _, c := range cs {
		if c.Project != "" {
			ps = append(ps, c.Project)
		}
	}
	slices.Sort(ps)
	return slices.Compact(ps)
}

// Summarize reports how many of cs are running, and whether all are up:
// running and, where they have a health check, healthy. An empty cs is not
// up (the caller shows the owning process's own state instead).
func Summarize(cs []Container) (up bool, running, total int) {
	up = len(cs) > 0
	for _, c := range cs {
		if c.State == "running" {
			running++
		} else {
			up = false
		}
		if c.Health != "" && c.Health != "healthy" {
			up = false
		}
	}
	return up, running, len(cs)
}

// ComposeStop runs `docker compose -p <project> stop`: the containers stay,
// a later `up` restarts them.
func (c *Client) ComposeStop(ctx context.Context, project string) error {
	return c.compose(ctx, project, "stop")
}

// ComposeDown runs `docker compose -p <project> down`. Volumes are kept:
// this never passes -v.
func (c *Client) ComposeDown(ctx context.Context, project string) error {
	return c.compose(ctx, project, "down")
}

func (c *Client) compose(ctx context.Context, project, verb string) error {
	if project == "" {
		// Without -p compose would pick a project from the cwd.
		return errors.New("docker compose: empty project name")
	}
	_, err := c.runIn(ctx, c.composeTimeout(), neutralDir(), "compose", "-p", project, verb)
	return err
}

// neutralDir is where compose commands run. compose looks for a compose
// file in the cwd and its parents; acting by project name alone must not
// pick up an unrelated file from wherever rov was started.
func neutralDir() string { return string(filepath.Separator) }

// StartedAt returns when each of the containers ids (or names) last
// started, from one `docker inspect`. A container that never started, or
// that inspect doesn't report, is missing from the map.
func (c *Client) StartedAt(ctx context.Context, ids []string) (map[string]time.Time, error) {
	if len(ids) == 0 {
		return map[string]time.Time{}, nil
	}
	if err := c.cachedUnavailable(); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, c.timeout(), append([]string{"inspect", "--type", "container", "--format", "{{.Id}} {{.Name}} {{.State.StartedAt}}"}, ids...)...)
	if err != nil {
		return nil, err
	}
	return ParseStartedAt(out), nil
}

// ParseStartedAt parses StartedAt's inspect output, "<id> /<name> <time>" per
// line, into a map keyed by both id and name.
func ParseStartedAt(out []byte) map[string]time.Time {
	m := map[string]time.Time{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, f[2])
		if err != nil || t.Year() < 2000 {
			continue // never started: 0001-01-01T00:00:00Z
		}
		m[f[0]] = t
		m[strings.TrimPrefix(f[1], "/")] = t
	}
	return m
}

// Start runs `docker start <container>`.
func (c *Client) Start(ctx context.Context, container string) error {
	_, err := c.run(ctx, c.composeTimeout(), "start", container)
	return err
}

// Stop runs `docker stop <container>`.
func (c *Client) Stop(ctx context.Context, container string) error {
	_, err := c.run(ctx, c.composeTimeout(), "stop", container)
	return err
}

// run runs docker with args under a timeout and returns its stdout. See
// runIn.
func (c *Client) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	return c.runIn(ctx, timeout, "", args...)
}

// runIn runs docker with args in dir ("" = the current directory). Errors
// carry docker's stderr; those meaning docker is unusable wrap
// ErrUnavailable and update the availability cache.
func (c *Client) runIn(ctx context.Context, timeout time.Duration, dir string, args ...string) ([]byte, error) {
	bin, err := exec.LookPath(c.bin())
	if err != nil {
		err = fmt.Errorf("%w: %v", ErrUnavailable, err)
		c.remember(false, err)
		return nil, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = composeEnv(os.Environ())
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runErr == nil {
		c.remember(true, nil)
		return stdout.Bytes(), nil
	}
	what := "docker " + strings.Join(args, " ")
	msg := strings.TrimSpace(stderr.String())
	// Only our own timeout on a quick query, or docker saying so, means the
	// daemon is unusable. The caller's ctx running out (its deadline, a
	// cancel) says nothing about the daemon and must not be cached.
	switch {
	case ownTimeout(ctx) && isQuery(args):
		err = fmt.Errorf("%w: %s: no answer within %s", ErrUnavailable, what, timeout)
	case daemonDown(msg):
		err = fmt.Errorf("%w: %s", ErrUnavailable, msg)
	case ctx.Err() != nil:
		// Cancelled, or a slow action: says nothing about the daemon.
		if msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", what, ctx.Err(), msg)
		}
		return nil, fmt.Errorf("%s: %w", what, ctx.Err())
	default:
		if msg == "" {
			msg = runErr.Error()
		}
		return nil, fmt.Errorf("%s: %s", what, msg)
	}
	c.remember(false, err)
	return nil, err
}

// errTimeout is the cause of a context that ran out because of the timeout
// rov gives a docker command, as opposed to the caller's deadline.
var errTimeout = errors.New("docker command timed out")

// ownTimeout reports whether ctx, made with WithTimeoutCause(…, errTimeout),
// ended because that timeout fired first.
func ownTimeout(ctx context.Context) bool { return errors.Is(context.Cause(ctx), errTimeout) }

// callerGaveUp reports whether err comes from the caller's context ending.
func callerGaveUp(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isQuery reports whether args is one of the quick queries whose timeout
// means the daemon is unusable.
func isQuery(args []string) bool {
	return len(args) > 0 && (args[0] == "ps" || args[0] == "version")
}

// composeEnv drops variables that would make compose read a compose file or
// override the project name given with -p.
func composeEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == "COMPOSE_FILE" || k == "COMPOSE_PROJECT_NAME" || k == "COMPOSE_PATH_SEPARATOR"
	})
}

// daemonDown reports whether a docker stderr message means the daemon can't
// be reached at all.
func daemonDown(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"error during connect",
		"permission denied while trying to connect",
		"docker daemon is not running",
		"failed to connect to the docker api",
	} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
