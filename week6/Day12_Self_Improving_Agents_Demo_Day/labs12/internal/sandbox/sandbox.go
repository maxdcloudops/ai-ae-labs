// Package sandbox is brake #1 of the autoresearch pattern: a bounded change
// surface. The agent may write exactly two declared things — the strategy file
// and the experiments directory — and everything else is read-only.
//
// It is a type rather than a strings.HasPrefix check at the write site because
// a prefix check catches the naive case and misses the three real ones:
// `..`-traversal, an absolute path, and a symlink pointing outside. A guarantee
// that is easy to walk around is not a guarantee, it is a comment.
//
// No ADK import: the change surface is testable without a model, without a key
// and without an agent.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideSurface reports that a path lies outside the declared surface. It
// is a sentinel so callers and tests branch on errors.Is and the wording stays
// free to change.
var ErrOutsideSurface = errors.New("sandbox: шлях поза дозволеною поверхнею змін")

// Default names of the two mutable entries.
const (
	// StrategyFile is the one file the agent rewrites.
	StrategyFile = "strategy.md"
	// ExperimentsDir holds discarded candidates: write-only scratch space.
	ExperimentsDir = "experiments"
)

// Sandbox is the agent's writable surface inside one working directory.
//
// root is always symlink-resolved: on macOS t.TempDir() hands back
// /var/folders/..., which really is /private/var/folders/..., and without
// normalising it every path comparison would answer a false "outside".
type Sandbox struct {
	root string
	// file is the single mutable file, relative to root.
	file string
	// dir is the single mutable directory subtree, relative to root.
	dir string
}

// New declares the surface: a working directory, one mutable file and one
// mutable directory inside it.
//
// Both names must be relative and free of `..`, otherwise the *declaration*
// itself would already put the agent outside the working directory and every
// later check would be theatre.
func New(root, file, dir string) (*Sandbox, error) {
	if root == "" {
		return nil, errors.New("sandbox: порожній root; робоча директорія обовʼязкова")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: абсолютний шлях root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("sandbox: root %q недоступний: %w", root, err)
	}
	if fi, err := os.Stat(resolved); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("sandbox: root %q не є каталогом", root)
	}
	cleanFile, err := relInside(file)
	if err != nil {
		return nil, err
	}
	cleanDir, err := relInside(dir)
	if err != nil {
		return nil, err
	}
	if cleanFile == cleanDir {
		return nil, fmt.Errorf("sandbox: файл і каталог не можуть бути тим самим шляхом %q", cleanFile)
	}
	return &Sandbox{root: resolved, file: cleanFile, dir: cleanDir}, nil
}

// relInside validates one declared entry.
func relInside(p string) (string, error) {
	clean := filepath.Clean(p)
	if p == "" || filepath.IsAbs(clean) || clean == "." || hasDotDot(clean) {
		return "", fmt.Errorf("sandbox: %q не є відносним шляхом усередині root", p)
	}
	return clean, nil
}

// hasDotDot reports whether a cleaned path escapes upwards.
//
// filepath.Clean collapses every interior `..` ("a/../b" → "b"), so after Clean
// a `..` segment can only remain at the front — and that is exactly the escape.
func hasDotDot(clean string) bool {
	return clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// Root returns the symlink-resolved working directory.
func (s *Sandbox) Root() string { return s.root }

// StrategyPath is the absolute path of the mutable file.
func (s *Sandbox) StrategyPath() string { return filepath.Join(s.root, s.file) }

// ExperimentsPath is the absolute path of the mutable directory.
func (s *Sandbox) ExperimentsPath() string { return filepath.Join(s.root, s.dir) }

// StrategyRel is the declared relative path of the mutable file.
func (s *Sandbox) StrategyRel() string { return s.file }

// IsMutable answers the question the reviewer's checklist asks: may the agent
// write here?
//
// It is the boolean form of Resolve and deliberately swallows the reason — use
// Resolve when the caller has to tell the agent WHY it was refused.
func (s *Sandbox) IsMutable(p string) bool {
	_, err := s.Resolve(p)
	return err == nil
}

// Resolve checks a path the agent asks to write to and returns its absolute
// form, or ErrOutsideSurface.
//
// The order of checks is deliberate: normalise first (Clean removes "a/../b"),
// then expand symlinks of the *directory* (otherwise `link/strategy.md`, where
// link → /tmp/evil, would pass as "the same base name"), and only then compare
// against the two allowed locations. The entry itself is checked last: a
// symlink sitting where strategy.md should be turns an allowed write into a
// write to anything.
func (s *Sandbox) Resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: порожній шлях", ErrOutsideSurface)
	}
	cand := p
	if !filepath.IsAbs(cand) {
		cand = filepath.Join(s.root, cand)
	}
	cand = filepath.Clean(cand)

	if dir, err := filepath.EvalSymlinks(filepath.Dir(cand)); err == nil {
		cand = filepath.Join(dir, filepath.Base(cand))
	}

	switch {
	case cand == s.StrategyPath():
		// allowed: the one mutable file
	case strings.HasPrefix(cand, s.ExperimentsPath()+string(filepath.Separator)):
		// allowed: inside the one mutable directory. The trailing separator is
		// what stops "experiments-evil" from matching "experiments".
	default:
		return "", fmt.Errorf("%w: %q; дозволено лише %q і %q/",
			ErrOutsideSurface, p, s.file, s.dir)
	}

	if fi, err := os.Lstat(cand); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %q — символьне посилання, а не звичайний файл", ErrOutsideSurface, p)
	}
	return cand, nil
}

// ReadStrategy reads the current content of the mutable file.
func (s *Sandbox) ReadStrategy() ([]byte, error) {
	path, err := s.Resolve(s.file)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// WriteStrategy replaces the mutable file. Applying a candidate and reverting
// it are the same operation.
func (s *Sandbox) WriteStrategy(content []byte) error { return s.Write(s.file, content) }

// Write writes content at p after putting it through Resolve. This is the only
// write path the engine and the agent's tool use.
//
// A production caveat worth saying out loud: between Resolve and the write
// there is a TOCTOU window — a process running in parallel can swap the file
// for a symlink in exactly that moment. In Go, without openat/O_NOFOLLOW, that
// gap cannot be closed portably, which is why real isolation lives one level
// down (gVisor, a Firecracker microVM) and not in a path check inside the
// process. That is the first of the three missing production layers named in
// the README.
func (s *Sandbox) Write(p string, content []byte) error {
	path, err := s.Resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("sandbox: каталог для %q: %w", p, err)
	}
	return atomicWrite(path, content, 0o644)
}

// atomicWrite writes to a temporary file beside path, fsyncs it, and only then
// renames it into place.
//
// Why: the engine writes a candidate to disk BEFORE scoring it and may revert
// it immediately after (write → score → write back). A bare os.WriteFile
// leaves a window where a crash mid-write leaves a half-written file on disk —
// and no record of it in experiments.log, because the entry is appended after.
// os.Rename on the same volume is atomic: afterwards the path holds either the
// old content or the new one, never half.
func atomicWrite(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("sandbox: тимчасовий файл: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful Rename

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("sandbox: запис у тимчасовий файл: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sandbox: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sandbox: закриття тимчасового файлу: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("sandbox: права доступу: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("sandbox: атомарне перейменування: %w", err)
	}
	return nil
}
