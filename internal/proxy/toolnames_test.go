package proxy

import (
	"strings"
	"testing"
)

func TestResolveDeclaredToolName(t *testing.T) {
	declared := []string{"Bash", "Read", "Edit"}
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// exact and case-insensitive matches keep working
		{"Bash", "Bash", true},
		{"bash", "Bash", true},
		{"READ", "Read", true},
		// OpenCode namespacing applied by Zen: last dot segment resolves
		{"default.Bash", "Bash", true},
		{"default.bash", "Bash", true},
		{"provider.default.Read", "Read", true},
		// free-tier marker tools resolve to their declared equivalents
		{"shell", "Bash", true},
		{"SHELL", "Bash", true},
		{"read", "Read", true},
		{"default.shell", "Bash", true},
		{"default.read", "Read", true},
		// genuinely undeclared names still resolve to nothing
		{"evil_tool", "", false},
		{"default.evil_tool", "", false},
		{"", "", false},
		{"default.", "", false},
	}
	for _, c := range cases {
		got, ok := ResolveDeclaredToolName(c.in, declared)
		if ok != c.ok || got != c.want {
			t.Errorf("ResolveDeclaredToolName(%q) = (%q, %v), want (%q, %v)",
				c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestResolveDeclaredToolNameNamespacedDeclared(t *testing.T) {
	// The sidecar counts a namespaced declaration's short name as runnable and
	// therefore keeps the canonical tool. Resolution must agree, or the call is
	// dropped after the sidecar deliberately advertised the tool.
	cases := []struct {
		in       string
		declared []string
		want     string
	}{
		{"read", []string{"mcp.read"}, "mcp.read"},
		{"read", []string{"mcp.Read"}, "mcp.Read"},
		{"shell", []string{"mcp.Bash"}, "mcp.Bash"},
		{"shell", []string{"mcp.exec_command"}, "mcp.exec_command"},
		{"default.read", []string{"mcp.read"}, "mcp.read"},
	}
	for _, c := range cases {
		got, ok := ResolveDeclaredToolName(c.in, c.declared)
		if !ok || got != c.want {
			t.Errorf("ResolveDeclaredToolName(%q, %v) = (%q, %v), want (%q, true)",
				c.in, c.declared, got, ok, c.want)
		}
	}
	if _, ok := ResolveDeclaredToolName("read", []string{"mcp.bash"}); ok {
		t.Errorf("read must not resolve to an unrelated namespaced tool")
	}
}

func TestResolveDeclaredToolNameEmptyDeclared(t *testing.T) {
	if _, ok := ResolveDeclaredToolName("Bash", nil); ok {
		t.Errorf("nil declared list must resolve nothing")
	}
	if _, ok := ResolveDeclaredToolName("Bash", []string{}); ok {
		t.Errorf("empty declared list must resolve nothing")
	}
}

func TestResolveDeclaredToolNameMarkerWithoutEquivalent(t *testing.T) {
	// A marker with no declared equivalent (e.g. no Read-like tool) still
	// resolves to nothing rather than inventing a tool.
	if _, ok := ResolveDeclaredToolName("read", []string{"Bash"}); ok {
		t.Errorf("read with no Read-like declared tool must resolve nothing")
	}
}

func TestFileCommandReadHonoursOffsetAndLimit(t *testing.T) {
	cmd, ok := fileCommand("read", `{"path":"/tmp/a.txt","offset":10,"limit":5}`)
	if !ok {
		t.Fatal("read fallback not produced")
	}
	for _, want := range []string{"NR>=10", "NR<15", "/tmp/a.txt"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("read command %q missing %q", cmd, want)
		}
	}
}

func TestFileCommandReadDefaultsToBoundedWindow(t *testing.T) {
	cmd, ok := fileCommand("read", `{"path":"/tmp/a.txt"}`)
	if !ok {
		t.Fatal("read fallback not produced")
	}
	if !strings.Contains(cmd, "NR<2001") {
		t.Fatalf("read command %q is not bounded to the canonical 2000-line cap", cmd)
	}
}

func TestFileCommandEditRejectsAmbiguousMatch(t *testing.T) {
	cmd, ok := fileCommand("edit", `{"path":"a.txt","oldString":"x","newString":"y"}`)
	if !ok {
		t.Fatal("edit fallback not produced")
	}
	if !strings.Contains(cmd, "old_string must match exactly once") {
		t.Fatalf("edit command %q silently replaces the first of several matches", cmd)
	}
}

// shell_command and run_command are real client shell names. If Go does not
// recognise them, the sidecar keeps read/write/edit alive as shell fallbacks
// but every fallback resolves to no tool, which is the dropped-call incident
// again with a different client.
func TestShellFallbackAcceptsShellCommandAndRunCommand(t *testing.T) {
	for _, name := range []string{"shell_command", "run_command"} {
		gotName, gotArgs, ok := ResolveDeclaredCall("read", `{"path":"/tmp/a.txt"}`, []string{name})
		if !ok || gotName != name {
			t.Fatalf("%s fallback = %q (ok=%v), want %q", name, gotName, ok, name)
		}
		if !strings.Contains(gotArgs, "/tmp/a.txt") {
			t.Fatalf("%s fallback args = %s, want the requested path", name, gotArgs)
		}
		if name == "shell_command" && !strings.Contains(gotArgs, `"cmd"`) {
			t.Fatalf("shell_command fallback must use the Codex cmd key: %s", gotArgs)
		}
		if name == "run_command" && !strings.Contains(gotArgs, `"command"`) {
			t.Fatalf("run_command fallback must use the command key: %s", gotArgs)
		}
	}
}

// AppendToolNameFragment rebuilds split streamed names without turning a
// repeated complete name into `readread` or `shellshell`.
func TestAppendToolNameFragment(t *testing.T) {
	cases := []struct {
		current, fragment string
		declared          []string
		want              string
	}{
		{"", "re", []string{"Bash"}, "re"},
		{"re", "ad", []string{"Bash"}, "read"},
		{"she", "ll", []string{"Bash"}, "shell"},
		{"sh", "ell", []string{"Bash"}, "shell"},
		{"shell", "l", []string{"Bash"}, "shell"},
		{"shell", "shell", []string{"Bash"}, "shell"},
		{"read", "read", []string{"Read"}, "read"},
		{"re", "read", []string{"Read"}, "read"},
	}
	for _, tc := range cases {
		if got := AppendToolNameFragment(tc.current, tc.fragment, tc.declared); got != tc.want {
			t.Errorf("AppendToolNameFragment(%q, %q, %v) = %q, want %q",
				tc.current, tc.fragment, tc.declared, got, tc.want)
		}
	}
}
