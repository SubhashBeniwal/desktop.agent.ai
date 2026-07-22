// Package files provides sandboxed filesystem access: every path is confined
// to one of the configured workspace roots.
package files

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sandbox confines file operations to a set of root directories.
type Sandbox struct {
	roots []string // absolute, cleaned
}

// NewSandbox creates a sandbox over the given roots (already absolute).
func NewSandbox(roots []string) (*Sandbox, error) {
	cleaned := make([]string, 0, len(roots))
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("workspace %q: %w", r, err)
		}
		cleaned = append(cleaned, filepath.Clean(abs))
	}
	return &Sandbox{roots: cleaned}, nil
}

// Roots returns a copy of the configured roots.
func (s *Sandbox) Roots() []string {
	return append([]string{}, s.roots...)
}

// Resolve cleans path to an absolute path and verifies it lies within a
// configured root. It does not require the path to exist.
func (s *Sandbox) Resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	for _, root := range s.roots {
		if abs == root || strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed workspaces", path)
}

// ResolveDir resolves path and requires it to be an existing directory.
func (s *Sandbox) ResolveDir(path string) (string, error) {
	abs, err := s.Resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return abs, nil
}

// Read returns the contents of a file within the sandbox.
func (s *Sandbox) Read(path string) ([]byte, error) {
	abs, err := s.Resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// Write writes data to a file within the sandbox, creating parent directories.
func (s *Sandbox) Write(path string, data []byte, perm os.FileMode) error {
	abs, err := s.Resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if perm == 0 {
		perm = 0o644
	}
	return os.WriteFile(abs, data, perm)
}

// Entry describes a directory entry returned by List.
type Entry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
}

// List returns the entries of a directory within the sandbox.
func (s *Sandbox) List(path string) ([]Entry, error) {
	abs, err := s.ResolveDir(path)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Name:    de.Name(),
			IsDir:   de.IsDir(),
			Size:    info.Size(),
			Mode:    info.Mode().String(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}
