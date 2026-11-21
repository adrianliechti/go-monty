package monty_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	monty "github.com/adrianliechti/go-monty"
)

func TestClockPolicy(t *testing.T) {
	ctx := context.Background()
	// The default reads the real clock without an OS handler.
	out, err := newSession(t, monty.SessionOptions{}).Run(ctx, "from datetime import datetime\ndatetime.now().year", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if year := out.(int64); year < int64(time.Now().UTC().Year())-1 {
		t.Fatalf("year = %d", year)
	}

	fixed := time.Date(2030, 5, 6, 7, 8, 9, 0, time.UTC)
	out, err = newSession(t, monty.SessionOptions{OSPolicy: monty.OSPolicy{Clock: monty.ClockFixed, FixedTime: fixed}}).
		Run(ctx, "from datetime import datetime\ndatetime.now().isoformat()", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "2030-05-06T07:08:09" {
		t.Fatalf("fixed now = %v", out)
	}

	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Skip("no tz database on this host")
	}
	out, err = newSession(t, monty.SessionOptions{OSPolicy: monty.OSPolicy{Clock: monty.ClockFixed, FixedTime: fixed, TimeZone: zurich}}).
		Run(ctx, "from datetime import datetime\ndatetime.now().hour", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != int64(9) {
		t.Fatalf("zurich hour = %v", out)
	}
}

func TestSleep(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{Limits: &monty.Limits{MaxSleep: 80 * time.Millisecond}})
	start := time.Now()
	if _, err := s.Run(ctx, "import time\ntime.sleep(0.05)", monty.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 50*time.Millisecond {
		t.Fatalf("sleep returned after %v", took)
	}
	// The second sleep would pass the budget.
	_, err := s.Run(ctx, "time.sleep(0.05)", monty.RunOptions{})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "TimeoutError" || !strings.Contains(exc.Message, "sleep limit") {
		t.Fatalf("err = %v", err)
	}
	if out, err := s.Run(ctx, "1 + 1", monty.RunOptions{}); err != nil || out != int64(2) {
		t.Fatalf("after sleep limit: %v, %v", out, err)
	}

	capped := newSession(t, monty.SessionOptions{OSPolicy: monty.OSPolicy{MaxSleep: 20 * time.Millisecond}})
	start = time.Now()
	if _, err := capped.Run(ctx, "import time\ntime.sleep(30)", monty.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("capped sleep took %v", took)
	}

	none := newSession(t, monty.SessionOptions{OSPolicy: monty.OSPolicy{Sleep: monty.SleepNone}})
	start = time.Now()
	if _, err := none.Run(ctx, "import time\ntime.sleep(30)", monty.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("disabled sleep took %v", took)
	}
}

func TestAsyncSleepsOverlap(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	start := time.Now()
	out, err := s.Run(context.Background(), `
import asyncio

async def main():
    await asyncio.gather(asyncio.sleep(0.3), asyncio.sleep(0.3), asyncio.sleep(0.3))
    return 'done'

asyncio.run(main())
`, monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "done" {
		t.Fatalf("out = %v", out)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 700*time.Millisecond {
		t.Fatalf("three overlapping 0.3s sleeps took %v", took)
	}
}

func TestSleepCancelled(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Run(ctx, "import time\ntime.sleep(5)", monty.RunOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	// Cancelling a wait on the host leaves the sandbox alive.
	if out, err := s.Run(context.Background(), "40 + 2", monty.RunOptions{}); err != nil || out != int64(42) {
		t.Fatalf("after cancelled sleep: %v, %v", out, err)
	}
}

func TestRejectedAnswerKeepsSessionUsable(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	_, err := s.Run(context.Background(), "bad()", monty.RunOptions{
		Functions: map[string]monty.Function{
			"bad": func(ctx context.Context, call *monty.Call) (any, error) {
				return monty.Date{Year: 2024, Month: 13, Day: 40}, nil
			},
		},
	})
	if err == nil {
		t.Fatal("an invalid date was accepted")
	}
	if s.Pending() != nil {
		t.Fatal("session is still paused")
	}
	if out, err := s.Run(context.Background(), "40 + 2", monty.RunOptions{}); err != nil || out != int64(42) {
		t.Fatalf("after rejected answer: %v, %v", out, err)
	}
}

func TestNewValueKinds(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{})
	out, err := s.Run(ctx, "z * 2", monty.RunOptions{Inputs: map[string]any{"z": complex(1, 2)}})
	if err != nil {
		t.Fatal(err)
	}
	if out != complex(2, 4) {
		t.Fatalf("complex = %#v", out)
	}
	out, err = s.Run(ctx, "from decimal import Decimal\nd + Decimal('1.20')", monty.RunOptions{Inputs: map[string]any{"d": monty.Decimal("2.5")}})
	if err != nil {
		t.Fatal(err)
	}
	if out != monty.Decimal("3.70") {
		t.Fatalf("decimal = %#v", out)
	}
	// One object referenced twice crosses once and decodes in both places.
	out, err = s.Run(ctx, "a = [1, 2]\n{'x': a, 'y': a, 'z': (a, a)}", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if len(m["x"].([]any)) != 2 || len(m["y"].([]any)) != 2 || len(m["z"].(monty.Tuple)) != 2 {
		t.Fatalf("shared = %#v", out)
	}
}

func TestScanAndExclusiveCreate(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{})
	mem := monty.NewMemFS(map[string]any{
		"a.txt":         "a",
		"sub/b.txt":     "b",
		"sub/deep/c.md": "c",
	})
	opts := monty.RunOptions{OS: monty.NewFS("/data", mem)}
	out, err := s.Run(ctx, `
import os
from pathlib import Path
walked = []
for root, dirs, files in os.walk('/data'):
    walked.append((root, sorted(dirs), sorted(files)))
[
    sorted(walked),
    sorted(str(p) for p in Path('/data').glob('*.txt')),
    sorted(str(p) for p in Path('/data').rglob('*.txt')),
    sorted(e.name for e in os.scandir('/data')),
]
`, opts)
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	walked := items[0].([]any)
	if len(walked) != 3 || walked[0].(monty.Tuple)[0] != "/data" {
		t.Fatalf("walk = %#v", walked)
	}
	if got := items[1].([]any); len(got) != 1 || got[0] != "/data/a.txt" {
		t.Fatalf("glob = %#v", got)
	}
	if got := items[2].([]any); len(got) != 2 || got[1] != "/data/sub/b.txt" {
		t.Fatalf("rglob = %#v", got)
	}
	if got := items[3].([]any); len(got) != 2 || got[0] != "a.txt" || got[1] != "sub" {
		t.Fatalf("scandir = %#v", got)
	}

	// Upstream 1.6 does not emit create_exclusive yet (open(..., 'x') is
	// refused inside the sandbox), so the handler is exercised directly.
	fsys := monty.NewFS("/data", mem)
	create := &monty.OSCall{Name: "create_exclusive", Path: "/data/new.txt", Mode: "w"}
	handle, err := fsys.HandleOSCall(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if fh := handle.(monty.FileHandle); fh.Path != "/data/new.txt" || fh.Mode != "w" {
		t.Fatalf("handle = %#v", handle)
	}
	if err := mem.WriteFile("new.txt", []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = fsys.HandleOSCall(ctx, create)
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "FileExistsError" {
		t.Fatalf("err = %v", err)
	}
	if data, _ := mem.ReadFile("new.txt"); string(data) != "fresh" {
		t.Fatalf("exclusive create replaced the file: %q", data)
	}
}

func TestHostModule(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{})
	handler := monty.OSFunc(func(ctx context.Context, call *monty.OSCall) (any, error) {
		if call.Name == "import" && call.Module == "acme" {
			return &monty.Module{Name: "acme", Attrs: map[string]any{"VERSION": "1.2", "LIMIT": 3}}, nil
		}
		return nil, monty.ErrNotHandled
	})
	out, err := s.Run(ctx, "import acme\n[acme.VERSION, acme.LIMIT + 1]", monty.RunOptions{OS: handler})
	if err != nil {
		t.Fatal(err)
	}
	if items := out.([]any); items[0] != "1.2" || items[1] != int64(4) {
		t.Fatalf("out = %#v", out)
	}
	_, err = s.Run(ctx, "import nope", monty.RunOptions{OS: handler})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "ModuleNotFoundError" {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreWithOptions(t *testing.T) {
	ctx := context.Background()
	opts := monty.SessionOptions{OSPolicy: monty.OSPolicy{
		Clock:     monty.ClockFixed,
		FixedTime: time.Date(2030, 5, 6, 7, 8, 9, 0, time.UTC),
	}}
	s := newSession(t, opts)
	if _, err := s.Run(ctx, "from datetime import datetime\ncounter = 41", monty.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err := s.Dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := rt.Restore(ctx, state, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	out, err := restored.Run(ctx, "[counter + 1, datetime.now().year]", monty.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if items := out.([]any); items[0] != int64(42) || items[1] != int64(2030) {
		t.Fatalf("out = %#v", out)
	}
}

func TestFunctionsShadowBuiltins(t *testing.T) {
	s := newSession(t, monty.SessionOptions{
		TypeCheck:      true,
		TypeCheckStubs: "def exec(*, command: str) -> str: ...\nasync def open(*, path: str) -> str: ...\n",
	})
	out, err := s.Run(context.Background(), "[exec(command='ls'), await open(path='a.txt'), len('abc')]", monty.RunOptions{
		Functions: map[string]monty.Function{
			"exec": func(ctx context.Context, call *monty.Call) (any, error) {
				return "ran " + call.Kwargs["command"].(string), nil
			},
		},
		AsyncFunctions: map[string]monty.Function{
			"open": func(ctx context.Context, call *monty.Call) (any, error) {
				return "opened " + call.Kwargs["path"].(string), nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if items := out.([]any); items[0] != "ran ls" || items[1] != "opened a.txt" || items[2] != int64(3) {
		t.Fatalf("out = %#v", out)
	}
}
