package monty_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	monty "github.com/adrianliechti/go-monty"
)

func TestPauseDumpRestoreResume(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{})
	p, err := s.Start(ctx, "x = approve('deploy')\nx + '!'", monty.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Done() || p.Suspension.Kind != monty.SuspensionCall || p.Suspension.Call.Name != "approve" {
		t.Fatalf("progress = %+v", p)
	}
	// Persist the paused run and answer it in a fresh session.
	state, err := p.Suspension.Dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := rt.Restore(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	su := restored.Pending()
	if su == nil || su.Call.Name != "approve" || su.Call.Args[0] != "deploy" {
		t.Fatalf("pending = %+v", su)
	}
	p2, err := su.Return(ctx, "approved")
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Done() || p2.Value != "approved!" {
		t.Fatalf("result = %+v", p2)
	}
	// The original session's suspension is still answerable too.
	p3, err := p.Suspension.Raise(ctx, monty.Raise("PermissionError", "denied"))
	if err == nil || !strings.Contains(err.Error(), "PermissionError") {
		t.Fatalf("raise: %v %v", p3, err)
	}
	// Continue answers a pending call from RunOptions.
	restored2, err := rt.Restore(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer restored2.Close(ctx)
	out, err := restored2.Continue(ctx, monty.RunOptions{Functions: map[string]monty.Function{
		"approve": func(ctx context.Context, call *monty.Call) (any, error) { return "ok", nil },
	}})
	if err != nil || out != "ok!" {
		t.Fatalf("continue: %v %v", out, err)
	}
}

func TestSuspensionAnsweredOnce(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{})
	p, err := s.Start(ctx, "f()", monty.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Suspension.Decline(ctx); err == nil {
		t.Fatal("decline should raise NameError")
	}
	if _, err := p.Suspension.Return(ctx, 1); err == nil || !strings.Contains(err.Error(), "already answered") {
		t.Fatalf("second answer: %v", err)
	}
}

func TestAsyncFunctionsGather(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{})
	var inFlight, maxInFlight int32
	fetch := func(ctx context.Context, call *monty.Call) (any, error) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&maxInFlight)
			if n <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return "got " + call.Args[0].(string), nil
	}
	start := time.Now()
	out, err := s.Run(ctx, `
import asyncio
async def main():
    return await asyncio.gather(fetch('a'), fetch('b'), fetch('c'))
asyncio.run(main())
`, monty.RunOptions{AsyncFunctions: map[string]monty.Function{"fetch": fetch}})
	if err != nil {
		t.Fatal(err)
	}
	if items := out.([]any); len(items) != 3 || items[2] != "got c" {
		t.Fatalf("out = %#v", out)
	}
	if maxInFlight < 3 {
		t.Fatalf("calls did not overlap: max in flight %d", maxInFlight)
	}
	if elapsed := time.Since(start); elapsed > 80*time.Millisecond {
		t.Fatalf("gather took %v, expected overlap", elapsed)
	}
	// An async function used synchronously still works.
	out, err = s.Run(ctx, "import asyncio\nasync def one():\n    return await fetch('x')\nasyncio.run(one())", monty.RunOptions{AsyncFunctions: map[string]monty.Function{"fetch": fetch}})
	if err != nil || out != "got x" {
		t.Fatalf("single: %v %v", out, err)
	}
	// Errors propagate as exceptions.
	_, err = s.Run(ctx, "import asyncio\nasync def bad():\n    return await boom()\nasyncio.run(bad())", monty.RunOptions{AsyncFunctions: map[string]monty.Function{
		"boom": func(ctx context.Context, call *monty.Call) (any, error) {
			return nil, monty.Raise("ValueError", "nope")
		},
	}})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "ValueError" {
		t.Fatalf("err = %v", err)
	}
}

type account struct {
	owner   string
	balance float64
}

func TestHostObjects(t *testing.T) {
	ctx := context.Background()
	s := newSession(t, monty.SessionOptions{})
	class := &monty.HostClass{
		Name:  "Account",
		Attrs: map[string]any{"CURRENCY": "CHF"},
		Methods: map[string]monty.Method{
			"deposit": func(ctx context.Context, obj *monty.HostObject, call *monty.Call) (any, error) {
				acc := obj.Value.(*account)
				acc.balance += call.Args[0].(float64)
				return acc.balance, nil
			},
			"twin": func(ctx context.Context, obj *monty.HostObject, call *monty.Call) (any, error) {
				return monty.NewObject(obj.Class, &account{owner: "twin"}, map[string]any{"owner": "twin"}), nil
			},
		},
		Lazy: func(ctx context.Context, obj *monty.HostObject, name string) (any, error) {
			if name == "balance" {
				return obj.Value.(*account).balance, nil
			}
			return nil, monty.ErrUndefined
		},
	}
	class.Init = func(ctx context.Context, call *monty.Call) (*monty.HostObject, error) {
		acc := &account{owner: call.Args[0].(string)}
		return monty.NewObject(class, acc, map[string]any{"owner": acc.owner}), nil
	}
	acc := &account{owner: "anna", balance: 10}
	obj := monty.NewObject(class, acc, map[string]any{"owner": acc.owner})

	out, err := s.Run(ctx, `
acct.deposit(5.5)
b = Account('bob')
b.deposit(1.0)
[acct.owner, acct.balance, Account.CURRENCY, b.owner, hasattr(acct, 'missing'), acct.twin().owner, acct]
`, monty.RunOptions{Inputs: map[string]any{"acct": obj}, Values: map[string]any{"Account": class}})
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	if items[0] != "anna" || items[1] != 15.5 || items[2] != "CHF" || items[3] != "bob" || items[4] != false || items[5] != "twin" {
		t.Fatalf("items = %#v", items)
	}
	if items[6] != obj {
		t.Fatalf("returned object is not the original: %#v", items[6])
	}
	if acc.balance != 15.5 {
		t.Fatalf("host state not updated: %v", acc.balance)
	}
	// Forbidden construction and unknown methods.
	class.Init = nil
	_, err = s.Run(ctx, "Account('x')", monty.RunOptions{Values: map[string]any{"Account": class}})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "TypeError" {
		t.Fatalf("init: %v", err)
	}
	_, err = s.Run(ctx, "acct.withdraw(1)", monty.RunOptions{})
	if !errors.As(err, &exc) || exc.Type != "AttributeError" {
		t.Fatalf("unknown method: %v", err)
	}
}

func TestPool(t *testing.T) {
	ctx := context.Background()
	pool := rt.NewPool(ctx, monty.PoolOptions{Idle: 2, MaxRuns: 3})
	defer pool.Close(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for pool.Stats() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pool.Stats() != 2 {
		t.Fatalf("idle = %d", pool.Stats())
	}
	for i := 0; i < 5; i++ {
		start := time.Now()
		s, err := pool.Checkout(ctx, monty.SessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			t.Logf("checkout took %v", time.Since(start))
		}
		out, err := s.Run(ctx, "leaked = 'state'\n1", monty.RunOptions{})
		if err != nil || out != int64(1) {
			t.Fatalf("run %d: %v %v", i, out, err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// A recycled worker carries no state over.
		s2, err := pool.Checkout(ctx, monty.SessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s2.Run(ctx, "leaked", monty.RunOptions{}); err == nil {
			t.Fatal("state leaked across checkouts")
		}
		_ = s2.Close(ctx)
	}
}

func TestOverlayReadOnlyQuota(t *testing.T) {
	ctx := context.Background()
	base := fstest.MapFS{
		"keep.txt":        {Data: []byte("keep")},
		"edit.txt":        {Data: []byte("old")},
		"gone.txt":        {Data: []byte("bye")},
		"dir/inner.txt":   {Data: []byte("inner")},
		"dir/other.txt":   {Data: []byte("other")},
		"dir/sub/leaf.md": {Data: []byte("leaf")},
	}
	ov := monty.NewOverlay(base)
	quota := &monty.WriteQuota{Limit: 100}
	fsys := monty.NewFS("/w", ov)
	fsys.Quota = quota
	s := newSession(t, monty.SessionOptions{})
	out, err := s.Run(ctx, `
from pathlib import Path
w = Path('/w')
(w / 'edit.txt').write_text('new')
(w / 'gone.txt').unlink()
(w / 'dir' / 'sub' / 'leaf.md').unlink()
(w / 'dir' / 'sub').rmdir()
(w / 'dir' / 'sub').mkdir()
(w / 'dir' / 'sub' / 'fresh.txt').write_text('fresh')
(w / 'dir' / 'inner.txt').rename(w / 'dir' / 'moved.txt')
with open('/w/keep.txt', 'a') as f:
    f.write('!')
[sorted(p.name for p in w.iterdir()), sorted(p.name for p in (w / 'dir').iterdir()), sorted(p.name for p in (w / 'dir' / 'sub').iterdir()),
 (w / 'edit.txt').read_text(), (w / 'keep.txt').read_text(), (w / 'gone.txt').exists()]
`, monty.RunOptions{OS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	want := []string{"dir edit.txt keep.txt", "moved.txt other.txt sub", "fresh.txt"}
	for i, w := range want {
		var names []string
		for _, n := range items[i].([]any) {
			names = append(names, n.(string))
		}
		if got := strings.Join(names, " "); got != w {
			t.Errorf("listing %d = %q, want %q", i, got, w)
		}
	}
	if items[3] != "new" || items[4] != "keep!" || items[5] != false {
		t.Fatalf("contents = %#v", items[3:])
	}
	if string(base["edit.txt"].Data) != "old" || base["gone.txt"] == nil {
		t.Fatal("base was modified")
	}
	if del := ov.Deleted(); len(del) != 3 {
		t.Fatalf("deleted = %v", del)
	}
	if quota.Written() == 0 {
		t.Fatal("quota did not count writes")
	}
	// Quota exceeded.
	_, err = s.Run(ctx, "Path('/w/big.txt').write_text('x' * 200)", monty.RunOptions{OS: fsys})
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "OSError" || !strings.Contains(exc.Message, "quota") {
		t.Fatalf("quota: %v", err)
	}
	// ReadOnly hides the write capability.
	ro := monty.NewFS("/ro", monty.ReadOnly(monty.NewMemFS(map[string]any{"a": "1"})))
	_, err = s.Run(ctx, "Path('/ro/a').write_text('2')", monty.RunOptions{OS: ro})
	if !errors.As(err, &exc) || exc.Type != "PermissionError" {
		t.Fatalf("readonly: %v", err)
	}
}

func TestExceptionData(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	_, err := s.Run(context.Background(), "import json\njson.loads('{bad')", monty.RunOptions{})
	var exc *monty.Exception
	if !errors.As(err, &exc) {
		t.Fatal(err)
	}
	data, ok := exc.Data.(*monty.JSONErrorData)
	if !ok || data.Lineno != 1 || data.Doc == nil || *data.Doc != "{bad" {
		t.Fatalf("data = %#v", exc.Data)
	}
	_, err = s.Run(context.Background(), "b'\\xff'.decode()", monty.RunOptions{})
	if !errors.As(err, &exc) {
		t.Fatal(err)
	}
	if u, ok := exc.Data.(*monty.UnicodeErrorData); !ok || u.Encoding != "utf-8" {
		t.Fatalf("data = %#v", exc.Data)
	}
}

func TestStreamingPrints(t *testing.T) {
	s := newSession(t, monty.SessionOptions{})
	var seenBeforeCall int
	var out bytes.Buffer
	_, err := s.Run(context.Background(), "print('before')\nprobe()\nprint('after')", monty.RunOptions{
		Stdout: &out,
		Functions: map[string]monty.Function{
			"probe": func(ctx context.Context, call *monty.Call) (any, error) {
				seenBeforeCall = out.Len()
				return nil, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenBeforeCall == 0 {
		t.Fatal("print output was not delivered before the host call ran")
	}
	if out.String() != "before\nafter\n" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestStructValues(t *testing.T) {
	type Point struct {
		X      int     `json:"x"`
		Y      float64 `json:"y"`
		Tag    string  `json:"tag,omitempty"`
		hidden int
	}
	s := newSession(t, monty.SessionOptions{})
	out, err := s.Run(context.Background(), "[p['x'], p['y'], 'tag' in p]", monty.RunOptions{Inputs: map[string]any{"p": Point{X: 1, Y: 2.5}}})
	if err != nil {
		t.Fatal(err)
	}
	if items := out.([]any); items[0] != int64(1) || items[1] != 2.5 || items[2] != false {
		t.Fatalf("out = %#v", out)
	}
}
