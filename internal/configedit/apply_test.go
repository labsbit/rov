package configedit

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labsbit/rov/internal/config"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

// fixture returns a named input: a file in testdata, or the starter file.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	if name == "starter" {
		return config.StarterYAML("demo")
	}
	data, err := os.ReadFile(filepath.Join("testdata", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func env(kv ...string) []config.EnvVar {
	var out []config.EnvVar
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, config.EnvVar{Key: kv[i], Value: kv[i+1]})
	}
	return out
}

func flowProc(name, cmd string) config.Process { return config.Process{Name: name, Cmd: cmd} }

var mockS3 = config.Process{
	Name: "mock-s3", Cmd: "go run .", Cwd: "tools/mock-s3",
	Env: env("AWS_REGION", "eu-central-1"), Description: "local S3 for upload tests",
}

// goldenCases run an op on a fixture and compare with
// testdata/golden/<fixture>/<name>.yaml. Run with -update to rewrite them,
// then review the diff.
var goldenCases = []struct {
	fixture, name string
	op            Op
}{
	// The spec's example.
	{"spec", "add-first", Add{Proc: flowProc("mock-s3", "go run ./tools/mock-s3"), Pos: Position{First: true}}},
	{"spec", "add-after", Add{Proc: flowProc("mock-s3", "go run ./tools/mock-s3"), Pos: Position{After: "api"}}},
	{"spec", "add-last", Add{Proc: flowProc("mock-s3", "go run ./tools/mock-s3")}},
	{"spec", "add-block-after", Add{Proc: mockS3, Pos: Position{After: "gateway"}}},
	{"spec", "add-quoted", Add{Proc: flowProc("odd", `printf '%s, %s\n' a "b" # not a comment`), Pos: Position{Last: true}}},
	{"spec", "set-cmd", Set{Name: "gateway", Patch: Patch{Cmd: new("just gateway-dev"), Description: new("gateway, dev build")}}},
	{"spec", "set-cmd-flow", Set{Name: "fetch-py", Patch: Patch{Cmd: new("just fetch-serve fetch-resi")}}},
	{"spec", "set-env", Set{Name: "gateway", Patch: Patch{Env: &[]config.EnvVar{{Key: "PORT", Value: "8080"}, {Key: "LOG_LEVEL", Value: "debug"}}}}},
	{"spec", "set-flow-gains-field", Set{Name: "tracking", Patch: Patch{Description: new("tracking service"), Cwd: new("apps/tracking-service")}}},
	{"spec", "set-docker-keeps-comment", Set{Name: "infra", Patch: Patch{Docker: new("sag")}}},
	{"spec", "set-drop-field", Set{Name: "infra", Patch: Patch{Description: new("")}}},
	{"spec", "rename-block", Set{Name: "infra", Patch: Patch{Rename: new("infrastructure")}}},
	{"spec", "rename-flow", Set{Name: "worker-rust", Patch: Patch{Rename: new("wr")}}},
	{"spec", "rename-flow-longer", Set{Name: "api", Patch: Patch{Rename: new("api-service-v2")}}},
	{"spec", "rm-block", Remove{Name: "infra"}},
	{"spec", "rm-flow", Remove{Name: "api"}},
	{"spec", "rm-last", Remove{Name: "fetch-py"}},
	{"spec", "mv-up", Move{Name: "worker-rust", Pos: Position{Up: true}}},
	{"spec", "mv-down", Move{Name: "infra", Pos: Position{Down: true}}},
	{"spec", "mv-first", Move{Name: "fetch-py", Pos: Position{First: true}}},
	{"spec", "mv-after", Move{Name: "worker-rust", Pos: Position{After: "gateway"}}},
	{"spec", "mv-last", Move{Name: "infra", Pos: Position{Last: true}}},

	// Comments, blank-line separators, a section comment, mixed styles.
	{"commented", "add-first", Add{Proc: flowProc("mock", "node mock.js"), Pos: Position{First: true}}},
	{"commented", "add-after", Add{Proc: mockS3, Pos: Position{After: "gateway"}}},
	{"commented", "add-last", Add{Proc: flowProc("mock", "node mock.js")}},
	{"commented", "set-cmd-keeps-comment", Set{Name: "infra", Patch: Patch{Cmd: new("just infra-up")}}},
	{"commented", "set-env", Set{Name: "gateway", Patch: Patch{Env: &[]config.EnvVar{{Key: "PORT", Value: "9090"}}}}},
	{"commented", "set-add-fields", Set{Name: "infra", Patch: Patch{Cwd: new("infra"), Description: new("redis + clickhouse")}}},
	{"commented", "set-multiline", Set{Name: "web", Patch: Patch{Cmd: new("npm run dev -- --port 5173")}}},
	{"commented", "set-flow-unspaced", Set{Name: "worker-go", Patch: Patch{Cmd: new("go run ./apps/worker-search-go")}}},
	{"commented", "rename-with-comments", Set{Name: "infra", Patch: Patch{Rename: new("infra-local")}}},
	{"commented", "rm-head-comment", Remove{Name: "infra"}},
	{"commented", "rm-after-section", Remove{Name: "worker-go"}},
	{"commented", "rm-last", Remove{Name: "web"}},
	{"commented", "mv-first", Move{Name: "web", Pos: Position{First: true}}},
	{"commented", "mv-down", Move{Name: "infra", Pos: Position{Down: true}}},
	{"commented", "mv-after", Move{Name: "infra", Pos: Position{After: "worker-rust"}}},

	// The starter file from rov init: no processes, a comment block.
	{"starter", "add-flow", Add{Proc: flowProc("web", "npm run dev")}},
	{"starter", "add-block", Add{Proc: mockS3}},
}

func TestGolden(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.fixture+"/"+tc.name, func(t *testing.T) {
			got, err := Apply(fixture(t, tc.fixture), tc.op)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "golden", tc.fixture, tc.name+".yaml")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
				t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", path, got, want)
			}
		})
	}
}

// TestUntouchedLinesSurvive checks the promise behind the goldens on every
// case: each line of the input that doesn't belong to the edited entry is in
// the output unchanged.
func TestUntouchedLinesSurvive(t *testing.T) {
	for _, tc := range goldenCases {
		src := fixture(t, tc.fixture)
		got, err := Apply(src, tc.op)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.fixture, tc.name, err)
		}
		d, err := parseDoc(src)
		if err != nil {
			t.Fatal(err)
		}
		touched := map[int]bool{}
		edits, _ := tc.op.touches()
		for _, e := range d.entries {
			if e.name == edits[0] {
				for l := e.start; l <= e.end; l++ {
					touched[l] = true
				}
			}
		}
		out := string(got)
		for i, l := range d.lines {
			if !touched[i] && !isBlank(l) && !strings.Contains(out, l) {
				t.Errorf("%s/%s: line %d %q is gone", tc.fixture, tc.name, i+1, l)
			}
		}
	}
}

func TestNoOpIsIdentity(t *testing.T) {
	for _, fx := range []string{"spec", "commented", "starter"} {
		src := fixture(t, fx)
		cfg, err := config.Parse("rov.yaml", src)
		if err != nil {
			t.Fatal(err)
		}
		var ops []Op
		if names := cfg.Names(); len(names) > 0 {
			first, last := cfg.Processes[0], cfg.Processes[len(names)-1]
			ops = append(ops,
				Set{Name: first.Name},
				Set{Name: first.Name, Patch: Patch{Cmd: new(first.Cmd), Rename: new(first.Name), Env: &first.Env}},
				Set{Name: last.Name, Patch: Patch{Docker: new("")}},
				Move{Name: first.Name, Pos: Position{Up: true}},
				Move{Name: first.Name, Pos: Position{First: true}},
				Move{Name: last.Name, Pos: Position{Down: true}},
				Move{Name: last.Name, Pos: Position{Last: true}},
			)
			if len(names) > 1 {
				ops = append(ops, Move{Name: names[1], Pos: Position{After: names[0]}})
			}
		}
		for _, op := range ops {
			got, err := Apply(src, op)
			if err != nil {
				t.Errorf("%s: %#v: %v", fx, op, err)
			} else if !bytes.Equal(got, src) {
				t.Errorf("%s: %#v changed the file:\n%s", fx, op, got)
			}
		}
	}
}

// TestAddThenRemove: adding an entry anywhere and removing it again gives
// back the input byte for byte.
func TestAddThenRemove(t *testing.T) {
	for _, fx := range []string{"spec", "commented", "starter"} {
		src := fixture(t, fx)
		cfg, _ := config.Parse("rov.yaml", src)
		positions := []Position{{First: true}, {Last: true}}
		for _, n := range cfg.Names() {
			positions = append(positions, Position{After: n})
		}
		for _, proc := range []config.Process{flowProc("zz", "true"), mockS3} {
			for _, pos := range positions {
				added, err := Apply(src, Add{Proc: proc, Pos: pos})
				if err != nil {
					t.Fatalf("%s: add %s: %v", fx, pos, err)
				}
				back, err := Apply(added, Remove{Name: proc.Name})
				if err != nil {
					t.Fatalf("%s: rm after add %s: %v", fx, pos, err)
				}
				if !bytes.Equal(back, src) {
					t.Errorf("%s: add %s %s then rm is not the identity:\n%s", fx, proc.Name, pos, back)
				}
			}
		}
	}
}

func TestApplyInline(t *testing.T) {
	web := flowProc("web", "npm run dev")
	tests := []struct {
		name, src string
		op        Op
		want      string
	}{
		{"empty file", "", Add{Proc: web}, "processes:\n  web: { cmd: npm run dev }\n"},
		{"no processes key", "log_dir: var/log\n", Add{Proc: web}, "log_dir: var/log\nprocesses:\n  web: { cmd: npm run dev }\n"},
		{"null processes", "processes:\nlog_max: 1MiB\n", Add{Proc: web}, "processes:\n  web: { cmd: npm run dev }\nlog_max: 1MiB\n"},
		{"inline empty map", "processes: {}  # none yet\n", Add{Proc: web}, "processes: # none yet\n  web: { cmd: npm run dev }\n"},
		{"inline null", "processes: ~\n", Add{Proc: web}, "processes:\n  web: { cmd: npm run dev }\n"},
		{"no final newline", "processes:\n  a: { cmd: x }", Add{Proc: web}, "processes:\n  a: { cmd: x }\n  web: { cmd: npm run dev }"},
		{"crlf", "processes:\r\n  a:\r\n    cmd: x\r\n", Add{Proc: mockS3, Pos: Position{First: true}},
			"processes:\r\n  mock-s3:\r\n    cmd: go run .\r\n    cwd: tools/mock-s3\r\n    env:\r\n      AWS_REGION: eu-central-1\r\n    description: local S3 for upload tests\r\n  a:\r\n    cmd: x\r\n"},
		{"four-space indent", "processes:\n    a:\n        cmd: x\n", Add{Proc: config.Process{Name: "b", Cmd: "y", Env: env("K", "v")}},
			"processes:\n    a:\n        cmd: x\n    b:\n        cmd: y\n        env:\n            K: v\n"},
		{"unspaced braces", "processes:\n  a: {cmd: x}\n", Add{Proc: web}, "processes:\n  a: {cmd: x}\n  web: {cmd: npm run dev}\n"},
		{"numbers stay strings", "processes:\n", Add{Proc: config.Process{Name: "n", Cmd: "123", Env: env("PORT", "3000", "DEBUG", "true", "EMPTY", "")}},
			"processes:\n  n:\n    cmd: \"123\"\n    env:\n      PORT: \"3000\"\n      DEBUG: \"true\"\n      EMPTY: \"\"\n"},
		{"multi-line cmd", "processes:\n", Add{Proc: config.Process{Name: "m", Cmd: "a\nb\n", Cwd: "."}},
			"processes:\n  m:\n    cmd: |\n      a\n      b\n    cwd: .\n"},
		{"name that reads as null", "processes:\n", Add{Proc: flowProc("null", "x")}, "processes:\n  \"null\": { cmd: x }\n"},
		{"fill a null entry", "processes:\n  a:\n  b: { cmd: y }\n", Set{Name: "a", Patch: Patch{Cmd: new("x")}},
			"processes:\n  a:\n    cmd: x\n  b: { cmd: y }\n"},
		{"fix an inline null entry", "processes:\n  a: ~  # todo\n", Set{Name: "a", Patch: Patch{Cmd: new("x")}},
			"processes:\n  a: { cmd: x } # todo\n"},
		{"multi-line flow entry", "processes:\n  a: { cmd: x,\n    cwd: d }\n", Set{Name: "a", Patch: Patch{Cwd: new("")}},
			"processes:\n  a: { cmd: x }\n"},
		{"flow keeps key order", "processes:\n  a: { cwd: d, cmd: x }\n", Set{Name: "a", Patch: Patch{Cmd: new("y")}},
			"processes:\n  a: { cwd: d, cmd: y }\n"},
		{"flow drops a field", "processes:\n  a: { cmd: x, cwd: d }   # c\n", Set{Name: "a", Patch: Patch{Cwd: new("")}},
			"processes:\n  a: { cmd: x }           # c\n"},
		{"flow env stays flow", "processes:\n  a:\n    cmd: x\n    env: { A: \"1\" }\n", Set{Name: "a", Patch: Patch{Env: &[]config.EnvVar{{Key: "B", Value: "2"}}}},
			"processes:\n  a:\n    cmd: x\n    env: { B: \"2\" }\n"},
		{"env edit keeps comments", "processes:\n  a:\n    cmd: x\n    env:\n      # first\n      A: \"1\"  # one\n      B: b\n      C: c # gone\n", Set{Name: "a", Patch: Patch{Env: &[]config.EnvVar{{Key: "A", Value: "2"}, {Key: "B", Value: "b"}, {Key: "D", Value: "d"}}}},
			"processes:\n  a:\n    cmd: x\n    env:\n      # first\n      A: \"2\"  # one\n      B: b\n      D: d\n"},
		{"env reorder renders afresh", "processes:\n  a:\n    cmd: x\n    env:\n      A: \"1\"  # one\n      B: b\n", Set{Name: "a", Patch: Patch{Env: &[]config.EnvVar{{Key: "B", Value: "b"}, {Key: "A", Value: "1"}}}},
			"processes:\n  a:\n    cmd: x\n    env:\n      B: b\n      A: \"1\"\n"},
		{"drop env", "processes:\n  a:\n    cmd: x\n    env:\n      A: b\n    cwd: d\n", Set{Name: "a", Patch: Patch{Env: &[]config.EnvVar{}}},
			"processes:\n  a:\n    cmd: x\n    cwd: d\n"},
		{"new field goes in field order", "processes:\n  a:\n    docker: p\n    cmd: x\n", Set{Name: "a", Patch: Patch{Description: new("d")}},
			"processes:\n  a:\n    description: d\n    docker: p\n    cmd: x\n"},
		{"remove only entry", "processes:\n  a: { cmd: x }\nlog_max: 1MiB\n", Remove{Name: "a"}, "processes:\nlog_max: 1MiB\n"},
		{"remove keeps section comment", "processes:\n  a: { cmd: x }\n\n  # section\n\n  b: { cmd: y }\n\n  c: { cmd: z }\n", Remove{Name: "b"},
			"processes:\n  a: { cmd: x }\n\n  # section\n\n  c: { cmd: z }\n"},
		{"remove last keeps blank before next key", "processes:\n  a: { cmd: x }\n\n  b: { cmd: y }\n\nlog_max: 1MiB\n", Remove{Name: "b"},
			"processes:\n  a: { cmd: x }\n\nlog_max: 1MiB\n"},
		{"add between blank-separated", "processes:\n  a: { cmd: x }\n\n  b: { cmd: y }\n", Add{Proc: flowProc("c", "z"), Pos: Position{After: "a"}},
			"processes:\n  a: { cmd: x }\n\n  c: { cmd: z }\n\n  b: { cmd: y }\n"},
		{"swap keeps gaps", "processes:\n  a: { cmd: x }\n\n  # section\n\n  b: { cmd: y }\n", Move{Name: "b", Pos: Position{Up: true}},
			"processes:\n  b: { cmd: y }\n\n  # section\n\n  a: { cmd: x }\n"},
		{"rename quoted key", "processes:\n  'a': { cmd: x }\n", Set{Name: "a", Patch: Patch{Rename: new("b")}}, "processes:\n  b: { cmd: x }\n"},
		{"comment char inside quotes", "processes:\n  a: { cmd: 'echo #1' }  # real\n", Set{Name: "a", Patch: Patch{Rename: new("bb")}},
			"processes:\n  bb: { cmd: 'echo #1' } # real\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply([]byte(tt.src), tt.op)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestApplyErrors(t *testing.T) {
	spec := string(fixture(t, "spec"))
	tests := []struct {
		name, src string
		op        Op
		want      string
	}{
		{"add existing", spec, Add{Proc: flowProc("api", "x")}, `process "api" already exists`},
		{"add bad name", spec, Add{Proc: flowProc("My.App", "x")}, `invalid name "My.App"`},
		{"add without cmd", spec, Add{Proc: config.Process{Name: "x"}}, `process "x": cmd is required`},
		{"add after unknown", spec, Add{Proc: flowProc("x", "y"), Pos: Position{After: "nope"}}, `no process "nope" in rov.yaml (it has infra, gateway`},
		{"add up", spec, Add{Proc: flowProc("x", "y"), Pos: Position{Up: true}}, "up and down only move"},
		{"two positions", spec, Move{Name: "api", Pos: Position{First: true, After: "web"}}, "give one position"},
		{"bad env key", spec, Add{Proc: config.Process{Name: "x", Cmd: "y", Env: env("1A", "b")}}, `invalid env key "1A"`},
		{"escaping cwd", spec, Add{Proc: config.Process{Name: "x", Cmd: "y", Cwd: "../z"}}, `leaves the project root`},
		{"set unknown", spec, Set{Name: "nope", Patch: Patch{Cmd: new("x")}}, `no process "nope"`},
		{"set empty cmd", spec, Set{Name: "api", Patch: Patch{Cmd: new("")}}, `process "api": cmd is required`},
		{"rename taken", spec, Set{Name: "api", Patch: Patch{Rename: new("web")}}, `can't rename "api" to "web": that name is taken`},
		{"rename invalid", spec, Set{Name: "api", Patch: Patch{Rename: new("API")}}, `invalid name "API"`},
		{"rm unknown", spec, Remove{Name: "nope"}, `no process "nope"`},
		{"rm from empty", "", Remove{Name: "nope"}, `no process "nope" in rov.yaml (it has none)`},
		{"mv after itself", spec, Move{Name: "api", Pos: Position{After: "api"}}, "after itself"},
		{"invalid yaml", "processes: [\n", Add{Proc: flowProc("a", "b")}, "invalid YAML"},
		{"lone CR", "processes:\r  a: { cmd: x }\n", Add{Proc: flowProc("b", "y")}, "line break other than LF or CRLF"},
		{"final lone CR", "processes:\n  a: { cmd: x }\r", Add{Proc: flowProc("b", "y")}, "line break other than LF or CRLF"},
		{"line separator", "processes:\n  a: { cmd: \"x\u2028y\" }\n  b: { cmd: z }\n", Remove{Name: "b"}, "line break other than LF or CRLF"},
		{"flow processes", "processes: { a: { cmd: x } }\n", Add{Proc: flowProc("b", "y")}, "processes is a flow mapping"},
		{"problem elsewhere", "processes:\n  a: { comand: x }\n", Add{Proc: flowProc("b", "y")}, `rov.yaml:2: process "a": unknown key "comand"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply([]byte(tt.src), tt.op)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v (output %q), want an error containing %q", err, got, tt.want)
			}
		})
	}
}

func TestSplitComment(t *testing.T) {
	tests := []struct{ line, code, comment string }{
		{"  a: b # c\n", "  a: b ", "# c"},
		{"  a: 'b # c'\n", "  a: 'b # c'", ""},
		{`  a: "b \" # c" # d`, `  a: "b \" # c" `, "# d"},
		{"  a: it's # c", "  a: it's ", "# c"},
		{"  a: b#c", "  a: b#c", ""},
		{"  # whole line", "  ", "# whole line"},
		{"  a: { cmd: 'x # y', cwd: z }  # c", "  a: { cmd: 'x # y', cwd: z }  ", "# c"},
		{"  a: 'it''s # x'", "  a: 'it''s # x'", ""},
	}
	for _, tt := range tests {
		code, comment := splitComment(tt.line)
		if code != tt.code || comment != tt.comment {
			t.Errorf("splitComment(%q) = %q, %q; want %q, %q", tt.line, code, comment, tt.code, tt.comment)
		}
	}
}

// lowCommentInEntry has a comment line, indented no deeper than the entry
// keys, between two fields of an entry.
const lowCommentInEntry = "processes:\n  api:\n    cmd: bun run dev\n# cwd: apps/api\n    description: the api\n  web: { cmd: npm run dev }\n"

// flowWithCommentLine has a one-line flow entry followed by a comment line
// indented deeper than its key, which trails it.
const flowWithCommentLine = "processes:\n  web:   { cmd: npm run dev }   # vite\n    # needs PORT from .env.local\n  api:   { cmd: bun dev }\n"

// TestCommentLinesInsideEntries: a comment line's indentation doesn't end an
// entry (or an env map) whose fields go on below it, and a comment line
// trailing a one-line flow entry doesn't stop it being edited in place.
// These used to panic, be refused, or drop the comment.
func TestCommentLinesInsideEntries(t *testing.T) {
	envSrc := "processes:\n  api:\n    cmd: x\n    env:\n      A: \"1\"\n  # B: \"2\"\n      C: \"3\"\n  web: { cmd: y }\n"
	tests := []struct {
		name, src string
		op        Op
		want      string
	}{
		{"set cmd", lowCommentInEntry, Set{Name: "api", Patch: Patch{Cmd: new("bun dev")}},
			"processes:\n  api:\n    cmd: bun dev\n# cwd: apps/api\n    description: the api\n  web: { cmd: npm run dev }\n"},
		{"set description", lowCommentInEntry, Set{Name: "api", Patch: Patch{Description: new("x")}},
			"processes:\n  api:\n    cmd: bun run dev\n# cwd: apps/api\n    description: x\n  web: { cmd: npm run dev }\n"},
		{"rm", lowCommentInEntry, Remove{Name: "api"}, "processes:\n  web: { cmd: npm run dev }\n"},
		{"mv", lowCommentInEntry, Move{Name: "api", Pos: Position{Last: true}},
			"processes:\n  web: { cmd: npm run dev }\n  api:\n    cmd: bun run dev\n# cwd: apps/api\n    description: the api\n"},
		{"add after", lowCommentInEntry, Add{Proc: flowProc("x", "y"), Pos: Position{After: "api"}},
			"processes:\n  api:\n    cmd: bun run dev\n# cwd: apps/api\n    description: the api\n  x: { cmd: y }\n  web: { cmd: npm run dev }\n"},
		{"env keeps comment", envSrc, Set{Name: "api", Patch: Patch{Env: &[]config.EnvVar{{Key: "A", Value: "9"}, {Key: "C", Value: "3"}}}},
			"processes:\n  api:\n    cmd: x\n    env:\n      A: \"9\"\n  # B: \"2\"\n      C: \"3\"\n  web: { cmd: y }\n"},
		{"trailing low comment stays with the next entry", "processes:\n  api:\n    cmd: x\n  # the web app\n  web: { cmd: y }\n", Move{Name: "web", Pos: Position{First: true}},
			"processes:\n  # the web app\n  web: { cmd: y }\n  api:\n    cmd: x\n"},
		{"flow set keeps comment line and padding", flowWithCommentLine, Set{Name: "web", Patch: Patch{Cmd: new("npm run dev -- --host")}},
			"processes:\n  web:   { cmd: npm run dev -- --host } # vite\n    # needs PORT from .env.local\n  api:   { cmd: bun dev }\n"},
		{"flow rename keeps comment line and padding", flowWithCommentLine, Set{Name: "web", Patch: Patch{Rename: new("site")}},
			"processes:\n  site:  { cmd: npm run dev }   # vite\n    # needs PORT from .env.local\n  api:   { cmd: bun dev }\n"},
		{"new flow entry aligns with it", flowWithCommentLine, Add{Proc: flowProc("db", "x")},
			"processes:\n  web:   { cmd: npm run dev }   # vite\n    # needs PORT from .env.local\n  api:   { cmd: bun dev }\n  db:    { cmd: x }\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply([]byte(tt.src), tt.op)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

// TestLostComment: the post-check catches an edit that drops or reorders a
// comment outside the edited entry, and one that drops a moved entry's.
func TestLostComment(t *testing.T) {
	src := "# top\nprocesses:\n  # a's\n  a: { cmd: x }\n  # section\n\n  b:\n    # b's\n    cmd: y\n# end\n"
	d, err := parseDoc([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Parse("rov.yaml", []byte(src))
	tests := []struct {
		op   Op
		out  string
		lost string
	}{
		{Set{Name: "b", Patch: Patch{Cmd: new("z")}}, "# top\nprocesses:\n  # a's\n  a: { cmd: x }\n  # section\n\n  b:\n    cmd: z\n# end\n", ""},
		{Set{Name: "b", Patch: Patch{Cmd: new("z")}}, "# top\nprocesses:\n  # a's\n  a: { cmd: x }\n\n  b:\n    cmd: z\n# end\n", "# section"},
		{Set{Name: "a", Patch: Patch{Cmd: new("z")}}, "# top\nprocesses:\n  a: { cmd: z }\n  # section\n\n  b:\n    # b's\n    cmd: y\n# end\n", ""},
		{Set{Name: "a", Patch: Patch{Cmd: new("z")}}, "processes:\n  a: { cmd: z }\n  # section\n\n  b:\n    # b's\n    cmd: y\n# end\n# top\n", "# section"}, // first one out of order
		{Remove{Name: "a"}, "# top\nprocesses:\n  # section\n\n  b:\n    # b's\n    cmd: y\n# end\n", ""},
		{Remove{Name: "a"}, "# top\nprocesses:\n  b:\n    # b's\n    cmd: y\n# end\n", "# section"},
		{Move{Name: "a", Pos: Position{Last: true}}, "# top\nprocesses:\n  # section\n\n  b:\n    # b's\n    cmd: y\n  # a's\n  a: { cmd: x }\n# end\n", ""},
		{Move{Name: "a", Pos: Position{Last: true}}, "# top\nprocesses:\n  # section\n\n  b:\n    # b's\n    cmd: y\n  a: { cmd: x }\n# end\n", "# a's"},
		{Add{Proc: flowProc("c", "z")}, "# top\nprocesses:\n  # a's\n  a: { cmd: x }\n  # section\n\n  b:\n    cmd: y\n  c: { cmd: z }\n# end\n", "# b's"},
	}
	for _, tt := range tests {
		pl, err := resolve(cfg.Processes, tt.op)
		if err != nil {
			t.Fatal(err)
		}
		got := d.lostComment(tt.op, pl, []byte(tt.out))
		if tt.lost == "" && got != "" || tt.lost != "" && !strings.Contains(got, tt.lost) {
			t.Errorf("%#v on\n%s: got %q, want %q", tt.op, tt.out, got, tt.lost)
		}
	}
}
