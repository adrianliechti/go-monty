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
//	getenv                                   (Key, Default)
//	get_environ date_today                   (no fields)
//	datetime_now                             (TZ)
//
// Expected results: bool for the checks; string for read_text, resolve and
// absolute; []byte for read_bytes; []Path or []string for iterdir; a
// FileHandle for open; a NamedTuple named "os.stat_result" (see StatResult)
// for stat; the number of characters or bytes written for the writes (nil is
// accepted and converted); nil for mkdir, rename, unlink and rmdir; a string
// (or Default) for getenv; map[string]string for get_environ; Date for
// date_today and a time.Time for datetime_now.
type OSCall struct {
	Name    string
	Path    string
	Dest    string
	Text    string
	Bytes   []byte
	Mode    string
	Parents bool
	ExistOK bool
	Key     string
	Default any
	// TZ is the requested zone for datetime_now; nil asks for a naive value.
	TZ *time.Location
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

// osCallFromProto projects the typed wire call into an OSCall.
func osCallFromProto(c *pb.OsCall) (*OSCall, error) {
	call := &OSCall{}
	switch k := c.Call.(type) {
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
	case *pb.OsCall_Mkdir_:
		call.Name, call.Path = "mkdir", k.Mkdir.GetPath()
		call.Parents, call.ExistOK = k.Mkdir.GetParents(), k.Mkdir.GetExistOk()
	case *pb.OsCall_Rename_:
		call.Name, call.Path, call.Dest = "rename", k.Rename.GetSrc(), k.Rename.GetDst()
	case *pb.OsCall_Getenv_:
		call.Name, call.Key = "getenv", k.Getenv.GetKey()
		def, err := decode(k.Getenv.GetDefault())
		if err != nil {
			return nil, err
		}
		call.Default = def
	case *pb.OsCall_GetEnviron:
		call.Name = "get_environ"
	case *pb.OsCall_DateToday:
		call.Name = "date_today"
	case *pb.OsCall_DateTimeNow_:
		call.Name = "datetime_now"
		if tz := k.DateTimeNow.GetTz(); tz != nil {
			call.TZ = time.FixedZone(tz.GetName(), int(tz.GetOffsetSeconds()))
		}
	default:
		return nil, Raise("RuntimeError", "unknown OS call %T", c.Call)
	}
	return call, nil
}

// encodeOSResult converts a handler result, applying per-call conveniences.
func encodeOSResult(call *OSCall, v any) (*pb.MontyObject, error) {
	switch call.Name {
	case "write_text", "append_text":
		if v == nil {
			return encode(utf8.RuneCountInString(call.Text))
		}
	case "write_bytes", "append_bytes":
		if v == nil {
			return encode(len(call.Bytes))
		}
	case "iterdir":
		if names, ok := v.([]string); ok {
			paths := make([]any, len(names))
			for i, n := range names {
				paths[i] = Path(n)
			}
			return encode(paths)
		}
		if paths, ok := v.([]Path); ok {
			items := make([]any, len(paths))
			for i, p := range paths {
				items[i] = p
			}
			return encode(items)
		}
	case "get_environ":
		if env, ok := v.(map[string]string); ok {
			m := make(map[string]any, len(env))
			for k, val := range env {
				m[k] = val
			}
			return encode(m)
		}
	case "date_today":
		if t, ok := v.(time.Time); ok {
			return encode(Date{Year: t.Year(), Month: t.Month(), Day: t.Day()})
		}
	case "datetime_now":
		if t, ok := v.(time.Time); ok {
			if call.TZ == nil {
				// A naive result carries no offset.
				dt := encodeTime(t)
				dt.GetDatetime().OffsetSeconds = nil
				dt.GetDatetime().TimezoneName = nil
				return dt, nil
			}
			return encode(t.In(call.TZ))
		}
	}
	return encode(v)
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
