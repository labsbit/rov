package shell

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":             "''",
		"node":         "node",
		"scripts/x.js": "scripts/x.js",
		"--port=8080":  "--port=8080",
		"a b":          "'a b'",
		"it's":         `'it'\''s'`,
		"$HOME":        "'$HOME'",
		"*.go":         "'*.go'",
		"~/x":          "'~/x'",
		"a\nb":         "'a\nb'",
		"=ls":          "'=ls'",
		"=":            "'='",
		"a=b":          "a=b",
		"!x":           "'!x'",
		"^x":           "'^x'",
		"x~":           "'x~'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJoinAndCommand(t *testing.T) {
	if got := Join([]string{"FOO=1", "x"}); got != "'FOO=1' x" {
		t.Errorf("assignment-looking first word: %q", got)
	}
	if got := Join([]string{"env", "FOO=1", "x"}); got != "env FOO=1 x" {
		t.Errorf("later '=' word: %q", got)
	}
	if got := Command([]string{"npm run dev | tee x"}); got != "npm run dev | tee x" {
		t.Errorf("single arg kept verbatim: %q", got)
	}
	if got := Command([]string{"node", "my script.js"}); got != "node 'my script.js'" {
		t.Errorf("args quoted: %q", got)
	}
}

// TestRoundTrip runs the joined command through real shells and checks that
// every argument arrives unchanged.
func TestRoundTrip(t *testing.T) {
	args := []string{"printf", `%s\n`, "plain", "two words", "it's", `"dq"`, "$HOME", "`id`", "a\\b", "*", "~", "#x", "a;b", "", "tab\there", "new\nline", "é日本", "!bang", "=ls", "=", "a=b", "--x==y", "^caret", "x^y", "~root", "a~b", "a=~/x", "%1", "{a,b}", "a:~/b"}
	want := strings.Join(args[2:], "\n") + "\n"
	for _, sh := range []string{"sh", "bash", "zsh", "dash"} {
		path, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		out, err := exec.Command(path, "-c", Join(args)).Output()
		if err != nil {
			t.Errorf("%s: %v", sh, err)
			continue
		}
		if string(out) != want {
			t.Errorf("%s: got %q, want %q", sh, out, want)
		}
	}
}
