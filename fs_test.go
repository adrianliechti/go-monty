package monty_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	monty "github.com/adrianliechti/go-monty"
)

const fsScript = `
from pathlib import Path
import json

root = Path('/data')
names = sorted(p.name for p in root.iterdir())
text = (root / 'in.txt').read_text()
(root / 'out').mkdir(exist_ok=True)
(root / 'out' / 'result.json').write_text(json.dumps({'upper': text.upper()}))
with open('/data/out/log.txt', 'a') as f:
    f.write('line 1\n')
    f.write('line 2\n')
(root / 'out' / 'log.txt').read_text()
with open('/data/in.txt') as f:
    first = f.read(2)
(root / 'out' / 'result.json').rename(root / 'out' / 'final.json')
(root / 'nested' / 'deep').mkdir(parents=True)
stat = (root / 'in.txt').stat()
[
    names,
    json.loads((root / 'out' / 'final.json').read_text()),
    (root / 'out' / 'log.txt').read_text(),
    first,
    (root / 'nested' / 'deep').is_dir(),
    stat.st_size,
    (root / 'in.txt').resolve().as_posix(),
]
`

func runFSScript(t *testing.T, fsys *monty.FS) []any {
	t.Helper()
	s := newSession(t, monty.SessionOptions{})
	out, err := s.Run(context.Background(), fsScript, monty.RunOptions{OS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	names := items[0].([]any)
	if len(names) != 2 || names[0] != "in.txt" || names[1] != "sub" {
		t.Fatalf("names = %#v", names)
	}
	if m := items[1].(map[string]any); m["upper"] != "HELLO" {
		t.Fatalf("json = %#v", items[1])
	}
	if items[2] != "line 1\nline 2\n" {
		t.Fatalf("log = %#v", items[2])
	}
	if items[3] != "he" {
		t.Fatalf("first = %#v", items[3])
	}
	if items[4] != true {
		t.Fatalf("is_dir = %#v", items[4])
	}
	if items[5] != int64(5) {
		t.Fatalf("st_size = %#v", items[5])
	}
	if items[6] != "/data/in.txt" {
		t.Fatalf("resolve = %#v", items[6])
	}
	return items
}

func TestMemFS(t *testing.T) {
	mem := monty.NewMemFS(map[string]any{"in.txt": "hello", "sub/x.bin": []byte{1}})
	runFSScript(t, monty.NewFS("/data", mem))
	if data, err := mem.ReadFile("out/final.json"); err != nil || len(data) == 0 {
		t.Fatalf("final.json: %v %q", err, data)
	}
	if _, err := mem.Stat("out/result.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("result.json should be renamed away: %v", err)
	}
}

func TestDirFS(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := monty.DirFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	runFSScript(t, monty.NewFS("/data", root))
	if data, err := os.ReadFile(filepath.Join(dir, "out", "log.txt")); err != nil || string(data) != "line 1\nline 2\n" {
		t.Fatalf("log.txt on disk: %v %q", err, data)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested", "deep")); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyFSAndErrors(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	ro := monty.NewFS("/ro", fstest.MapFS{"a.txt": {Data: []byte("a")}})
	cases := map[string]string{
		"Path('/ro/a.txt').write_text('x')":  "PermissionError",
		"Path('/ro/missing').read_text()":    "FileNotFoundError",
		"Path('/ro/a.txt').mkdir()":          "FileExistsError",
		"Path('/ro').read_text()":            "IsADirectoryError",
		"Path('/elsewhere/a').read_text()":   "PermissionError", // outside the mount: not handled
		"Path('/ro/../etc/passwd').exists()": "PermissionError", // cleaned to /etc/passwd: outside the mount
	}
	for code, want := range cases {
		_, err := s.Run(context.Background(), "from pathlib import Path\n"+code, monty.RunOptions{OS: ro})
		var exc *monty.Exception
		if !errors.As(err, &exc) || exc.Type != want {
			t.Errorf("%s: got %v, want %s", code, err, want)
		}
	}
}

func TestHandlersEnvClock(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	fixed := time.Date(2030, 5, 6, 7, 8, 9, 0, time.UTC)
	os := monty.Handlers(
		monty.NewFS("/data", monty.NewMemFS(map[string]any{"f": "1"})),
		monty.Env(map[string]string{"API_KEY": "secret"}),
		monty.Clock(func() time.Time { return fixed }),
	)
	out, err := s.Run(context.Background(), `
import os
from datetime import datetime, date, timezone
from pathlib import Path
[os.environ['API_KEY'], os.getenv('NOPE', 'd'), Path('/data/f').read_text(), date.today().isoformat(), datetime.now(timezone.utc).hour]
`, monty.RunOptions{OS: os})
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	if items[0] != "secret" || items[1] != "d" || items[2] != "1" || items[3] != "2030-05-06" || items[4] != int64(7) {
		t.Fatalf("got %#v", items)
	}
}
