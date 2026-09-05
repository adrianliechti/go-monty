package monty_test

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	monty "github.com/adrianliechti/go-monty"
)

var rt *monty.Runtime

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	rt, err = monty.NewRuntime(ctx)
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = rt.Close(ctx)
	os.Exit(code)
}

func newSession(t *testing.T, opts monty.SessionOptions) *monty.Session {
	t.Helper()
	s, err := rt.NewSession(context.Background(), opts)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestVersion(t *testing.T) {
	if rt.Version() == "" {
		t.Fatal("empty version")
	}
	t.Logf("monty %s", rt.Version())
}

func TestRunExpression(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	out, err := s.Run(context.Background(), "40 + 2", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != int64(42) {
		t.Fatalf("got %#v", out)
	}
}

func TestInputsAndFunctions(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	var stdout bytes.Buffer
	code := `
kcal = nutrition('chocolate bar')['kcal']
hours = kcal * 4184 / (bulb_watts * 3600)
print(f'a chocolate bar powers a {bulb_watts} W bulb for {hours:.1f} hours')
{'hours': round(hours, 1), 'tags': tags}
`
	var got *monty.Call
	out, err := s.Run(context.Background(), code, monty.RunOptions{
		Inputs: map[string]any{"bulb_watts": 10, "tags": []string{"a", "b"}},
		Functions: map[string]monty.Function{
			"nutrition": func(ctx context.Context, call *monty.Call) (any, error) {
				got = call
				return map[string]any{"kcal": 230}, nil
			},
		},
		Stdout: &stdout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "nutrition" || len(got.Args) != 1 || got.Args[0] != "chocolate bar" {
		t.Fatalf("call = %#v", got)
	}
	m, ok := out.(map[string]any)
	if !ok || m["hours"] != 26.7 {
		t.Fatalf("out = %#v", out)
	}
	if tags, ok := m["tags"].([]any); !ok || len(tags) != 2 || tags[1] != "b" {
		t.Fatalf("tags = %#v", m["tags"])
	}
	if want := "a chocolate bar powers a 10 W bulb for 26.7 hours\n"; stdout.String() != want {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestGlobalsPersist(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	ctx := context.Background()
	if _, err := s.Run(ctx, "x = [1, 2, 3]", monty.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Run(ctx, "sum(x)", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != int64(6) {
		t.Fatalf("got %#v", out)
	}
}

func TestPythonException(t *testing.T) {
	s := newSession(t, monty.SessionOptions{ScriptName: "calc.py"})
	_, err := s.Run(context.Background(), "def f():\n    raise ValueError('boom')\nf()", monty.RunOptions{})
	var exc *monty.Exception
	if !errors.As(err, &exc) {
		t.Fatalf("err = %v", err)
	}
	if exc.Type != "ValueError" || exc.Message != "boom" {
		t.Fatalf("exc = %#v", exc)
	}
	if !strings.Contains(exc.Traceback, "line 3, in <module>") || !strings.Contains(exc.Traceback, "line 2, in f") {
		t.Fatalf("traceback:\n%s", exc.Traceback)
	}
	// The session survives an exception.
	if out, err := s.Run(context.Background(), "1", monty.RunOptions{}); err != nil || out != int64(1) {
		t.Fatalf("after error: %v %v", out, err)
	}
}

func TestHostFunctionRaises(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	out, err := s.Run(context.Background(), `
try:
    lookup('x')
except KeyError as e:
    result = f'caught {e}'
result`, monty.RunOptions{
		Functions: map[string]monty.Function{
			"lookup": func(ctx context.Context, call *monty.Call) (any, error) {
				return nil, monty.Raise("KeyError", "no %s", call.Args[0])
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "caught 'no x'" && out != "caught no x" {
		t.Fatalf("got %#v", out)
	}
}

func TestUnknownFunction(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	_, err := s.Run(context.Background(), "nope()", monty.RunOptions{})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "NameError" {
		t.Fatalf("err = %v", err)
	}
}

func TestValueRoundTrip(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	big1 := new(big.Int).Lsh(big.NewInt(1), 100)
	when := time.Date(2024, 3, 1, 12, 30, 0, 0, time.FixedZone("CET", 3600))
	out, err := s.Run(context.Background(), "[n, big, f, s, b, t, d, dt, td, none, (1, 'a'), {1, 2}, {'k': [1]}, {1: 2}]", monty.RunOptions{
		Inputs: map[string]any{
			"n": 7, "big": big1, "f": 1.5, "s": "hé", "b": []byte{1, 2},
			"t": monty.Tuple{1, 2}, "d": monty.Date{Year: 2020, Month: 2, Day: 29},
			"dt": when, "td": 90 * time.Minute, "none": nil,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	if items[0] != int64(7) || items[1].(*big.Int).Cmp(big1) != 0 || items[2] != 1.5 || items[3] != "hé" {
		t.Fatalf("scalars: %#v", items[:4])
	}
	if !bytes.Equal(items[4].([]byte), []byte{1, 2}) {
		t.Fatalf("bytes: %#v", items[4])
	}
	if tup, ok := items[5].(monty.Tuple); !ok || tup[1] != int64(2) {
		t.Fatalf("tuple: %#v", items[5])
	}
	if items[6] != (monty.Date{Year: 2020, Month: 2, Day: 29}) {
		t.Fatalf("date: %#v", items[6])
	}
	if got := items[7].(time.Time); !got.Equal(when) {
		t.Fatalf("datetime: %v", got)
	}
	if items[8] != 90*time.Minute {
		t.Fatalf("timedelta: %#v", items[8])
	}
	if items[9] != nil {
		t.Fatalf("none: %#v", items[9])
	}
	if tup := items[10].(monty.Tuple); tup[1] != "a" {
		t.Fatalf("tuple literal: %#v", items[10])
	}
	if set := items[11].(monty.Set); len(set) != 2 {
		t.Fatalf("set: %#v", items[11])
	}
	if m := items[12].(map[string]any); m["k"].([]any)[0] != int64(1) {
		t.Fatalf("dict: %#v", items[12])
	}
	if d := items[13].(monty.Dict); d[0].Key != int64(1) {
		t.Fatalf("int-key dict: %#v", items[13])
	}
}

func TestDumpRestore(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{ScriptName: "state.py"})
	if _, err := s.Run(ctx, "counter = 41", monty.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err := s.Dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := rt.Restore(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	if restored.Pending() != nil {
		t.Fatal("unexpected pending call")
	}
	out, err := restored.Run(ctx, "counter + 1", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != int64(42) {
		t.Fatalf("got %#v", out)
	}
}

func TestMaxDuration(t *testing.T) {
	s := newSession(t, monty.SessionOptions{Limits: &monty.Limits{MaxDuration: 50 * time.Millisecond}})
	_, err := s.Run(context.Background(), "while True:\n    pass", monty.RunOptions{})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "TimeoutError" {
		t.Fatalf("err = %v", err)
	}
}

func TestMaxMemory(t *testing.T) {
	s := newSession(t, monty.SessionOptions{Limits: &monty.Limits{MaxMemory: 8 << 20}})
	_, err := s.Run(context.Background(), "x = []\nwhile True:\n    x.append('a' * 1000)", monty.RunOptions{})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "MemoryError" {
		t.Fatalf("err = %v", err)
	}
}

func TestContextDeadline(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.Run(ctx, "while True:\n    pass", monty.RunOptions{})
	var werr *monty.WorkerError
	if !errors.As(err, &werr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Run(context.Background(), "1", monty.RunOptions{}); !errors.Is(err, monty.ErrClosed) {
		t.Fatalf("dead session err = %v", err)
	}
}

func TestMaxSuspensions(t *testing.T) {
	s := newSession(t, monty.SessionOptions{Limits: &monty.Limits{MaxSuspensions: 3}})
	calls := 0
	_, err := s.Run(context.Background(), "for i in range(10):\n    ping()", monty.RunOptions{
		Functions: map[string]monty.Function{
			"ping": func(ctx context.Context, call *monty.Call) (any, error) { calls++; return nil, nil },
		},
	})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "RuntimeError" || !strings.Contains(exc.Message, "suspension limit") {
		t.Fatalf("err = %v", err)
	}
	if calls > 3 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestTypeCheck(t *testing.T) {
	s := newSession(t, monty.SessionOptions{TypeCheck: true, TypeCheckFormat: monty.TypeCheckConcise})
	_, err := s.Run(context.Background(), "x: int = 'no'", monty.RunOptions{})
	var terr *monty.TypingError
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(terr.Diagnostics, "int") {
		t.Fatalf("diagnostics = %q", terr.Diagnostics)
	}
}

func TestOSHandler(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	files := map[string]string{"/data/in.txt": "hello"}
	handler := monty.OSFunc(func(ctx context.Context, call *monty.OSCall) (any, error) {
		switch call.Name {
		case "exists":
			_, ok := files[call.Path]
			return ok, nil
		case "read_text":
			if s, ok := files[call.Path]; ok {
				return s, nil
			}
			return nil, monty.Raise("FileNotFoundError", "No such file: %s", call.Path)
		case "write_text":
			files[call.Path] = call.Text
			return nil, nil
		case "getenv":
			if call.Key == "HOME" {
				return "/home/sandbox", nil
			}
			return call.Default, nil
		case "datetime_now":
			return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), nil
		}
		return nil, monty.ErrNotHandled
	})
	out, err := s.Run(context.Background(), `
from pathlib import Path
import os
from datetime import datetime
p = Path('/data/in.txt')
Path('/data/out.txt').write_text(p.read_text().upper())
[p.exists(), Path('/nope').exists(), os.getenv('HOME'), os.getenv('X', 'dflt'), datetime.now().year]
`, monty.RunOptions{OS: handler})
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	if items[0] != true || items[1] != false || items[2] != "/home/sandbox" || items[3] != "dflt" || items[4] != int64(2024) {
		t.Fatalf("got %#v", items)
	}
	if files["/data/out.txt"] != "HELLO" {
		t.Fatalf("files = %v", files)
	}
	// Declined calls raise PermissionError.
	_, err = s.Run(context.Background(), "Path('/x').unlink()", monty.RunOptions{OS: handler})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "PermissionError" {
		t.Fatalf("err = %v", err)
	}
}

func TestReset(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{})
	if _, err := s.Run(ctx, "x = 1", monty.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(ctx, monty.SessionOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Run(ctx, "x", monty.RunOptions{})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "NameError" {
		t.Fatalf("err = %v", err)
	}
}

func BenchmarkNewSession(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		s, err := rt.NewSession(ctx, monty.SessionOptions{})
		if err != nil {
			b.Fatal(err)
		}
		_ = s.Close(ctx)
	}
}

func BenchmarkRun(b *testing.B) {
	ctx := context.Background()
	s, err := rt.NewSession(ctx, monty.SessionOptions{})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close(ctx)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Run(ctx, "sum(range(100))", monty.RunOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestConcurrentSessions(t *testing.T) {
	ctx := context.Background()
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			s, err := rt.NewSession(ctx, monty.SessionOptions{})
			if err != nil {
				errs <- err
				return
			}
			defer s.Close(ctx)
			out, err := s.Run(ctx, "n * n", monty.RunOptions{Inputs: map[string]any{"n": i}})
			if err == nil && out != int64(i*i) {
				err = errors.New("wrong result")
			}
			errs <- err
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecursionLimit(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	_, err := s.Run(context.Background(), "def f(n):\n    return f(n + 1)\nf(0)", monty.RunOptions{})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "RecursionError" {
		t.Fatalf("err = %v", err)
	}
	// Deeper than the default limit but still inside the wasm stack.
	s2 := newSession(t, monty.SessionOptions{Limits: &monty.Limits{MaxRecursionDepth: 5000}})
	out, err := s2.Run(context.Background(), "def f(n):\n    return 0 if n == 0 else 1 + f(n - 1)\nf(4000)", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != int64(4000) {
		t.Fatalf("got %#v", out)
	}
}

func TestRuntimeCacheDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	start := time.Now()
	r1, err := monty.NewRuntime(ctx, monty.WithCacheDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	cold := time.Since(start)
	_ = r1.Close(ctx)
	start = time.Now()
	r2, err := monty.NewRuntime(ctx, monty.WithCacheDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	warm := time.Since(start)
	_ = r2.Close(ctx)
	t.Logf("NewRuntime cold %v, cached %v", cold, warm)
}

func TestLazyValuesAndExecutionTime(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	converted := 0
	out, err := s.Run(context.Background(), "greeting + ' ' + name", monty.RunOptions{
		Values: map[string]any{"greeting": "hello", "name": "world", "unused": func() {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello world" || converted != 0 {
		t.Fatalf("got %#v", out)
	}
	if s.ExecutionTime() <= 0 {
		t.Fatalf("execution time = %v", s.ExecutionTime())
	}
}

func TestHostPanicKeepsSessionUsable(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic did not propagate")
			}
		}()
		_, _ = s.Run(context.Background(), "boom()", monty.RunOptions{
			Functions: map[string]monty.Function{
				"boom": func(ctx context.Context, call *monty.Call) (any, error) { panic("oops") },
			},
		})
	}()
	out, err := s.Run(context.Background(), "1 + 1", monty.RunOptions{})
	if err != nil || out != int64(2) {
		t.Fatalf("after panic: %v %v", out, err)
	}
}

func TestOversizedResultAborts(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	_, err := s.Run(context.Background(), "len(blob())", monty.RunOptions{
		Functions: map[string]monty.Function{
			"blob": func(ctx context.Context, call *monty.Call) (any, error) { return make([]byte, 300<<20), nil },
		},
	})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "RuntimeError" {
		t.Fatalf("err = %v", err)
	}
	if out, err := s.Run(context.Background(), "2", monty.RunOptions{}); err != nil || out != int64(2) {
		t.Fatalf("after abort: %v %v", out, err)
	}
}
