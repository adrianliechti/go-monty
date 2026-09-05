package monty

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"testing/fstest"
	"time"
	"unicode/utf8"
)

// Optional interfaces an fs.FS may implement to let sandbox code write.
// fs.FS itself is read-only; without these, writes raise PermissionError.
type (
	// WriteFileFS creates or truncates a file. Implement AppendFileFS as well
	// to make append modes efficient; otherwise appends read then rewrite.
	WriteFileFS interface {
		WriteFile(name string, data []byte, perm fs.FileMode) error
	}
	// AppendFileFS appends to a file, creating it if missing.
	AppendFileFS interface {
		AppendFile(name string, data []byte, perm fs.FileMode) error
	}
	// MkdirFS creates one directory; the parent must exist.
	MkdirFS interface {
		Mkdir(name string, perm fs.FileMode) error
	}
	// RemoveFS removes a file or an empty directory.
	RemoveFS interface {
		Remove(name string) error
	}
	// RenameFS renames a file or directory.
	RenameFS interface {
		Rename(oldname, newname string) error
	}
)

// FS serves the sandbox's pathlib and open() calls under Mount from an fs.FS.
// Calls for paths outside Mount are declined with ErrNotHandled, so several
// FS values can be combined with Handlers. Directory listings and stat use
// fs.ReadDirFS / fs.StatFS when available and fall back otherwise.
type FS struct {
	// Mount is the sandbox directory the filesystem appears at, e.g. "/data".
	// "/" mounts it at the root.
	Mount string
	// FS is the backing filesystem. Names passed to it are slash-separated and
	// relative to Mount, with "." for Mount itself.
	FS fs.FS
	// Quota, when set, caps the bytes written through this mount.
	Quota *WriteQuota
}

// NewFS mounts fsys at the sandbox path mount.
func NewFS(mount string, fsys fs.FS) *FS {
	return &FS{Mount: mount, FS: fsys}
}

// DirFS returns a writable filesystem rooted at dir on the host, backed by
// os.Root so sandbox paths cannot escape it via symlinks or "..".
func DirFS(dir string) (*DirRoot, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &DirRoot{root: root}, nil
}

// DirRoot is a writable fs.FS over a host directory. See DirFS.
type DirRoot struct {
	root *os.Root
}

func (d *DirRoot) Open(name string) (fs.File, error)          { return d.root.Open(name) }
func (d *DirRoot) Stat(name string) (fs.FileInfo, error)      { return d.root.Stat(name) }
func (d *DirRoot) ReadFile(name string) ([]byte, error)       { return d.root.ReadFile(name) }
func (d *DirRoot) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(d.root.FS(), name) }
func (d *DirRoot) Mkdir(name string, perm fs.FileMode) error  { return d.root.Mkdir(name, perm) }
func (d *DirRoot) Remove(name string) error                   { return d.root.Remove(name) }
func (d *DirRoot) Rename(oldname, newname string) error       { return d.root.Rename(oldname, newname) }
func (d *DirRoot) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return d.root.WriteFile(name, data, perm)
}
func (d *DirRoot) AppendFile(name string, data []byte, perm fs.FileMode) error {
	f, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// Close releases the host directory.
func (d *DirRoot) Close() error { return d.root.Close() }

// MemFS is an in-memory writable filesystem, handy as scratch space for a
// sandbox or in tests. The zero value is empty and ready to use.
type MemFS struct {
	m fstest.MapFS
}

// NewMemFS creates a MemFS pre-populated with files (paths without a leading
// slash, contents as string or []byte).
func NewMemFS(files map[string]any) *MemFS {
	m := &MemFS{}
	for name, content := range files {
		m.mkdirAll(path.Dir(name))
		switch c := content.(type) {
		case string:
			_ = m.WriteFile(name, []byte(c), 0o644)
		case []byte:
			_ = m.WriteFile(name, c, 0o644)
		}
	}
	return m
}

// mkdirAll creates dir and its parents, ignoring existing ones.
func (m *MemFS) mkdirAll(dir string) {
	if dir == "." || dir == "" {
		return
	}
	m.mkdirAll(path.Dir(dir))
	_ = m.Mkdir(dir, 0o755)
}

func (m *MemFS) init() {
	if m.m == nil {
		m.m = fstest.MapFS{}
	}
}

func (m *MemFS) Open(name string) (fs.File, error)          { m.init(); return m.m.Open(name) }
func (m *MemFS) Stat(name string) (fs.FileInfo, error)      { m.init(); return m.m.Stat(name) }
func (m *MemFS) ReadFile(name string) ([]byte, error)       { m.init(); return m.m.ReadFile(name) }
func (m *MemFS) ReadDir(name string) ([]fs.DirEntry, error) { m.init(); return m.m.ReadDir(name) }

func (m *MemFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	m.init()
	if !fs.ValidPath(name) || name == "." {
		return &fs.PathError{Op: "write", Path: name, Err: fs.ErrInvalid}
	}
	if existing, ok := m.m[name]; ok && existing.Mode.IsDir() {
		return &fs.PathError{Op: "write", Path: name, Err: errIsDir}
	}
	if dir := path.Dir(name); dir != "." {
		info, err := m.m.Stat(dir)
		if err != nil {
			return &fs.PathError{Op: "write", Path: name, Err: fs.ErrNotExist}
		}
		if !info.IsDir() {
			return &fs.PathError{Op: "write", Path: name, Err: errNotDir}
		}
	}
	m.m[name] = &fstest.MapFile{Data: append([]byte(nil), data...), Mode: perm, ModTime: time.Now()}
	return nil
}

func (m *MemFS) AppendFile(name string, data []byte, perm fs.FileMode) error {
	m.init()
	if f, ok := m.m[name]; ok && !f.Mode.IsDir() {
		f.Data = append(f.Data, data...)
		f.ModTime = time.Now()
		return nil
	}
	return m.WriteFile(name, data, perm)
}

func (m *MemFS) Mkdir(name string, perm fs.FileMode) error {
	m.init()
	if !fs.ValidPath(name) || name == "." {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrInvalid}
	}
	if _, err := m.m.Stat(name); err == nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	if dir := path.Dir(name); dir != "." {
		info, err := m.m.Stat(dir)
		if err != nil {
			return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrNotExist}
		}
		if !info.IsDir() {
			return &fs.PathError{Op: "mkdir", Path: name, Err: errNotDir}
		}
	}
	m.m[name] = &fstest.MapFile{Mode: fs.ModeDir | perm, ModTime: time.Now()}
	return nil
}

func (m *MemFS) Remove(name string) error {
	m.init()
	info, err := m.m.Stat(name)
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, _ := m.m.ReadDir(name)
		if len(entries) > 0 {
			return &fs.PathError{Op: "remove", Path: name, Err: errNotEmpty}
		}
	}
	delete(m.m, name)
	return nil
}

func (m *MemFS) Rename(oldname, newname string) error {
	m.init()
	if _, err := m.m.Stat(oldname); err != nil {
		return err
	}
	if !fs.ValidPath(newname) || newname == "." {
		return &fs.PathError{Op: "rename", Path: newname, Err: fs.ErrInvalid}
	}
	prefix := oldname + "/"
	for name, f := range m.m {
		switch {
		case name == oldname:
			delete(m.m, name)
			m.m[newname] = f
		case strings.HasPrefix(name, prefix):
			delete(m.m, name)
			m.m[newname+"/"+strings.TrimPrefix(name, prefix)] = f
		}
	}
	return nil
}

var (
	errIsDir    = errors.New("is a directory")
	errNotDir   = errors.New("not a directory")
	errNotEmpty = errors.New("directory not empty")
)

// ---------------------------------------------------------------------------
// OSHandler implementation
// ---------------------------------------------------------------------------

// relative maps a sandbox path to a name inside the mount, or false when the
// path lies outside it.
func (f *FS) relative(vpath string) (string, bool) {
	mount := path.Clean("/" + strings.Trim(f.Mount, "/"))
	clean := path.Clean("/" + vpath)
	if mount == "/" {
		if clean == "/" {
			return ".", true
		}
		return clean[1:], true
	}
	if clean == mount {
		return ".", true
	}
	if strings.HasPrefix(clean, mount+"/") {
		return clean[len(mount)+1:], true
	}
	return "", false
}

// virtual is the sandbox path for a name inside the mount.
func (f *FS) virtual(name string) string {
	mount := path.Clean("/" + strings.Trim(f.Mount, "/"))
	if name == "." {
		return mount
	}
	return path.Join(mount, name)
}

// HandleOSCall implements OSHandler.
func (f *FS) HandleOSCall(ctx context.Context, call *OSCall) (any, error) {
	switch call.Name {
	case "getenv", "get_environ", "date_today", "datetime_now":
		return nil, ErrNotHandled
	}
	name, ok := f.relative(call.Path)
	if !ok {
		return nil, ErrNotHandled
	}
	v, err := f.handle(call, name)
	if err != nil {
		return nil, fsError(err, call)
	}
	return v, nil
}

func (f *FS) handle(call *OSCall, name string) (any, error) {
	switch call.Name {
	case "exists":
		_, err := fs.Stat(f.FS, name)
		return err == nil, nil
	case "is_file":
		info, err := fs.Stat(f.FS, name)
		return err == nil && info.Mode().IsRegular(), nil
	case "is_dir":
		info, err := fs.Stat(f.FS, name)
		return err == nil && info.IsDir(), nil
	case "is_symlink":
		info, err := fs.Stat(f.FS, name)
		return err == nil && info.Mode()&fs.ModeSymlink != 0, nil
	case "read_text":
		data, err := f.readFile(name)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) {
			return nil, Raise("UnicodeDecodeError", "'utf-8' codec can't decode file %s", call.Path)
		}
		return string(data), nil
	case "read_bytes":
		return f.readFile(name)
	case "stat":
		info, err := fs.Stat(f.FS, name)
		if err != nil {
			return nil, err
		}
		return StatResult(info.Size(), info.ModTime(), info.IsDir()), nil
	case "iterdir":
		entries, err := fs.ReadDir(f.FS, name)
		if err != nil {
			return nil, err
		}
		paths := make([]Path, 0, len(entries))
		for _, e := range entries {
			paths = append(paths, Path(f.virtual(path.Join(name, e.Name()))))
		}
		sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })
		return paths, nil
	case "resolve", "absolute":
		return Path(f.virtual(name)), nil
	case "write_text":
		return utf8.RuneCountInString(call.Text), f.writeFile(name, []byte(call.Text), false)
	case "append_text":
		return utf8.RuneCountInString(call.Text), f.writeFile(name, []byte(call.Text), true)
	case "write_bytes":
		return len(call.Bytes), f.writeFile(name, call.Bytes, false)
	case "append_bytes":
		return len(call.Bytes), f.writeFile(name, call.Bytes, true)
	case "open":
		return f.open(call, name)
	case "mkdir":
		return nil, f.mkdir(call, name)
	case "unlink", "rmdir":
		info, err := fs.Stat(f.FS, name)
		if err != nil {
			return nil, err
		}
		if call.Name == "unlink" && info.IsDir() {
			return nil, &fs.PathError{Op: "unlink", Path: name, Err: errIsDir}
		}
		if call.Name == "rmdir" && !info.IsDir() {
			return nil, &fs.PathError{Op: "rmdir", Path: name, Err: errNotDir}
		}
		r, ok := f.FS.(RemoveFS)
		if !ok {
			return nil, fs.ErrPermission
		}
		return nil, r.Remove(name)
	case "rename":
		dest, ok := f.relative(call.Dest)
		if !ok {
			return nil, Raise("PermissionError", "Permission denied: %q", call.Dest)
		}
		r, ok := f.FS.(RenameFS)
		if !ok {
			return nil, fs.ErrPermission
		}
		return nil, r.Rename(name, dest)
	}
	return nil, ErrNotHandled
}

func (f *FS) readFile(name string) ([]byte, error) {
	info, err := fs.Stat(f.FS, name)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errIsDir}
	}
	return fs.ReadFile(f.FS, name)
}

func (f *FS) writeFile(name string, data []byte, appendMode bool) error {
	if err := f.Quota.charge(len(data)); err != nil {
		return err
	}
	if appendMode {
		if a, ok := f.FS.(AppendFileFS); ok {
			return a.AppendFile(name, data, 0o644)
		}
	}
	w, ok := f.FS.(WriteFileFS)
	if !ok {
		return fs.ErrPermission
	}
	if appendMode {
		existing, err := fs.ReadFile(f.FS, name)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		data = append(append([]byte(nil), existing...), data...)
	}
	return w.WriteFile(name, data, 0o644)
}

func (f *FS) open(call *OSCall, name string) (any, error) {
	mode := strings.TrimSuffix(strings.ReplaceAll(call.Mode, "b", ""), "+")
	switch mode {
	case "r":
		info, err := fs.Stat(f.FS, name)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			return nil, &fs.PathError{Op: "open", Path: name, Err: errIsDir}
		}
	case "w":
		if err := f.writeFile(name, nil, false); err != nil {
			return nil, err
		}
	case "a":
		if _, err := fs.Stat(f.FS, name); errors.Is(err, fs.ErrNotExist) {
			if err := f.writeFile(name, nil, false); err != nil {
				return nil, err
			}
		}
	}
	return FileHandle{Path: f.virtual(name), Mode: call.Mode}, nil
}

func (f *FS) mkdir(call *OSCall, name string) error {
	if info, err := fs.Stat(f.FS, name); err == nil {
		if call.ExistOK && info.IsDir() {
			return nil
		}
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	m, ok := f.FS.(MkdirFS)
	if !ok {
		return fs.ErrPermission
	}
	if call.Parents {
		parent := path.Dir(name)
		if parent != "." {
			if _, err := fs.Stat(f.FS, parent); errors.Is(err, fs.ErrNotExist) {
				if err := f.mkdir(&OSCall{Parents: true, ExistOK: true}, parent); err != nil {
					return err
				}
			}
		}
	}
	return m.Mkdir(name, 0o755)
}

// fsError maps Go filesystem errors onto Python's OSError hierarchy.
func fsError(err error, call *OSCall) error {
	var exc *Exception
	if errors.As(err, &exc) {
		return exc
	}
	if errors.Is(err, ErrNotHandled) {
		return err
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	p := call.Path
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Raise("FileNotFoundError", "[Errno 2] No such file or directory: '%s'", p)
	case errors.Is(err, fs.ErrExist):
		return Raise("FileExistsError", "[Errno 17] File exists: '%s'", p)
	case errors.Is(err, fs.ErrPermission):
		return Raise("PermissionError", "[Errno 13] Permission denied: '%s'", p)
	case errors.Is(err, errIsDir):
		return Raise("IsADirectoryError", "[Errno 21] Is a directory: '%s'", p)
	case errors.Is(err, errNotDir):
		return Raise("NotADirectoryError", "[Errno 20] Not a directory: '%s'", p)
	case errors.Is(err, errNotEmpty):
		return Raise("OSError", "[Errno 39] Directory not empty: '%s'", p)
	case errors.Is(err, io.EOF):
		return Raise("OSError", "unexpected end of file: '%s'", p)
	}
	msg := err.Error()
	if strings.Contains(msg, "not empty") {
		return Raise("OSError", "[Errno 39] Directory not empty: '%s'", p)
	}
	if strings.Contains(msg, "is a directory") {
		return Raise("IsADirectoryError", "[Errno 21] Is a directory: '%s'", p)
	}
	if strings.Contains(msg, "not a directory") {
		return Raise("NotADirectoryError", "[Errno 20] Not a directory: '%s'", p)
	}
	return Raise("OSError", "%s: '%s'", msg, p)
}

// ---------------------------------------------------------------------------
// Other handlers and composition
// ---------------------------------------------------------------------------

// Handlers tries each handler in order and returns the first answer that is
// not ErrNotHandled.
func Handlers(handlers ...OSHandler) OSHandler {
	return OSFunc(func(ctx context.Context, call *OSCall) (any, error) {
		for _, h := range handlers {
			if h == nil {
				continue
			}
			v, err := h.HandleOSCall(ctx, call)
			if errors.Is(err, ErrNotHandled) {
				continue
			}
			return v, err
		}
		return nil, ErrNotHandled
	})
}

// Env exposes the given variables through os.getenv and os.environ.
func Env(vars map[string]string) OSHandler {
	return OSFunc(func(ctx context.Context, call *OSCall) (any, error) {
		switch call.Name {
		case "getenv":
			if v, ok := vars[call.Key]; ok {
				return v, nil
			}
			return call.Default, nil
		case "get_environ":
			return vars, nil
		}
		return nil, ErrNotHandled
	})
}

// Clock answers datetime.now() and date.today() from now; pass time.Now for
// the real clock.
func Clock(now func() time.Time) OSHandler {
	return OSFunc(func(ctx context.Context, call *OSCall) (any, error) {
		switch call.Name {
		case "date_today", "datetime_now":
			return now(), nil
		}
		return nil, ErrNotHandled
	})
}
