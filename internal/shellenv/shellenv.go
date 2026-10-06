// Package shellenv makes a GUI-launched process see the user's shell PATH.
//
// Apps started from Finder/Dock (or a login item) inherit launchd's minimal
// PATH (/usr/bin:/bin:/usr/sbin:/sbin), so CLIs installed via Homebrew, npm,
// etc. (claude, codex, gh, ...) would not be found. Windows GUI apps already
// get the user's full PATH, so this is a no-op there.
package shellenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const marker = "__AIO_PATH__"

// ImportPATH merges the login shell's PATH into this process's PATH. Entries
// already present keep their position; new ones are appended. It is best
// effort: on any failure the PATH is left as is, plus common install dirs.
func ImportPATH() {
	if runtime.GOOS == "windows" {
		return
	}
	merge(loginShellPATH())
	home, _ := os.UserHomeDir()
	merge(strings.Join([]string{
		"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin",
		filepath.Join(home, ".local", "bin"), filepath.Join(home, "go", "bin"),
	}, string(os.PathListSeparator)))
}

func loginShellPATH() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// -i -l so rc files that set PATH (.zshrc, .bash_profile, ...) run. The
	// marker separates PATH from anything the rc files print.
	out, err := exec.CommandContext(ctx, shell, "-ilc", `printf '`+marker+`%s`+marker+`' "$PATH"`).Output()
	if err != nil {
		return ""
	}
	parts := strings.Split(string(out), marker)
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

func merge(extra string) {
	if extra == "" {
		return
	}
	cur := filepath.SplitList(os.Getenv("PATH"))
	seen := make(map[string]bool, len(cur))
	for _, p := range cur {
		seen[p] = true
	}
	for _, p := range filepath.SplitList(extra) {
		if p != "" && !seen[p] {
			cur = append(cur, p)
			seen[p] = true
		}
	}
	os.Setenv("PATH", strings.Join(cur, string(os.PathListSeparator)))
}
