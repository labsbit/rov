package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/skills"
)

// Agents rov can install its skill for, in display order.
var skillAgents = []string{"claude", "cursor", "codex"}

// skillDir is where one agent reads skills from, relative to the project
// root or the home directory:
//
//   - Claude Code reads only .claude/skills.
//   - Codex reads only .agents/skills (repo, and ~/.agents/skills).
//   - Cursor reads .agents/skills, .cursor/skills, and .claude/skills for
//     compatibility — so it needs no directory of its own when Claude's or
//     Codex's is written, and gets .agents/skills (the cross-agent one)
//     otherwise.
//
// skillTargets returns the directories for a set of agents, without
// duplicates, so one agent never sees two copies it could have avoided.
func skillTargets(agents []string) []string {
	has := func(a string) bool { return slices.Contains(agents, a) }
	var dirs []string
	if has("claude") {
		dirs = append(dirs, filepath.Join(".claude", "skills", "rov"))
	}
	if has("codex") || (has("cursor") && !has("claude")) {
		dirs = append(dirs, filepath.Join(".agents", "skills", "rov"))
	}
	return dirs
}

// detectAgents returns the agents that look installed on this machine: the
// CLI on PATH or its home directory present.
func detectAgents(home string) []string {
	var found []string
	probes := map[string][]string{
		"claude": {"claude"},
		"cursor": {"cursor", "cursor-agent"},
		"codex":  {"codex"},
	}
	for _, a := range skillAgents {
		ok := false
		for _, bin := range probes[a] {
			if _, err := exec.LookPath(bin); err == nil {
				ok = true
			}
		}
		if fi, err := os.Stat(filepath.Join(home, "."+a)); err == nil && fi.IsDir() {
			ok = true
		}
		if ok {
			found = append(found, a)
		}
	}
	return found
}

var (
	fGlobal = flagDef{names: []string{"--global", "-g"}}
	fAgent  = flagDef{names: []string{"--agent"}, value: true}
	fForce  = flagDef{names: []string{"--force"}}
)

// cmdSkill: rov skill [install|uninstall|path] — the agent skill.
func (a *App) cmdSkill(argv []string) error {
	sub := ""
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		sub, argv = argv[0], argv[1:]
	}
	switch sub {
	case "", "show":
		ar, err := parseArgs(argv)
		if err != nil {
			return err
		}
		if err := ar.exactly(0, ""); err != nil {
			return err
		}
		_, err = a.Stdout.Write(skills.Rov)
		return err
	case "install", "uninstall":
	default:
		return usagef("unknown skill command %q (show, install, uninstall)", sub)
	}

	ar, err := parseArgs(argv, fGlobal, fAgent, fForce)
	if err != nil {
		return err
	}
	if err := ar.exactly(0, ""); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	agents, err := parseAgents(ar.all("--agent"))
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		if agents = detectAgents(home); len(agents) == 0 {
			agents = skillAgents
		}
	}
	base, where := home, "~"
	if !ar.has("--global") {
		if base, err = a.projectDir(); err != nil {
			return err
		}
		where = base
	}
	fmt.Fprintf(a.Stderr, "rov: agents: %s\n", strings.Join(agents, ", "))
	for _, rel := range skillTargets(agents) {
		dir := filepath.Join(base, rel)
		shown := filepath.Join(where, rel)
		if sub == "uninstall" {
			if err := removeSkill(dir, ar.has("--force")); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "removed %s\n", shown)
			continue
		}
		verb, err := writeSkill(dir, ar.has("--force"))
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "%s %s\n", verb, filepath.Join(shown, "SKILL.md"))
	}
	return nil
}

func parseAgents(vals []string) ([]string, error) {
	var out []string
	for _, v := range vals {
		for _, a := range strings.Split(v, ",") {
			a = strings.TrimSpace(strings.ToLower(a))
			switch {
			case a == "":
			case a == "all":
				out = append(out, skillAgents...)
			case slices.Contains(skillAgents, a):
				out = append(out, a)
			default:
				return nil, usagef("unknown agent %q (claude, cursor, codex, all)", a)
			}
		}
	}
	// Canonical order, no duplicates.
	var agents []string
	for _, a := range skillAgents {
		if slices.Contains(out, a) {
			agents = append(agents, a)
		}
	}
	return agents, nil
}

// projectDir is -C, else the project root of the working directory (its
// rov.yaml or git root), else the working directory.
func (a *App) projectDir() (string, error) {
	if a.dir != "" {
		return filepath.Abs(a.dir)
	}
	wd, err := a.Getwd()
	if err != nil {
		return "", err
	}
	if root, _, err := config.FindRoot(wd); err == nil {
		return root, nil
	}
	return wd, nil
}

// ours reports whether a SKILL.md is rov's (any version).
func ours(data []byte) bool {
	return bytes.HasPrefix(data, []byte("---\nname: rov\n"))
}

func writeSkill(dir string, force bool) (string, error) {
	path := filepath.Join(dir, "SKILL.md")
	verb := "wrote"
	if old, err := os.ReadFile(path); err == nil {
		switch {
		case bytes.Equal(old, skills.Rov):
			return "up to date", nil
		case !ours(old) && !force:
			return "", fmt.Errorf("%s exists and isn't rov's skill; use --force to replace it", path)
		}
		verb = "updated"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, skills.Rov, 0o644); err != nil {
		return "", err
	}
	return verb, os.Rename(tmp, path)
}

func removeSkill(dir string, force bool) error {
	path := filepath.Join(dir, "SKILL.md")
	old, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ours(old) && !force {
		return fmt.Errorf("%s isn't rov's skill; use --force to remove it", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	_ = os.Remove(dir) // only if now empty
	return nil
}
