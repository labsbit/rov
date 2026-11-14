package configedit

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/labsbit/rov/internal/config"
)

// FuzzApply feeds arbitrary files to every kind of op. Apply must never
// panic, and whatever it returns without an error must be a valid rov.yaml
// that keeps the lines the op doesn't touch:
//
//   - Move keeps every non-blank line (it only reorders them);
//   - Add then Remove of the new entry gives back the input's non-blank lines;
//   - Set keeps, in order, every comment line outside the entry it edits.
//
// who picks the process the second group of ops works on.
func FuzzApply(f *testing.F) {
	for _, name := range []string{"spec", "commented"} {
		data, err := os.ReadFile(filepath.Join("testdata", name+".yaml"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, uint8(0))
		f.Add(data, uint8(3))
	}
	f.Add(config.StarterYAML("demo"), uint8(0))
	f.Add([]byte(""), uint8(0))
	f.Add([]byte("processes:\n  a: { cmd: x,\n    cwd: d }\n  b:\n    cmd: |\n      #x\n\n      y\n"), uint8(1))
	f.Add([]byte("processes: {}\r\n"), uint8(0))
	f.Add([]byte("processes:\n  a: &x { cmd: y }\n  b: *x\n"), uint8(0))
	f.Add([]byte(lowCommentInEntry), uint8(0))
	f.Add([]byte("processes:\n  api:\n    cmd: x\n    env:\n      A: \"1\"\n  # B: 2\n      C: \"3\"\n  web: { cmd: y }\n"), uint8(0))
	f.Add([]byte(flowWithCommentLine), uint8(0))
	f.Add([]byte("processes:\n  a: {cmd: x}\n  # c\n  b: {cmd: y}\n\n  # d\n\n  c: {cmd: z}\nlog_dir: x\n"), uint8(2))

	ops := []Op{
		Add{Proc: config.Process{Name: "new", Cmd: "run"}, Pos: Position{First: true}},
		Add{Proc: mockS3, Pos: Position{After: "a"}},
		Add{Proc: config.Process{Name: "new", Cmd: "run"}},
		Set{Name: "a", Patch: Patch{Cmd: new("y"), Rename: new("renamed"), Description: new("d")}},
		Set{Name: "a", Patch: Patch{Env: &[]config.EnvVar{{Key: "K", Value: "v"}}, Cwd: new("")}},
		Remove{Name: "a"},
		Move{Name: "a", Pos: Position{Down: true}},
		Move{Name: "b", Pos: Position{First: true}},
	}
	f.Fuzz(func(t *testing.T, src []byte, who uint8) {
		cfg, _ := config.Parse("rov.yaml", src)
		var name string
		if cfg != nil && len(cfg.Processes) > 0 {
			name = cfg.Processes[int(who)%len(cfg.Processes)].Name
			ops := ops[:len(ops):len(ops)]
			for _, pos := range []Position{{First: true}, {Up: true}, {Down: true}, {Last: true}} {
				ops = append(ops, Move{Name: name, Pos: pos})
			}
			ops = append(ops,
				Set{Name: name, Patch: Patch{Cmd: new("zz")}},
				Set{Name: name, Patch: Patch{Description: new("dd"), Rename: new("zz-renamed")}},
				Set{Name: name, Patch: Patch{Env: &[]config.EnvVar{{Key: "K", Value: "v"}}}},
				Add{Proc: config.Process{Name: "zz-new", Cmd: "q", Description: "d"}, Pos: Position{After: name}},
			)
		}
		for _, op := range append(ops, Add{Proc: config.Process{Name: "zz-new", Cmd: "q", Description: "d"}}) {
			out, err := Apply(src, op)
			if err != nil {
				continue
			}
			if _, err := config.Parse("rov.yaml", out); err != nil {
				t.Fatalf("%#v on %q returned an invalid file %q: %v", op, src, out, err)
			}
			switch op := op.(type) {
			case Move:
				if !slices.Equal(sorted(nonBlank(src)), sorted(nonBlank(out))) {
					t.Fatalf("%#v changed lines:\n%s\n---\n%s", op, src, out)
				}
			case Add:
				back, err := Apply(out, Remove{Name: op.Proc.Name})
				if err != nil {
					t.Fatalf("%#v then Remove failed on %q: %v\n%s", op, src, err, out)
				}
				want, got := nonBlank(src), nonBlank(back)
				if len(cfg.Processes) == 0 {
					// Add writes "processes: {}" as "processes:", or adds the
					// key; Remove leaves it.
					want, got = withoutProcessesKey(want), withoutProcessesKey(got)
				}
				if !slices.Equal(want, got) {
					t.Fatalf("%#v then Remove changed lines:\n%s\n---\n%s\n---\n%s", op, src, out, back)
				}
			case Set:
				if lost := lostOutside(src, op.Name, out); lost != "" {
					t.Fatalf("%#v lost the comment %q:\n%s\n---\n%s", op, lost, src, out)
				}
			}
		}
	})
}

// nonBlank returns the non-blank lines of s, without line endings.
func nonBlank(s []byte) []string {
	var out []string
	for l := range strings.SplitSeq(strings.ReplaceAll(string(s), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func withoutProcessesKey(lines []string) []string {
	return slices.DeleteFunc(lines, func(l string) bool { return strings.HasPrefix(l, "processes:") })
}

func sorted(s []string) []string { slices.Sort(s); return s }

func isSubsequence(want, got []string) bool {
	_, ok := subsequence(want, got)
	return ok
}

// lostOutside returns a comment line of src outside the entry name that is
// not in out, in order, or "". The entry is worked out from the node tree
// alone, independently of how Apply delimits it: its key line through the
// last line a node of its value starts on, and after that the comments
// indented deeper than its key, up to the next entry.
func lostOutside(src []byte, name string, out []byte) string {
	var top yaml.Node
	if yaml.Unmarshal(src, &top) != nil || len(top.Content) == 0 {
		return ""
	}
	var procs *yaml.Node
	root := top.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "processes" {
			procs = root.Content[i+1]
		}
	}
	if procs == nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	from, to, next, col := -1, -1, len(lines), 0
	for i := 0; i+1 < len(procs.Content); i += 2 {
		if k := procs.Content[i]; k.Value == name {
			from, to, col = k.Line-1, lastLine(procs.Content[i+1]), k.Column-1
			if i+2 < len(procs.Content) {
				next = procs.Content[i+2].Line - 1
			}
		}
	}
	if from < 0 {
		return ""
	}
	var want []string
	for l, line := range lines {
		inside := l > from && l <= to || l > to && l < next && indentOf(line) > col
		if isComment(line) && !inside {
			want = append(want, line)
		}
	}
	var got []string
	for _, line := range nonBlank(out) {
		if isComment(line) {
			got = append(got, line)
		}
	}
	missing, _ := subsequence(want, got)
	return missing
}
