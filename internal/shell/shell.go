// Package shell quotes arguments for a POSIX shell (sh, bash, zsh, dash).
//
// rov stores a spawned command as one string and reruns it with `sh -c`
// semantics on a restart, so the arguments an agent passes after `--` must
// survive that round trip exactly.
package shell

import "strings"

// Quote returns s as one shell word: unchanged when it holds only characters
// no shell treats specially, else in single quotes (a single quote inside
// becomes '\”).
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if safe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// safe reports whether s needs no quoting: it holds only characters from a
// conservative set no shell expands. '=' is safe except at the start of a
// word (zsh expands "=cmd" to the path of cmd) and in the first word
// position, where `A=b cmd` would be an assignment; Join handles that.
// Everything else — '~', '!', '^', '#', globs, quotes, '$', spaces — is
// quoted.
func safe(s string) bool {
	if strings.HasPrefix(s, "=") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_./:,+@%=", r):
		default:
			return false
		}
	}
	return true
}

// Join quotes each argument and joins them with spaces. A first word that
// contains '=' is quoted, so it runs as a command rather than being read as
// an environment assignment.
func Join(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		q := Quote(a)
		if i == 0 && q == a && strings.Contains(a, "=") {
			q = "'" + a + "'"
		}
		out[i] = q
	}
	return strings.Join(out, " ")
}

// Command turns the arguments after `--` into the command string rov
// stores: a single argument is taken as a shell command as it is (so
// `rov spawn x -- 'npm run dev | tee out'` works), several are quoted and
// joined (so `rov spawn x -- node "my script.js"` runs exactly that).
func Command(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return Join(args)
}
