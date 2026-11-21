package monty

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/go-monty/internal/pb"
)

// OSCall is an operating-system request made by sandbox code: a pathlib or
// open() operation, os.getenv / os.environ, or a clock read. Paths are
// virtual POSIX paths inside the sandbox, never host paths.
//
// Name is one of:
//
//	exists is_file is_dir is_symlink read_text read_bytes stat iterdir
//	resolve absolute unlink rmdir            (Path only)
//	write_text append_text                   (Path, Text)
//	write_bytes append_bytes                 (Path, Bytes)
//	open                                     (Path, Mode)
//	mkdir                                    (Path, Parents, ExistOK)
//	rename                                   (Path, Dest)
//	create_exclusive                         (Path, Mode, Permissions)
//	scan                                     (Path, Scan)
//	getenv                                   (Key, Default)
//	get_environ                              (no fields)
//	urandom                                  (Size)
//	import                                   (Module, SearchPath)
//	import_stubs                             (Modules)
//	get_type_hints                           (Function or ClassID)
//
// and, only when the session's OSPolicy routes them to the host:
//
//	date_today                               (no fields)
//	datetime_now                             (TZ)
//	time                                     (Caller)
//	sleep async_sleep                        (Seconds)
//
// Expected results: bool for the checks; string for read_text, resolve and
// absolute; []byte for read_bytes; []Path or []string for iterdir; a
// FileHandle for open and create_exclusive; a NamedTuple named
// "os.stat_result" (see StatResult) for stat; the number of characters or
// bytes written for the writes (nil is accepted and converted); nil for
// mkdir, rename, unlink and rmdir; []ScanEntry for scan; a string (or
// Default) for getenv; map[string]string for get_environ; []byte for
// urandom; a *Module for import; a map of dotted module paths to .pyi source
// for import_stubs; a map of hints for get_type_hints; Date for date_today, a
// time.Time for datetime_now and for time (or seconds since the epoch as a
// float64); anything for the sleeps, once the wait is over.
type OSCall struct {
	Name    string
	Path    string
	Dest    string
	Text    string
	Bytes   []byte
	Mode    string
	Parents bool
	ExistOK bool
	// Permissions holds the access bits (0..0777) for create_exclusive.
	Permissions uint32
	Key         string
	Default     any
	// TZ is the requested zone for datetime_now; nil asks for a naive value.
	TZ *time.Location
	// Caller names the function behind a time call, e.g. "time.monotonic".
	Caller string
	// Seconds is how long sleep and async_sleep should wait.
	Seconds float64
	// Size is the number of bytes urandom asks for.
	Size uint64
	// Module is the dotted name an import asks for; SearchPath the virtual
	// directories the sandbox would search for its .py file.
	Module     string
	SearchPath []string
	// Modules are the top-level modules import_stubs asks stubs for.
	Modules []string
	// Function or ClassID names the host function or class get_type_hints
	// asks about.
	Function string
	ClassID  string
	// Scan holds the arguments of a scan call.
	Scan *ScanArgs
}

// ScanArgs describes the subtree a scan call (os.walk, os.scandir,
// Path.walk, Path.glob) wants.
type ScanArgs struct {
	// MaxDepth is the deepest entry to return; nil for the whole tree.
	MaxDepth       *int
	FollowSymlinks bool
	// Pattern holds glob pattern parts, a pruning hint only.
	Pattern         []string
	CaseSensitive   *bool
	RecurseSymlinks bool
}

// ScanEntry is one entry of a scan result. Path is slash-separated and
// relative to the scanned path; the root itself is "" and comes first.
// IsDir and IsFile follow a symlink, IsSymlink does not.
type ScanEntry struct {
	Path      string
	IsDir     bool
	IsFile    bool
	IsSymlink bool
}

// OSHandler services OSCalls. Return ErrNotHandled to let the sandbox raise
// its default error (PermissionError for filesystem calls, RuntimeError for
// the rest), an *Exception to raise that exception, or a value.
type OSHandler interface {
	HandleOSCall(ctx context.Context, call *OSCall) (any, error)
}

// OSFunc adapts a function to the OSHandler interface.
type OSFunc func(ctx context.Context, call *OSCall) (any, error)

func (f OSFunc) HandleOSCall(ctx context.Context, call *OSCall) (any, error) { return f(ctx, call) }

// osCallFromProto projects the typed wire call into an OSCall, or nil for a
// call this package does not know, which the caller declines.
func (c *codec) osCall(o *pb.OsCall) (*OSCall, error) {
	call := &OSCall{}
	switch k := o.Call.(type) {
	case *pb.OsCall_Exists:
		call.Name, call.Path = "exists", k.Exists
	case *pb.OsCall_IsFile:
		call.Name, call.Path = "is_file", k.IsFile
	case *pb.OsCall_IsDir:
		call.Name, call.Path = "is_dir", k.IsDir
	case *pb.OsCall_IsSymlink:
		call.Name, call.Path = "is_symlink", k.IsSymlink
	case *pb.OsCall_ReadText:
		call.Name, call.Path = "read_text", k.ReadText
	case *pb.OsCall_ReadBytes:
		call.Name, call.Path = "read_bytes", k.ReadBytes
	case *pb.OsCall_Stat:
		call.Name, call.Path = "stat", k.Stat
	case *pb.OsCall_Iterdir:
		call.Name, call.Path = "iterdir", k.Iterdir
	case *pb.OsCall_Resolve:
		call.Name, call.Path = "resolve", k.Resolve
	case *pb.OsCall_Absolute:
		call.Name, call.Path = "absolute", k.Absolute
	case *pb.OsCall_Unlink:
		call.Name, call.Path = "unlink", k.Unlink
	case *pb.OsCall_Rmdir:
		call.Name, call.Path = "rmdir", k.Rmdir
	case *pb.OsCall_WriteText:
		call.Name, call.Path, call.Text = "write_text", k.WriteText.GetPath(), k.WriteText.GetData()
	case *pb.OsCall_AppendText:
		call.Name, call.Path, call.Text = "append_text", k.AppendText.GetPath(), k.AppendText.GetData()
	case *pb.OsCall_WriteBytes:
		call.Name, call.Path, call.Bytes = "write_bytes", k.WriteBytes.GetPath(), k.WriteBytes.GetData()
	case *pb.OsCall_AppendBytes:
		call.Name, call.Path, call.Bytes = "append_bytes", k.AppendBytes.GetPath(), k.AppendBytes.GetData()
	case *pb.OsCall_Open_:
		call.Name, call.Path, call.Mode = "open", k.Open.GetPath(), k.Open.GetMode()
	case *pb.OsCall_CreateExclusive_:
		call.Name, call.Path, call.Mode = "create_exclusive", k.CreateExclusive.GetPath(), k.CreateExclusive.GetMode()
		call.Permissions = k.CreateExclusive.GetPermissions()
	case *pb.OsCall_Scan_:
		call.Name, call.Path = "scan", k.Scan.GetPath()
		call.Scan = &ScanArgs{
			FollowSymlinks:  k.Scan.GetFollowSymlinks(),
			Pattern:         k.Scan.GetPattern(),
			CaseSensitive:   k.Scan.CaseSensitive,
			RecurseSymlinks: k.Scan.GetRecurseSymlinks(),
		}
		if k.Scan.MaxDepth != nil {
			depth := int(*k.Scan.MaxDepth)
			call.Scan.MaxDepth = &depth
		}
	case *pb.OsCall_Mkdir_:
		call.Name, call.Path = "mkdir", k.Mkdir.GetPath()
		call.Parents, call.ExistOK = k.Mkdir.GetParents(), k.Mkdir.GetExistOk()
	case *pb.OsCall_Rename_:
		call.Name, call.Path, call.Dest = "rename", k.Rename.GetSrc(), k.Rename.GetDst()
	case *pb.OsCall_Getenv_:
		call.Name, call.Key = "getenv", k.Getenv.GetKey()
		vals, err := c.decode(o.GetValues())
		if err != nil {
			return nil, err
		}
		if call.Default, err = vals.at(k.Getenv.GetDefault()); err != nil {
			return nil, err
		}
	case *pb.OsCall_GetEnviron:
		call.Name = "get_environ"
	case *pb.OsCall_DateToday:
		call.Name = "date_today"
	case *pb.OsCall_DateTimeNow_:
		call.Name = "datetime_now"
		if tz := k.DateTimeNow.GetTz(); tz != nil {
			call.TZ = time.FixedZone(tz.GetName(), int(tz.GetOffsetSeconds()))
		}
	case *pb.OsCall_Urandom_:
		call.Name, call.Size = "urandom", k.Urandom.GetSize()
	case *pb.OsCall_Time:
		call.Name, call.Caller = "time", k.Time.GetCaller()
	case *pb.OsCall_Sleep_:
		call.Name, call.Seconds = "sleep", k.Sleep.GetSeconds()
	case *pb.OsCall_AsyncSleep_:
		call.Name, call.Seconds = "async_sleep", k.AsyncSleep.GetDelay()
	case *pb.OsCall_SystemSleep:
		call.Name, call.Seconds = systemSleep, k.SystemSleep.GetSeconds()
	case *pb.OsCall_AsyncSystemSleep:
		call.Name, call.Seconds = asyncSystemSleep, k.AsyncSystemSleep.GetDelay()
	case *pb.OsCall_Import_:
		call.Name, call.Module, call.SearchPath = "import", k.Import.GetModule(), k.Import.GetSearchPath()
	case *pb.OsCall_ImportStubs_:
		call.Name, call.Modules = "import_stubs", k.ImportStubs.GetModules()
	case *pb.OsCall_GetTypeHints_:
		call.Name = "get_type_hints"
		call.Function = k.GetTypeHints.GetFunction()
		call.ClassID = uuidString(k.GetTypeHints.GetClassId())
	default:
		return nil, nil
	}
	return call, nil
}

// System sleeps are waited out by the session itself and never reach an
// OSHandler.
const (
	systemSleep      = "system_sleep"
	asyncSystemSleep = "async_system_sleep"
)

// osResult appends a handler result to a, applying per-call conveniences.
func (a *arena) osResult(call *OSCall, v any) (uint32, error) {
	switch call.Name {
	case "write_text", "append_text":
		if v == nil {
			v = utf8.RuneCountInString(call.Text)
		}
	case "write_bytes", "append_bytes":
		if v == nil {
			v = len(call.Bytes)
		}
	case "iterdir":
		switch names := v.(type) {
		case []string:
			paths := make([]any, len(names))
			for i, n := range names {
				paths[i] = Path(n)
			}
			v = paths
		case []Path:
			paths := make([]any, len(names))
			for i, n := range names {
				paths[i] = n
			}
			v = paths
		}
	case "scan":
		if entries, ok := v.([]ScanEntry); ok {
			items := make([]any, len(entries))
			for i, e := range entries {
				items[i] = Tuple{e.Path, e.IsDir, e.IsFile, e.IsSymlink}
			}
			v = items
		}
	case "get_environ":
		if env, ok := v.(map[string]string); ok {
			m := make(map[string]any, len(env))
			for k, val := range env {
				m[k] = val
			}
			v = m
		}
	case "import_stubs":
		if stubs, ok := v.(map[string]string); ok {
			m := make(map[string]any, len(stubs))
			for k, val := range stubs {
				m[k] = val
			}
			v = m
		}
	case "date_today":
		if t, ok := v.(time.Time); ok {
			v = Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}
		}
	case "datetime_now":
		if t, ok := v.(time.Time); ok {
			if call.TZ == nil {
				return a.push(timeNode(t, true)), nil
			}
			v = t.In(call.TZ)
		}
	case "time":
		if t, ok := v.(time.Time); ok {
			v = float64(t.UnixNano()) / 1e9
		}
	}
	return a.encode(v)
}

// StatResult builds the os.stat_result named tuple an OSHandler returns for
// a "stat" call. isDir selects directory mode bits (0o755) over file bits
// (0o644).
func StatResult(size int64, modTime time.Time, isDir bool) NamedTuple {
	mode := int64(0o100644)
	if isDir {
		mode = 0o040755
	}
	mtime := modTime.Unix()
	return NamedTuple{
		TypeName: "os.stat_result",
		Fields:   []string{"st_mode", "st_ino", "st_dev", "st_nlink", "st_uid", "st_gid", "st_size", "st_atime", "st_mtime", "st_ctime"},
		Values:   []any{mode, int64(0), int64(0), int64(1), int64(0), int64(0), size, mtime, mtime, mtime},
	}
}
