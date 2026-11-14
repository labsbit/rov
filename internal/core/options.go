package core

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/tmux"
)

// Session user options: rov's whole state outside rov.yaml.
const (
	OptKind      = "@rov_kind"       // "config" or "adhoc"
	OptCmd       = "@rov_cmd"        // the command the session was started with
	OptCwd       = "@rov_cwd"        // its working dir, relative to the root unless outside
	OptEnv       = "@rov_env"        // its extra env: JSON array of "K=V"
	OptStopped   = "@rov_stopped"    // "1" after rov stop
	OptStarted   = "@rov_started"    // unix seconds of the last start
	OptLastPorts = "@rov_last_ports" // "8080,8081"
	OptRunID     = "@rov_run_id"     // unix nanoseconds of the last start: the run's identity
	// OptRunDef is the definition the running process was started with, as
	// JSON, while Redefine has stored a different one in OptCmd/OptCwd/OptEnv
	// ("" otherwise). A start clears it.
	OptRunDef = "@rov_run_def"
)

// sessionOptions are the options every session listing asks for.
var sessionOptions = []string{OptKind, OptCmd, OptCwd, OptEnv, OptStopped, OptStarted, OptLastPorts, OptRunID, OptRunDef}

// definition is how a process is started: from rov.yaml or the session.
type definition struct {
	Name string
	Kind Kind
	Cmd  string
	// Cwd as stored: relative to the root ("." is the root), or absolute.
	Cwd string
	Env []config.EnvVar
}

// dir returns the absolute working directory.
func (d definition) dir(root string) string {
	return absDir(root, d.Cwd)
}

// configDefinition returns the definition of a rov.yaml process.
func configDefinition(p config.Process) definition {
	cwd := p.Cwd
	if cwd == "" {
		cwd = "."
	}
	return definition{Name: p.Name, Kind: KindConfig, Cmd: p.Cmd, Cwd: cleanRel(cwd), Env: p.Env}
}

// storedDefinition returns the definition kept in a session's options.
func storedDefinition(s tmux.Session) definition {
	cwd := s.Option(OptCwd)
	if cwd == "" {
		cwd = "."
	}
	kind := Kind(s.Option(OptKind))
	if kind != KindConfig {
		kind = KindAdhoc
	}
	return definition{Name: s.Name, Kind: kind, Cmd: s.Option(OptCmd), Cwd: cwd, Env: decodeEnv(s.Option(OptEnv))}
}

// sameDefinition reports whether a and b start the same thing. Env order
// doesn't matter.
func sameDefinition(a, b definition) bool {
	if a.Cmd != b.Cmd || cleanRel(a.Cwd) != cleanRel(b.Cwd) || len(a.Env) != len(b.Env) {
		return false
	}
	m := make(map[string]string, len(a.Env))
	for _, e := range a.Env {
		m[e.Key] = e.Value
	}
	for _, e := range b.Env {
		if v, ok := m[e.Key]; !ok || v != e.Value {
			return false
		}
	}
	return true
}

// runDef is the JSON form of a definition in OptRunDef.
type runDef struct {
	Cmd string   `json:"cmd"`
	Cwd string   `json:"cwd"`
	Env []string `json:"env,omitempty"`
}

func encodeRunDef(d definition) string {
	rd := runDef{Cmd: d.Cmd, Cwd: d.Cwd}
	for _, e := range d.Env {
		rd.Env = append(rd.Env, e.String())
	}
	b, _ := json.Marshal(rd)
	return string(b)
}

// decodeRunDef reads OptRunDef; ok is false when it is unset or unreadable.
func decodeRunDef(name, s string) (definition, bool) {
	var rd runDef
	if s == "" || json.Unmarshal([]byte(s), &rd) != nil {
		return definition{}, false
	}
	d := definition{Name: name, Kind: KindAdhoc, Cmd: rd.Cmd, Cwd: rd.Cwd}
	for _, e := range rd.Env {
		k, v, _ := strings.Cut(e, "=")
		d.Env = append(d.Env, config.EnvVar{Key: k, Value: v})
	}
	return d, true
}

// runID is the identity of a session's current run: @rov_run_id, or for a
// session started by an older rov the pane, its pid and the start second.
func runID(s tmux.Session) string {
	if id := s.Option(OptRunID); id != "" {
		return id
	}
	return s.PaneID + "/" + strconv.Itoa(s.PanePID) + "/" + strconv.FormatInt(startedAt(s).Unix(), 10)
}

func encodeEnv(env []config.EnvVar) string {
	if len(env) == 0 {
		return ""
	}
	kv := make([]string, len(env))
	for i, e := range env {
		kv[i] = e.String()
	}
	b, _ := json.Marshal(kv)
	return string(b)
}

func decodeEnv(s string) []config.EnvVar {
	if s == "" {
		return nil
	}
	var kv []string
	if json.Unmarshal([]byte(s), &kv) != nil {
		return nil
	}
	out := make([]config.EnvVar, 0, len(kv))
	for _, e := range kv {
		k, v, _ := strings.Cut(e, "=")
		out = append(out, config.EnvVar{Key: k, Value: v})
	}
	return out
}

func envKeys(env []config.EnvVar) []string {
	keys := make([]string, len(env))
	for i, e := range env {
		keys[i] = e.Key
	}
	return keys
}

func encodePorts(ps []int) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

func decodePorts(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n > 0 && n < 65536 {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// startedAt reads @rov_started, falling back to the session's creation (a
// session rov didn't start itself).
func startedAt(s tmux.Session) time.Time {
	if secs, err := strconv.ParseInt(s.Option(OptStarted), 10, 64); err == nil && secs > 0 {
		return time.Unix(secs, 0)
	}
	return s.Created
}
