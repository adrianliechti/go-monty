package tools_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	monty "github.com/adrianliechti/go-monty"
	"github.com/adrianliechti/go-monty/tools"
)

type Customer struct {
	Name    string   `json:"name"`
	Balance float64  `json:"balance"`
	Tags    []string `json:"tags"`
	secret  string
}

var rt *monty.Runtime

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if rt, err = monty.NewRuntime(ctx); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = rt.Close(ctx)
	os.Exit(code)
}

func TestRegistry(t *testing.T) {
	ctx := context.Background()
	reg := tools.New()
	reg.Add("customers", "All customers.", func(country string, limit *int) []Customer {
		out := []Customer{{Name: "anna", Balance: 12.5, Tags: []string{"vip"}}, {Name: "bob"}}
		if limit != nil && *limit < len(out) {
			out = out[:*limit]
		}
		return out
	}, "country", "limit")
	reg.Add("total", "Sum of balances.", func(ctx context.Context, cs []Customer) (float64, error) {
		sum := 0.0
		for _, c := range cs {
			sum += c.Balance
		}
		return sum, nil
	}, "customers")
	reg.Add("fail", "Always fails.", func() error { return monty.Raise("ValueError", "bad") })
	reg.Add("when", "A time.", func(t time.Time, d time.Duration) string { return t.Add(d).Format(time.RFC3339) }, "at", "plus")
	reg.AddAsync("slow", "Slow lookup.", func(key string) string { time.Sleep(20 * time.Millisecond); return key + "!" }, "key")

	stubs := reg.Stubs()
	for _, want := range []string{
		"class Customer(TypedDict):", "    name: str", "    balance: float", "    tags: list[str]",
		"def customers(country: str, limit: int | None = None) -> list[Customer]: ...",
		"def total(customers: list[Customer]) -> float: ...",
		"def fail() -> None: ...",
		"def when(at: datetime.datetime, plus: datetime.timedelta) -> str: ...",
		"async def slow(key: str) -> str: ...",
	} {
		if !strings.Contains(stubs, want) {
			t.Errorf("stubs missing %q:\n%s", want, stubs)
		}
	}
	if !strings.Contains(reg.Prompt(), `"""All customers."""`) {
		t.Error("prompt lacks docstring")
	}

	s, err := rt.NewSession(ctx, monty.SessionOptions{TypeCheck: true, TypeCheckStubs: stubs, TypeCheckFormat: monty.TypeCheckConcise})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	out, err := s.Run(ctx, `
import asyncio
from datetime import datetime, timedelta
cs = customers('CH', limit=1)
try:
    fail()
except ValueError as e:
    caught = str(e)
async def main():
    return await asyncio.gather(slow('a'), slow('b'))
[cs[0]['name'], cs[0]['tags'], total(cs), total(customers('CH')), caught, when(datetime(2024, 1, 1), timedelta(days=1)), asyncio.run(main())]
`, reg.Options(monty.RunOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	items := out.([]any)
	if items[0] != "anna" || items[2] != 12.5 || items[3] != 12.5 || items[4] != "bad" || items[5] != "2024-01-02T00:00:00Z" {
		t.Fatalf("items = %#v", items)
	}
	if tags := items[1].([]any); len(tags) != 1 || tags[0] != "vip" {
		t.Fatalf("tags = %#v", items[1])
	}
	if got := items[6].([]any); got[0] != "a!" || got[1] != "b!" {
		t.Fatalf("async = %#v", items[6])
	}

	// Binding errors become TypeErrors, and the checker catches them first.
	_, err = s.Run(ctx, "customers(1)", reg.Options(monty.RunOptions{}))
	var terr *monty.TypingError
	if !errors.As(err, &terr) {
		t.Fatalf("expected typing error, got %v", err)
	}
	_, err = s.Run(ctx, "customers(1)", reg.Options(monty.RunOptions{SkipTypeCheck: true}))
	var exc *monty.Exception
	if !errors.As(err, &exc) || exc.Type != "TypeError" || !strings.Contains(exc.Message, "country") {
		t.Fatalf("expected TypeError, got %v", err)
	}
	_, err = s.Run(ctx, "customers(country='CH', nope=1)", reg.Options(monty.RunOptions{SkipTypeCheck: true}))
	if !errors.As(err, &exc) || exc.Type != "TypeError" {
		t.Fatalf("expected TypeError, got %v", err)
	}
}
