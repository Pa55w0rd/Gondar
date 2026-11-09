package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	errWorkspaceBoundary  = errors.New("路径或符号链接越出工作区")
	errWorkspaceProtected = errors.New("系统数据目录不可修改")
	errWorkspaceChanged   = errors.New("路径在操作期间发生变化，请重试")
	errWorkspaceType      = errors.New("目标不是普通文件或目录")
)

// workspaceRoot pins the configured directory, including a symlink used as the
// configured root. All actual I/O uses os.Root handles; absolute names below are
// used only to translate an internal absolute symlink into root-relative parts.
// This protects the file API, not a process with root Bash/mount/hardlink access.
type workspaceRoot struct {
	root      *os.Root
	aliases   []string
	protected []os.FileInfo
}

func (s *Server) openWorkspace() (*workspaceRoot, error) {
	abs, err := filepath.Abs(s.m.dir)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	physical, err := workspacePhysicalRoot(r, abs)
	if err != nil {
		r.Close()
		return nil, err
	}
	a, err := os.Stat(physical)
	if err != nil {
		r.Close()
		return nil, err
	}
	b, err := r.Stat(".")
	if err != nil || !os.SameFile(a, b) {
		r.Close()
		return nil, errWorkspaceChanged
	}
	ws := &workspaceRoot{root: r, aliases: []string{filepath.Clean(abs), filepath.Clean(physical)}}
	for _, name := range workspaceProtectedDirs {
		if info, err := r.Lstat(name); err == nil {
			ws.protected = append(ws.protected, info)
		}
	}
	return ws, nil
}

func (s *Server) workspaceRelative(raw string) (string, error) {
	abs, ok := s.wsResolve(raw)
	if !ok {
		return "", errWorkspaceBoundary
	}
	rel, err := filepath.Rel(filepath.Clean(s.m.dir), abs)
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return "", errWorkspaceBoundary
	}
	return rel, nil
}

func workspaceNameEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func workspaceProtectedParts(parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	for _, name := range workspaceProtectedDirs {
		if workspaceNameEqual(parts[0], name) {
			return true
		}
	}
	return false
}

func workspaceIsLink(info os.FileInfo) bool {
	// Since Go 1.23 Windows junctions/name-surrogate reparse points use
	// ModeIrregular rather than ModeSymlink. Readlink below must still succeed;
	// other irregular files are rejected instead of treated as directories.
	return info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0)
}

func (ws *workspaceRoot) isProtectedInfo(info os.FileInfo) bool {
	for _, protected := range ws.protected {
		if os.SameFile(info, protected) {
			return true
		}
	}
	return false
}

// Do not Clean a link target before walking it: "link/.." must be resolved after
// that link, rather than collapsing the components lexically first.
func (ws *workspaceRoot) linkParts(target string) ([]string, bool, error) {
	if filepath.IsAbs(target) {
		target = filepath.FromSlash(target)
		for _, base := range ws.aliases {
			if workspaceNameEqual(target, base) {
				return nil, true, nil
			}
			prefix := strings.TrimRight(base, string(os.PathSeparator)) + string(os.PathSeparator)
			if len(target) >= len(prefix) && workspaceNameEqual(target[:len(prefix)], prefix) {
				return strings.Split(filepath.ToSlash(target[len(prefix):]), "/"), true, nil
			}
		}
		return nil, false, errWorkspaceBoundary
	}
	if filepath.VolumeName(target) != "" || strings.HasPrefix(target, string(os.PathSeparator)) {
		return nil, false, errWorkspaceBoundary
	}
	return strings.Split(filepath.ToSlash(target), "/"), false, nil
}

type workspaceTarget struct {
	parent *os.Root
	name   string
	info   os.FileInfo // Lstat of the resolved leaf; nil if a new leaf is allowed.
	dirs   []*os.Root  // pinned descendants; workspaceRoot owns the root itself.
}

func (t *workspaceTarget) close() {
	for i := len(t.dirs) - 1; i >= 0; i-- {
		t.dirs[i].Close()
	}
}

// resolve expands aliases explicitly, checks the physical protected prefix, and
// pins each non-link directory. Lstat/OpenRoot identity checks fail closed on a
// concurrent swap. Following the original alias again after validation would
// reintroduce both the escape and protected-directory races.
func (ws *workspaceRoot) resolve(rel string, mutate, followLeaf, makeParents bool) (_ *workspaceTarget, err error) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	dirs := []*os.Root{ws.root}
	var canonical []string
	defer func() {
		if err != nil {
			for i := len(dirs) - 1; i > 0; i-- {
				dirs[i].Close()
			}
		}
	}()
	links, steps := 0, 0
	for len(parts) > 0 {
		steps++
		if steps > 8192 {
			return nil, errWorkspaceBoundary
		}
		name := parts[0]
		parts = parts[1:]
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if len(dirs) == 1 {
				return nil, errWorkspaceBoundary
			}
			dirs[len(dirs)-1].Close()
			dirs = dirs[:len(dirs)-1]
			canonical = canonical[:len(canonical)-1]
			continue
		}
		if strings.IndexByte(name, 0) >= 0 || (runtime.GOOS == "windows" && (strings.Contains(name, ":") || strings.TrimRight(name, ". ") != name)) {
			return nil, errWorkspaceBoundary
		}
		candidate := append(append([]string(nil), canonical...), name)
		if mutate && workspaceProtectedParts(candidate) {
			return nil, errWorkspaceProtected
		}
		parent := dirs[len(dirs)-1]
		info, e := parent.Lstat(name)
		if os.IsNotExist(e) && len(parts) > 0 && makeParents {
			if e = parent.Mkdir(name, 0o755); e != nil && !os.IsExist(e) {
				return nil, e
			}
			info, e = parent.Lstat(name)
		}
		if os.IsNotExist(e) && len(parts) == 0 {
			return &workspaceTarget{parent: parent, name: name, dirs: dirs[1:]}, nil
		}
		if e != nil {
			return nil, e
		}
		// File identity also covers Windows short-name aliases of a protected
		// top-level directory; comparing only a spelling would miss those.
		if mutate && ws.isProtectedInfo(info) {
			return nil, errWorkspaceProtected
		}
		if workspaceIsLink(info) && (followLeaf || len(parts) > 0) {
			links++
			if links > 32 {
				return nil, errWorkspaceBoundary
			}
			target, e := parent.Readlink(name)
			if e != nil {
				return nil, errWorkspaceChanged
			}
			prefix, absolute, e := ws.linkParts(target)
			if e != nil {
				return nil, e
			}
			if absolute {
				for i := len(dirs) - 1; i > 0; i-- {
					dirs[i].Close()
				}
				dirs, canonical = dirs[:1], nil
			}
			parts = append(prefix, parts...)
			continue
		}
		if len(parts) == 0 {
			return &workspaceTarget{parent: parent, name: name, info: info, dirs: dirs[1:]}, nil
		}
		if !info.IsDir() {
			return nil, errWorkspaceType
		}
		next, e := parent.OpenRoot(name)
		if e != nil {
			return nil, errWorkspaceChanged
		}
		actual, e := next.Stat(".")
		if e != nil || !os.SameFile(info, actual) {
			next.Close()
			return nil, errWorkspaceChanged
		}
		dirs, canonical = append(dirs, next), candidate
	}
	parent := dirs[len(dirs)-1]
	info, err := parent.Stat(".")
	if err != nil {
		return nil, err
	}
	return &workspaceTarget{parent: parent, name: ".", info: info, dirs: dirs[1:]}, nil
}

// open checks the opened inode before reading, and before truncating/writing.
// O_EXCL protects a missing leaf from being replaced with a link before create.
func (t *workspaceTarget) open(write bool) (*os.File, error) {
	flags := os.O_RDONLY
	if write {
		flags = os.O_WRONLY
	}
	if t.info == nil {
		if !write {
			return nil, os.ErrNotExist
		}
		return t.parent.OpenFile(t.name, flags|os.O_CREATE|os.O_EXCL, 0o644)
	}
	if !t.info.Mode().IsRegular() {
		return nil, errWorkspaceType
	}
	f, err := t.parent.OpenFile(t.name, flags, 0)
	if err != nil {
		return nil, errWorkspaceChanged
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(t.info, actual) {
		f.Close()
		return nil, errWorkspaceChanged
	}
	return f, nil
}

func (t *workspaceTarget) openDirectory() (*os.Root, error) {
	if t.info == nil {
		return nil, os.ErrNotExist
	}
	if !t.info.IsDir() {
		return nil, errWorkspaceType
	}
	r, err := t.parent.OpenRoot(t.name)
	if err != nil {
		return nil, errWorkspaceChanged
	}
	actual, err := r.Stat(".")
	if err != nil || !os.SameFile(t.info, actual) {
		r.Close()
		return nil, errWorkspaceChanged
	}
	return r, nil
}

func (t *workspaceTarget) write(src io.Reader) error {
	f, err := t.open(true)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		return err
	}
	return f.Close()
}

func workspaceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errWorkspaceBoundary), errors.Is(err, errWorkspaceProtected), errors.Is(err, errWorkspaceChanged):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, os.ErrNotExist):
		writeErr(w, http.StatusNotFound, "路径不存在")
	case errors.Is(err, errWorkspaceType):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "工作区文件操作失败")
	}
}
