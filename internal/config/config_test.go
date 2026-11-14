package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSpecExample(t *testing.T) {
	data, err := os.ReadFile("testdata/spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse("rov.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogDir != ".logs" || cfg.LogMax != 64<<20 {
		t.Errorf("log settings = %q, %d", cfg.LogDir, cfg.LogMax)
	}
	wantNames := []string{"infra", "gateway", "tracking", "panel", "api", "web", "worker-go", "worker-rust", "fetch-py"}
	if got := cfg.Names(); !reflect.DeepEqual(got, wantNames) {
		t.Errorf("names = %v, want %v", got, wantNames)
	}
	infra, _ := cfg.Get("infra")
	want := Process{Name: "infra", Cmd: "just infra", Description: "Redis + ClickHouse (docker)", Docker: "search-api-gateway-local"}
	if !reflect.DeepEqual(infra, want) {
		t.Errorf("infra = %+v", infra)
	}
	if p, _ := cfg.Get("fetch-py"); p.Cmd != `just fetch-serve "fetch-resi,fetch-mobile"` {
		t.Errorf("fetch-py cmd = %q", p.Cmd)
	}
	if _, ok := cfg.Get("nope"); ok {
		t.Error("Get(nope) found something")
	}
}

func TestParseValid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want *Config
	}{
		{"empty file", "", &Config{Path: "rov.yaml", LogDir: ".logs", LogMax: DefaultLogMax}},
		{"comments only", "# nothing yet\n", &Config{Path: "rov.yaml", LogDir: ".logs", LogMax: DefaultLogMax}},
		{"null processes", "processes:\n", &Config{Path: "rov.yaml", LogDir: ".logs", LogMax: DefaultLogMax}},
		{
			"settings and env order",
			"log_dir: var/log\nlog_max: 512 KiB\nprocesses:\n  a:\n    cmd: run\n    cwd: apps/a\n    env:\n      Z: 1\n      A: \"two\"\n      EMPTY:\n",
			&Config{Path: "rov.yaml", LogDir: "var/log", LogMax: 512 << 10, Processes: []Process{{
				Name: "a", Cmd: "run", Cwd: "apps/a",
				Env: []EnvVar{{"Z", "1"}, {"A", "two"}, {"EMPTY", ""}},
			}}},
		},
		{"plain byte log_max", "log_max: 4096\n", &Config{Path: "rov.yaml", LogDir: ".logs", LogMax: 4096}},
		{
			"anchors",
			"processes:\n  a: &base { cmd: run }\n  b: *base\n",
			&Config{Path: "rov.yaml", LogDir: ".logs", LogMax: DefaultLogMax, Processes: []Process{{Name: "a", Cmd: "run"}, {Name: "b", Cmd: "run"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse("rov.yaml", []byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseProblems(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string // substrings, one per expected problem line, in order
	}{
		{"typo in field", "processes:\n  api:\n    comand: x\n", []string{
			`rov.yaml:3: process "api": unknown key "comand" (did you mean "cmd"?)`,
			`rov.yaml:2: process "api": cmd is required`,
		}},
		{"alias", "processes:\n  api: { command: x }\n", []string{`unknown key "command" (did you mean "cmd"?)`, "cmd is required"}},
		{"unknown field no hint", "processes:\n  api: { cmd: x, ports: 1 }\n", []string{`unknown key "ports" (expected cmd, cwd, env, description, docker)`}},
		{"top-level typo", "log_dri: x\n", []string{`rov.yaml:1: unknown key "log_dri" (did you mean "log_dir"?)`}},
		{"process at top level", "gateway:\n  cmd: just gateway\n", []string{`unknown key "gateway" (processes go under processes:)`}},
		{"bad name", "processes:\n  Api: { cmd: x }\n", []string{`process "Api": invalid name "Api"`}},
		{"dotted name", "processes:\n  a.b: { cmd: x }\n", []string{`invalid name "a.b"`}},
		{"duplicate", "processes:\n  a: { cmd: x }\n  a: { cmd: y }\n", []string{`rov.yaml:3: process "a": defined more than once (first on line 2)`}},
		{"blank cmd", "processes:\n  a: { cmd: '  ' }\n", []string{"cmd is required"}},
		{"cmd not a string", "processes:\n  a: { cmd: [x] }\n", []string{"cmd must be a string", "cmd is required"}},
		{"absolute cwd", "processes:\n  a: { cmd: x, cwd: /tmp }\n", []string{`cwd "/tmp" must be relative to the project root`}},
		{"escaping cwd", "processes:\n  a: { cmd: x, cwd: apps/../../x }\n", []string{`cwd "apps/../../x" leaves the project root`}},
		{"bad env key", "processes:\n  a:\n    cmd: x\n    env: { 1X: y }\n", []string{`rov.yaml:4: process "a": invalid env key "1X"`}},
		{"env not a map", "processes:\n  a:\n    cmd: x\n    env: [A=b]\n", []string{"env must be a mapping"}},
		{"bad docker", "processes:\n  a: { cmd: x, docker: My_Proj }\n", []string{`invalid compose project "My_Proj"`}},
		{"processes not a map", "processes: [a, b]\n", []string{"processes must be a mapping"}},
		{"process not a map", "processes:\n  a: just a\n", []string{`process "a": must be a mapping such as { cmd: … }`, "cmd is required"}},
		{"bad log_max", "log_max: 64MB\n", []string{`log_max: invalid size "64MB"`}},
		{"empty log_dir", "log_dir: ''\n", []string{"log_dir is empty"}},
		// rov deletes old logs in log_dir: it must be a directory of its own.
		{"log_dir is the root", "log_dir: .\n", []string{`rov.yaml:1: log_dir "." is the project root`}},
		{"log_dir is the root, spelled out", "log_dir: logs/..\n", []string{`log_dir "logs/.." is the project root`}},
		{"absolute log_dir", "log_dir: /tmp\n", []string{`log_dir "/tmp" must be relative to the project root`}},
		{"escaping log_dir", "log_dir: ../shared\n", []string{`log_dir "../shared" leaves the project root`}},
		{"field twice", "processes:\n  a: { cmd: x, cmd: y }\n", []string{"cmd is set twice"}},
		{"two documents", "processes:\n---\nprocesses:\n", []string{"more than one YAML document"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse("rov.yaml", []byte(tt.in))
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("got %v, want a *ValidationError", err)
			}
			if cfg == nil {
				t.Fatal("config is nil on a validation error")
			}
			lines := strings.Split(err.Error(), "\n")
			if len(lines) != len(tt.want) {
				t.Fatalf("got %d problems, want %d:\n%s", len(lines), len(tt.want), err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("missing %q in:\n%s", w, err)
				}
			}
		})
	}
}

func TestParseFatal(t *testing.T) {
	for _, in := range []string{"processes: [\n", "- a\n- b\n", "just a string\n"} {
		cfg, err := Parse("x/rov.yaml", []byte(in))
		if err == nil || cfg != nil {
			t.Errorf("%q: got cfg=%v err=%v, want nil config and an error", in, cfg, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "x/rov.yaml") {
			t.Errorf("%q: error %q does not name the file", in, err)
		}
	}
}

func TestValidate(t *testing.T) {
	c := Default("rov.yaml")
	c.Processes = []Process{{Name: "a", Cmd: "x"}, {Name: "a", Cmd: "y"}, {Name: "B"}}
	err := c.Validate()
	for _, w := range []string{`process "a": defined more than once`, `process "B": invalid name`, `process "B": cmd is required`} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("Validate() = %v, want it to contain %q", err, w)
		}
	}
	if err := Default("rov.yaml").Validate(); err != nil {
		t.Errorf("default config: %v", err)
	}
	for dir, want := range map[string]string{".": "is the project root", "/var/log": "must be relative", "../x": "leaves the project root", "": "log_dir is empty"} {
		c := Default("rov.yaml")
		c.LogDir = dir
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("log_dir %q: Validate() = %v, want it to contain %q", dir, err, want)
		}
	}
	for _, dir := range []string{".logs", "var/log", "./logs", "a/../b"} {
		if err := ValidLogDir(dir); err != nil {
			t.Errorf("ValidLogDir(%q) = %v", dir, err)
		}
	}
}

func TestValidName(t *testing.T) {
	for name, ok := range map[string]bool{
		"gateway": true, "worker-go": true, "a_b": true, "9lives": true, "x": true,
		"": false, "-a": false, "_a": false, "Api": false, "a.b": false, "a:b": false, "a b": false, "ä": false,
	} {
		if err := ValidName(name); (err == nil) != ok {
			t.Errorf("ValidName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
}

func TestValidEnvKey(t *testing.T) {
	for key, ok := range map[string]bool{
		"PORT": true, "_X": true, "a1": true, "": false, "1A": false, "A-B": false, "A=B": false,
	} {
		if err := ValidEnvKey(key); (err == nil) != ok {
			t.Errorf("ValidEnvKey(%q) = %v, want ok=%v", key, err, ok)
		}
	}
}

func TestCheckCwd(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "apps", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		cwd, err string
	}{
		{"", ""},
		{".", ""},
		{"apps/web", ""},
		{"apps/web/", ""},
		{"apps/nope", "does not exist"},
		{"file", "is a file"},
		{"../x", "leaves the project root"},
		{"/etc", "must be relative"},
	}
	for _, tt := range tests {
		err := CheckCwd(root, tt.cwd)
		if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("CheckCwd(%q) = %v, want %q", tt.cwd, err, tt.err)
		}
	}
}

func TestSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		fmt  string
	}{
		{"64MiB", 64 << 20, "64MiB"},
		{"64 mib", 64 << 20, "64MiB"},
		{"512KiB", 512 << 10, "512KiB"},
		{"1GiB", 1 << 30, "1GiB"},
		{"1536KiB", 1536 << 10, "1536KiB"},
		{"2048", 2048, "2KiB"},
		{"1000", 1000, "1000"},
		{"7B", 7, "7"},
		{" 3MiB ", 3 << 20, "3MiB"},
	}
	for _, tt := range tests {
		got, err := ParseSize(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
			continue
		}
		if f := FormatSize(got); f != tt.fmt {
			t.Errorf("FormatSize(%d) = %q, want %q", got, f, tt.fmt)
		}
	}
	for _, bad := range []string{"", "0", "0MiB", "-1", "1.5GiB", "64MB", "MiB", "64 MiB extra", "x", "99999999999GiB", "9223372036854775808"} {
		if n, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", bad, n)
		}
	}
}

func TestSuggest(t *testing.T) {
	tests := []struct{ key, want string }{
		{"comand", "cmd"}, {"cdm", "cmd"}, {"command", "cmd"}, {"desc", "description"},
		{"descripton", "description"}, {"working_dir", "cwd"}, {"enviroment", "env"},
		{"dokcer", "docker"}, {"zzz", ""}, {"ports", ""},
	}
	for _, tt := range tests {
		if got := suggest(tt.key, processKeys); got != tt.want {
			t.Errorf("suggest(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
