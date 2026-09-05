package monty

import (
	"io/fs"
	"path"
	"sort"
	"sync"
)

// ReadOnly hides any write capability of fsys, so a mount over it refuses
// writes with PermissionError even when fsys itself is writable.
func ReadOnly(fsys fs.FS) fs.FS {
	return readOnlyFS{fsys}
}

type readOnlyFS struct{ fsys fs.FS }

func (r readOnlyFS) Open(name string) (fs.File, error)          { return r.fsys.Open(name) }
func (r readOnlyFS) Stat(name string) (fs.FileInfo, error)      { return fs.Stat(r.fsys, name) }
func (r readOnlyFS) ReadFile(name string) ([]byte, error)       { return fs.ReadFile(r.fsys, name) }
func (r readOnlyFS) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(r.fsys, name) }

// ---------------------------------------------------------------------------
// Overlay
// ---------------------------------------------------------------------------

// Overlay is a copy-on-write filesystem: reads fall through to Base, writes
// and deletions land in an in-memory upper layer, and Base is never
// modified. Use it to let sandbox code "edit" a real directory while you
// decide afterwards what to keep; Upper exposes the changes.
type Overlay struct {
	Base fs.FS

	mu       sync.Mutex
	upper    *MemFS
	whiteout map[string]bool // names deleted or replaced from Base
	opaque   map[string]bool // directories whose Base entries are hidden
}

// NewOverlay creates an Overlay over base.
func NewOverlay(base fs.FS) *Overlay {
	return &Overlay{Base: base, upper: NewMemFS(nil), whiteout: map[string]bool{}, opaque: map[string]bool{}}
}

// Upper is the in-memory layer holding every file written or changed.
func (o *Overlay) Upper() *MemFS { return o.upper }

// Deleted lists the Base paths that sandbox code removed or replaced.
func (o *Overlay) Deleted() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for name := range o.whiteout {
		if _, err := fs.Stat(o.Base, name); err == nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// hidden reports whether name or an ancestor was whited out and not
// recreated in the upper layer.
func (o *Overlay) hidden(name string) bool {
	for n := name; n != "." && n != ""; n = path.Dir(n) {
		if o.whiteout[n] {
			if _, err := o.upper.Stat(n); err != nil {
				return true
			}
			// Recreated in upper: the base subtree below it stays hidden
			// only for its own entries (opaque), so keep checking parents.
		}
		if o.opaque[n] && n != name {
			return true
		}
	}
	return false
}

func (o *Overlay) Open(name string) (fs.File, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if f, err := o.upper.Open(name); err == nil {
		return f, nil
	}
	if o.hidden(name) || o.whiteout[name] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return o.Base.Open(name)
}

func (o *Overlay) Stat(name string) (fs.FileInfo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stat(name)
}

func (o *Overlay) stat(name string) (fs.FileInfo, error) {
	if info, err := o.upper.Stat(name); err == nil {
		return info, nil
	}
	if o.hidden(name) || o.whiteout[name] {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fs.Stat(o.Base, name)
}

func (o *Overlay) ReadFile(name string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.readFile(name)
}

func (o *Overlay) readFile(name string) ([]byte, error) {
	if data, err := o.upper.ReadFile(name); err == nil {
		return data, nil
	}
	if o.hidden(name) || o.whiteout[name] {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return fs.ReadFile(o.Base, name)
}

func (o *Overlay) ReadDir(name string) ([]fs.DirEntry, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	info, err := o.stat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errNotDir}
	}
	merged := map[string]fs.DirEntry{}
	if !o.opaque[name] && !o.hidden(name) {
		if entries, err := fs.ReadDir(o.Base, name); err == nil {
			for _, e := range entries {
				child := path.Join(name, e.Name())
				if !o.whiteout[child] {
					merged[e.Name()] = e
				}
			}
		}
	}
	if entries, err := o.upper.ReadDir(name); err == nil {
		for _, e := range entries {
			merged[e.Name()] = e
		}
	}
	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]fs.DirEntry, 0, len(names))
	for _, n := range names {
		out = append(out, merged[n])
	}
	return out, nil
}

// ensureUpperDir mirrors name's directory chain into the upper layer so
// writes below it succeed.
func (o *Overlay) ensureUpperDir(dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	if info, err := o.upper.Stat(dir); err == nil {
		if !info.IsDir() {
			return &fs.PathError{Op: "mkdir", Path: dir, Err: errNotDir}
		}
		return nil
	}
	info, err := o.stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &fs.PathError{Op: "mkdir", Path: dir, Err: errNotDir}
	}
	if err := o.ensureUpperDir(path.Dir(dir)); err != nil {
		return err
	}
	return o.upper.Mkdir(dir, 0o755)
}

func (o *Overlay) WriteFile(name string, data []byte, perm fs.FileMode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if info, err := o.stat(name); err == nil && info.IsDir() {
		return &fs.PathError{Op: "write", Path: name, Err: errIsDir}
	}
	if err := o.ensureUpperDir(path.Dir(name)); err != nil {
		return err
	}
	delete(o.whiteout, name)
	return o.upper.WriteFile(name, data, perm)
}

func (o *Overlay) AppendFile(name string, data []byte, perm fs.FileMode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	existing, err := o.readFile(name)
	if err != nil && !isNotExist(err) {
		return err
	}
	if err := o.ensureUpperDir(path.Dir(name)); err != nil {
		return err
	}
	delete(o.whiteout, name)
	return o.upper.WriteFile(name, append(append([]byte(nil), existing...), data...), perm)
}

func (o *Overlay) Mkdir(name string, perm fs.FileMode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.stat(name); err == nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	if err := o.ensureUpperDir(path.Dir(name)); err != nil {
		return err
	}
	if o.whiteout[name] {
		// Recreating a deleted base directory: its old contents stay gone.
		o.opaque[name] = true
		delete(o.whiteout, name)
	}
	return o.upper.Mkdir(name, perm)
}

func (o *Overlay) Remove(name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	info, err := o.stat(name)
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, _ := o.readDirLocked(name)
		if len(entries) > 0 {
			return &fs.PathError{Op: "remove", Path: name, Err: errNotEmpty}
		}
	}
	if _, err := o.upper.Stat(name); err == nil {
		if err := o.upper.Remove(name); err != nil {
			return err
		}
	}
	delete(o.opaque, name)
	if _, err := fs.Stat(o.Base, name); err == nil {
		o.whiteout[name] = true
	}
	return nil
}

func (o *Overlay) readDirLocked(name string) ([]fs.DirEntry, error) {
	o.mu.Unlock()
	defer o.mu.Lock()
	return o.ReadDir(name)
}

func (o *Overlay) Rename(oldname, newname string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	info, err := o.stat(oldname)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return &fs.PathError{Op: "rename", Path: oldname, Err: fs.ErrPermission}
	}
	data, err := o.readFile(oldname)
	if err != nil {
		return err
	}
	if err := o.ensureUpperDir(path.Dir(newname)); err != nil {
		return err
	}
	if err := o.upper.WriteFile(newname, data, info.Mode().Perm()); err != nil {
		return err
	}
	delete(o.whiteout, newname)
	if _, err := o.upper.Stat(oldname); err == nil {
		_ = o.upper.Remove(oldname)
	}
	if _, err := fs.Stat(o.Base, oldname); err == nil {
		o.whiteout[oldname] = true
	}
	return nil
}

func isNotExist(err error) bool {
	if pe, ok := err.(*fs.PathError); ok {
		err = pe.Err
	}
	return err == fs.ErrNotExist
}

// ---------------------------------------------------------------------------
// Write quota
// ---------------------------------------------------------------------------

// WriteQuota caps the bytes written through a mount. Set FS.Quota to use it;
// Reset starts the count over, e.g. between runs.
type WriteQuota struct {
	Limit int64

	mu      sync.Mutex
	written int64
}

// Written reports the bytes charged so far.
func (q *WriteQuota) Written() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.written
}

// Reset starts the count over.
func (q *WriteQuota) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.written = 0
}

// charge reserves n bytes, or fails when the limit would be exceeded.
func (q *WriteQuota) charge(n int) error {
	if q == nil || q.Limit <= 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.written+int64(n) > q.Limit {
		return Raise("OSError", "write quota of %d bytes exceeded", q.Limit)
	}
	q.written += int64(n)
	return nil
}
