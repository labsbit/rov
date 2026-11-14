package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/testenv"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (go test -update rewrites it)\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func sample(t *testing.T) *core.Status { return core.Compose(testenv.SampleInputs(t)) }

func TestGoldenStatus(t *testing.T) {
	st := sample(t)
	var b bytes.Buffer
	writeStatus(&b, st, st.Time)
	golden(t, "status.txt", b.Bytes())

	a := &App{Stdout: &b}
	b.Reset()
	if err := a.writeJSON(st); err != nil {
		t.Fatal(err)
	}
	golden(t, "status.json", b.Bytes())
}

func TestGoldenList(t *testing.T) {
	var b bytes.Buffer
	writeList(&b, sample(t))
	golden(t, "list.txt", b.Bytes())
}

func TestGoldenShow(t *testing.T) {
	st := sample(t)
	var b bytes.Buffer
	for _, n := range []string{"infra", "api", "worker-rust"} {
		writeShow(&b, st.Get(n), st.Time, time.UTC)
		b.WriteString("\n")
	}
	golden(t, "show.txt", b.Bytes())

	b.Reset()
	a := &App{Stdout: &b}
	if err := a.writeJSON(withField(*st.Get("gateway"), "lines", []string{"listening on :8080", "a <b> && c"})); err != nil {
		t.Fatal(err)
	}
	golden(t, "show.json", b.Bytes())
}

func TestGoldenPorts(t *testing.T) {
	in := testenv.SampleInputs(t)
	st := core.Compose(in)
	others := []core.Container{{Name: "elsewhere-redis", Project: "elsewhere", Service: "redis", State: "running",
		Ports: []core.Port{{Port: 16379, Addr: "127.0.0.1", Source: core.SourceDocker, Container: "elsewhere-redis"}}}}
	rows := core.BuildHostPorts(st, in.Ports, others)
	var b bytes.Buffer
	writePorts(&b, rows)
	golden(t, "ports.txt", b.Bytes())

	b.Reset()
	a := &App{Stdout: &b}
	if err := a.writeJSON(rows); err != nil {
		t.Fatal(err)
	}
	golden(t, "ports.json", b.Bytes())
}

func TestUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                            "-",
		45 * time.Second:             "45s",
		4*time.Minute + time.Second:  "4m",
		2*time.Hour + 13*time.Minute: "2h13m",
		76 * time.Hour:               "3d4h",
	} {
		if got := fmtUptime(d); got != want {
			t.Errorf("fmtUptime(%s) = %q, want %q", d, got, want)
		}
	}
}
