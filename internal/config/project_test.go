package config

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realTemp returns a temp dir with symlinks resolved (macOS /var → /private).
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFindRoot(t *testing.T) {
	base := realTemp(t)
	cfgRoot := filepath.Join(base, "cfg")     // rov.yaml, no git
	gitRoot := filepath.Join(base, "git")     // .git dir, no rov.yaml
	both := filepath.Join(base, "git", "sub") // rov.yaml inside a git repo
	worktree := filepath.Join(base, "wt")     // .git file
	none := filepath.Join(base, "none", "a")  // neither
	mkdirs(t, filepath.Join(cfgRoot, "a", "b"), filepath.Join(gitRoot, ".git"), filepath.Join(both, "deep"), worktree, none)
	writeFile(t, filepath.Join(cfgRoot, FileName), "")
	writeFile(t, filepath.Join(both, FileName), "")
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: /elsewhere\n")
	mkdirs(t, filepath.Join(gitRoot, "rov.yaml")) // a directory named rov.yaml doesn't count
	if err := os.Symlink(cfgRoot, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		dir, root string
		has       bool
	}{
		{cfgRoot, cfgRoot, true},
		{filepath.Join(cfgRoot, "a", "b"), cfgRoot, true},
		{gitRoot, gitRoot, false},
		{filepath.Join(both, "deep"), both, true},
		{worktree, worktree, false},
		{filepath.Join(base, "link", "a"), cfgRoot, true},
	}
	for _, tt := range tests {
		root, has, err := FindRoot(tt.dir)
		if err != nil || root != tt.root || has != tt.has {
			t.Errorf("FindRoot(%s) = %s, %v, %v; want %s, %v", tt.dir, root, has, err, tt.root, tt.has)
		}
	}
	// The temp dir may itself sit inside a git checkout; only assert the
	// error when it doesn't.
	if _, _, err := FindRoot(none); err == nil {
		if _, ok := walkUp(none, func(d string) bool { return exists(filepath.Join(d, ".git")) }); !ok {
			t.Error("FindRoot outside any project succeeded")
		}
	} else if !strings.Contains(err.Error(), "rov init") {
		t.Errorf("error %q does not point at rov init", err)
	}
	if _, _, err := FindRoot(filepath.Join(base, "missing")); err == nil {
		t.Error("FindRoot of a missing dir succeeded")
	}
}

func TestFindProject(t *testing.T) {
	root := realTemp(t)
	mkdirs(t, filepath.Join(root, "apps"))
	writeFile(t, filepath.Join(root, FileName), "log_dir: var/logs\nprocesses:\n  api: { cmd: just api }\n")
	p, err := FindProject(filepath.Join(root, "apps"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasConfig || p.Root != root || p.Name != filepath.Base(root) {
		t.Errorf("project = %+v", p)
	}
	if p.Config.Path != filepath.Join(root, FileName) {
		t.Errorf("config path = %s", p.Config.Path)
	}
	if got := p.LogDir(); got != filepath.Join(root, "var", "logs") {
		t.Errorf("LogDir = %s", got)
	}
	if got := p.LockDir(); got != filepath.Join(root, "var", "logs", ".lock") {
		t.Errorf("LockDir = %s", got)
	}

	writeFile(t, filepath.Join(root, FileName), "processes:\n  api: { comand: x }\n")
	if _, err := FindProject(root); err == nil || !strings.Contains(err.Error(), filepath.Join(root, FileName)+":2") {
		t.Errorf("broken config: got %v, want an error naming the file and line", err)
	}
}

func TestFindProjectWithoutConfig(t *testing.T) {
	root := realTemp(t)
	mkdirs(t, filepath.Join(root, ".git"))
	p, err := FindProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.HasConfig || p.Config == nil || len(p.Config.Processes) != 0 {
		t.Errorf("project = %+v", p)
	}
	if p.LogDir() != filepath.Join(root, ".logs") {
		t.Errorf("LogDir = %s", p.LogDir())
	}
}

func TestSocketName(t *testing.T) {
	tests := []struct {
		root string
		base string
	}{
		{"/home/dev/myapp", "myapp"},
		{"/home/dev/My Project.v2", "my-project-v2"},
		{"/srv/__x__", "__x__"},
		{"/", "root"},
		{"/tmp/" + strings.Repeat("abc", 20), strings.Repeat("abc", 10) + "ab"},
	}
	re := regexp.MustCompile(`^rov-[a-z0-9_-]+-[0-9a-f]{8}$`)
	for _, tt := range tests {
		got := (&Project{Root: tt.root}).SocketName()
		if !re.MatchString(got) || !strings.HasPrefix(got, "rov-"+tt.base+"-") {
			t.Errorf("SocketName(%s) = %s, want rov-%s-<hash8>", tt.root, got, tt.base)
		}
	}
	a := (&Project{Root: "/a/x"}).SocketName()
	b := (&Project{Root: "/b/x"}).SocketName()
	if a == b {
		t.Errorf("same socket %s for different roots", a)
	}
	if a != (&Project{Root: "/a/x"}).SocketName() {
		t.Error("SocketName is not stable")
	}
}

func TestStarterYAMLIsValid(t *testing.T) {
	data := StarterYAML("myapp")
	cfg, err := Parse("rov.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Processes) != 0 || cfg.LogDir != DefaultLogDir || cfg.LogMax != DefaultLogMax {
		t.Errorf("starter config = %+v", cfg)
	}
	if !strings.Contains(string(data), "for myapp.") {
		t.Error("starter does not name the project")
	}
}

func TestInit(t *testing.T) {
	tests := []struct {
		name      string
		gitignore *string // nil = no file
		want      string  // .gitignore afterwards ("" = unchanged / absent)
	}{
		{"no gitignore", nil, "/.logs/\n"},
		{"already ignored", new("node_modules\n.logs/\n"), ""},
		{"anchored", new("/.logs/\n"), ""},
		{"bare", new(".logs"), ""},
		{"glob", new("/.logs/*\n"), ""},
		{"negated only", new("!.logs\n"), "!.logs\n/.logs/\n"},
		{"commented out", new("# .logs\n"), "# .logs\n/.logs/\n"},
		{"no trailing newline", new("dist"), "dist\n/.logs/\n"},
		{"other dir", new(".logs-old/\n"), ".logs-old/\n/.logs/\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			gi := filepath.Join(root, ".gitignore")
			if tt.gitignore != nil {
				writeFile(t, gi, *tt.gitignore)
			}
			created, err := Init(root)
			if err != nil {
				t.Fatal(err)
			}
			wantCreated := []string{filepath.Join(root, FileName)}
			if tt.want != "" {
				wantCreated = append(wantCreated, gi)
			}
			if strings.Join(created, ",") != strings.Join(wantCreated, ",") {
				t.Errorf("created = %v, want %v", created, wantCreated)
			}
			if tt.want != "" {
				got, _ := os.ReadFile(gi)
				if string(got) != tt.want {
					t.Errorf(".gitignore = %q, want %q", got, tt.want)
				}
			}
			if _, err := Load(filepath.Join(root, FileName)); err != nil {
				t.Errorf("written rov.yaml: %v", err)
			}
		})
	}
}

func TestInitRefusesExisting(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, FileName), "processes:\n")
	_, err := Init(root)
	if !errors.Is(err, ErrConfigExists) {
		t.Fatalf("got %v, want ErrConfigExists", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, FileName)); string(got) != "processes:\n" {
		t.Errorf("rov.yaml was changed to %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Error(".gitignore was written after a refusal")
	}
}
