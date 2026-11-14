package core_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/testenv"
)

func TestCompose(t *testing.T) {
	st := core.Compose(testenv.SampleInputs(t))

	if got, want := st.Names(), []string{"infra", "gateway", "tracking", "worker-rust", "api", "panel", "old-mock", "mock-stripe"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	get := func(name string) *core.Process {
		p := st.Get(name)
		if p == nil {
			t.Fatalf("no %s", name)
		}
		return p
	}

	infra := get("infra")
	if infra.State != core.StateExited || infra.DockerState != "up" || len(infra.Containers) != 2 {
		t.Errorf("infra: %s %q %d containers", infra.State, infra.DockerState, len(infra.Containers))
	}
	if got := infra.PortNumbers(); !slices.Equal(got, []int{6379, 8123}) {
		t.Errorf("infra ports = %v", got)
	}
	for _, p := range infra.Ports {
		if p.Source != core.SourceDocker || p.Container == "" {
			t.Errorf("infra port %+v", p)
		}
	}
	if redis := infra.Containers[1]; redis.Service != "redis" || len(redis.Ports) != 1 {
		t.Errorf("redis = %+v (udp must be dropped)", redis)
	}

	gw := get("gateway")
	if gw.State != core.StateRunning || gw.PID != 41233 || gw.Uptime(st.Time).String() != "2h13m0s" {
		t.Errorf("gateway: %s pid %d up %s", gw.State, gw.PID, gw.Uptime(st.Time))
	}
	if len(gw.Ports) != 2 || !slices.Equal(gw.PortNumbers(), []int{8080}) || gw.Ports[0].PID != 41301 || gw.Ports[0].Comm != "api-gateway" {
		t.Errorf("gateway ports = %+v", gw.Ports)
	}
	if gw.Changed || gw.Description != "search gateway" {
		t.Errorf("gateway changed %v desc %q", gw.Changed, gw.Description)
	}

	wr := get("worker-rust")
	if wr.StateText() != "crashed(101)" || len(wr.Ports) != 0 || !slices.Equal(wr.LastPorts, []int{9101}) {
		t.Errorf("worker-rust: %s ports %v last %v", wr.StateText(), wr.Ports, wr.LastPorts)
	}

	api := get("api")
	if !api.Changed || api.RunningCmd != "just api" || !slices.Equal(api.EnvKeys, []string{"PORT"}) {
		t.Errorf("api changed %v running %q env %v", api.Changed, api.RunningCmd, api.EnvKeys)
	}
	if p := get("panel"); p.State != core.StateAbsent || p.HasSession || p.Log != ".logs/panel.log" {
		t.Errorf("panel: %+v", p)
	}
	old := get("old-mock")
	if old.Kind != core.KindAdhoc || old.State != core.StateStopped || old.Cwd != "tools" || !slices.Equal(old.LastPorts, []int{12000}) {
		t.Errorf("old-mock: %+v", old)
	}
	ms := get("mock-stripe")
	if ms.Kind != core.KindAdhoc || ms.Cmd != "node scripts/mock-stripe.js" || !slices.Equal(ms.EnvKeys, []string{"STRIPE_KEY"}) || !slices.Equal(ms.PortNumbers(), []int{12111}) {
		t.Errorf("mock-stripe: %+v", ms)
	}

	if !st.Docker.Available || len(st.Docker.Unowned) != 1 || st.Docker.Unowned[0].Project != "scratch" {
		t.Errorf("docker = %+v", st.Docker)
	}
	want := "worker-rust: :9101 is held by pid 7712 (worker-search-r, not managed by rov)"
	if !slices.Contains(st.Warnings, want) || len(st.Warnings) != 1 {
		t.Errorf("warnings = %q, want [%q]", st.Warnings, want)
	}
}

func TestComposeDockerAndConfigProblems(t *testing.T) {
	in := testenv.SampleInputs(t)
	in.Containers = nil
	in.DockerErr = fmt.Errorf("%w: no docker binary", docker.ErrUnavailable)
	in.ConfigErr = &config.ValidationError{Path: "rov.yaml", Problems: []config.Problem{{Line: 9, Process: "panel", Msg: "cmd is required"}}}
	st := core.Compose(in)
	if st.Docker.Available {
		t.Error("docker available")
	}
	if !slices.ContainsFunc(st.Warnings, func(w string) bool { return strings.HasPrefix(w, "docker: unavailable") }) {
		t.Errorf("no docker warning: %q", st.Warnings)
	}
	if !slices.Contains(st.Warnings, `rov.yaml:9: process "panel": cmd is required`) {
		t.Errorf("no config warning: %q", st.Warnings)
	}
	if p := st.Get("panel"); !slices.Equal(p.Problems, []string{"cmd is required"}) {
		t.Errorf("panel problems = %q", p.Problems)
	}
	// Without containers infra shows its own state and its stored last ports.
	if p := st.Get("infra"); p.DockerState != "" || len(p.Ports) != 0 || !slices.Equal(p.LastPorts, []int{6379, 8123}) {
		t.Errorf("infra: %+v", p)
	}
}

func TestComposeWithoutPorts(t *testing.T) {
	in := testenv.SampleInputs(t)
	in.Ports = nil
	in.Containers = nil
	st := core.Compose(in)
	if p := st.Get("gateway"); len(p.Ports) != 0 || !slices.Equal(p.LastPorts, []int{8080}) {
		t.Errorf("gateway without a scan: %+v", p)
	}
	if len(st.Warnings) != 0 {
		t.Errorf("warnings without a scan: %q", st.Warnings)
	}
}

func TestBuildHostPorts(t *testing.T) {
	in := testenv.SampleInputs(t)
	st := core.Compose(in)
	rows := core.BuildHostPorts(st, in.Ports, nil)
	type row struct {
		port        int
		addr, owner string
		pid         int
		source      string
		visible     bool
	}
	var got []row
	for _, r := range rows {
		got = append(got, row{r.Port, r.Addr, r.Owner, r.PID, r.Source, r.Visible})
	}
	want := []row{
		{3001, "0.0.0.0", "api", 41530, "proc", true},
		{5432, "0.0.0.0", "", 0, "proc", false},
		{6379, "127.0.0.1", "infra/redis", 0, "docker", true},
		{8080, "0.0.0.0", "gateway", 41301, "proc", true},
		{8080, "::", "gateway", 41301, "proc", true},
		{8090, "127.0.0.1", "tracking", 41402, "proc", true},
		{8091, "127.0.0.1", "tracking", 41402, "proc", true},
		{8123, "127.0.0.1", "infra/clickhouse", 0, "docker", true},
		{9101, "0.0.0.0", "", 7712, "proc", true},
		{12111, "127.0.0.1", "mock-stripe", 51002, "proc", true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows:\n got %v\nwant %v", got, want)
	}
}

func TestStatusJSONIsStable(t *testing.T) {
	st := core.Compose(testenv.SampleInputs(t))
	b, err := core.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"project":"myapp"`, `"exit_code":null`, `"pid":null`, `"started_at":"2026-11-07T08:01:12Z"`, `"docker":{"available":true`, `"unowned":[{"project":"scratch"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON lacks %s", key)
		}
	}
	if strings.Contains(string(b), `&`) {
		t.Error("JSON escapes HTML")
	}
}
