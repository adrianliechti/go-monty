package tools_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	monty "github.com/adrianliechti/go-monty"
	"github.com/adrianliechti/go-monty/tools"
)

func schemaRegistry(got *map[string]any) *tools.Registry {
	reg := tools.New()
	reg.AddSchema(tools.Schema{
		Name:        "github-search_issues",
		Description: "Search issues in a repository.",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "The search\nquery."},
				"state": map[string]any{"type": "string", "enum": []any{"open", "closed"}},
				"limit": map[string]any{"type": []any{"integer", "null"}},
				"from":  map[string]any{"type": "string"},
				"filter": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"author": map[string]any{"$ref": "#/$defs/User"},
					},
					"required": []any{"labels"},
				},
				"extra": map[string]any{"type": "object"},
			},
			"required": []any{"query"},
			"$defs": map[string]any{
				"User": map[string]any{
					"type":       "object",
					"properties": map[string]any{"login": map[string]any{"type": "string"}, "manager": map[string]any{"$ref": "#/$defs/User"}},
					"required":   []any{"login"},
				},
			},
		},
		Output: map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"number": map[string]any{"type": "integer", "description": "Issue number,\nunique per repository."}, "title": map[string]any{"type": "string"}},
				"required":   []any{"number", "title"},
			},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		*got = args
		return []any{map[string]any{"number": 7, "title": "flaky test"}}, nil
	})
	reg.AddSchema(tools.Schema{Name: "ping", Async: true}, func(ctx context.Context, args map[string]any) (any, error) {
		return "pong", nil
	})
	return reg
}

func TestSchemaStubs(t *testing.T) {
	var got map[string]any
	reg := schemaRegistry(&got)
	stubs := reg.Stubs()
	for _, want := range []string{
		"def github_search_issues(*, query: str, extra: dict[str, Any] | None = None, " +
			"filter: GithubSearchIssuesFilter | None = None, from_: str | None = None, limit: int | None = None, " +
			"state: Literal['open', 'closed'] | None = None) -> list[GithubSearchIssuesResultItem]: ...",
		"class GithubSearchIssuesFilter(TypedDict):\n    author: NotRequired[User]\n    labels: list[str]\n",
		"class User(TypedDict):\n    login: str\n    manager: NotRequired[Any]\n",
		"class GithubSearchIssuesResultItem(TypedDict):\n    number: int  # Issue number, unique per repository.\n    title: str\n",
		"async def ping() -> Any: ...",
	} {
		if !strings.Contains(stubs, want) {
			t.Errorf("stubs lack %q:\n%s", want, stubs)
		}
	}

	if only := reg.Signatures("ping", "nope"); only != "async def ping() -> Any: ...\n" {
		t.Errorf("Signatures = %q", only)
	}

	prompt := reg.Describe("github_search_issues")
	if !strings.Contains(prompt, "\"\"\"Search issues in a repository.\n\n    query: The search query.\"\"\"") {
		t.Errorf("docstring:\n%s", prompt)
	}
	if strings.Contains(prompt, "ping") {
		t.Errorf("Describe included an unrequested tool:\n%s", prompt)
	}
	if only := reg.Describe("ping"); strings.Contains(only, "TypedDict") {
		t.Errorf("Describe included unused types:\n%s", only)
	}
}

func TestSchemaCalls(t *testing.T) {
	ctx := context.Background()
	var got map[string]any
	reg := schemaRegistry(&got)
	s, err := rt.NewSession(ctx, monty.SessionOptions{TypeCheck: true, TypeCheckStubs: reg.Stubs()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)

	out, err := s.Run(ctx, `
import asyncio
issues = github_search_issues(query='flaky', state='open', limit=None, from_='2026-01-01',
                              filter={'labels': ['bug'], 'author': {'login': 'anna'}})
[issues[0]['number'], issues[0]['title'], asyncio.run(ping())]
`, reg.Options(monty.RunOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if items := out.([]any); items[0] != int64(7) || items[1] != "flaky test" || items[2] != "pong" {
		t.Fatalf("out = %#v", out)
	}
	want := map[string]any{
		"query": "flaky", "state": "open", "from": "2026-01-01",
		"filter": map[string]any{"labels": []any{"bug"}, "author": map[string]any{"login": "anna"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v", got)
	}

	// The checker rejects mistakes before anything runs.
	got = nil
	for _, code := range []string{
		"github_search_issues('flaky')",
		"github_search_issues(query=3)",
		"github_search_issues(query='x', state='merged')",
		"github_search_issues(query='x', filter={'author': {'login': 'anna'}})",
		"github_search_issues(query='x')[0]['nope']",
	} {
		_, err := s.Run(ctx, code, reg.Options(monty.RunOptions{}))
		var terr *monty.TypingError
		if !errors.As(err, &terr) {
			t.Errorf("%s: err = %v", code, err)
		}
	}
	if got != nil {
		t.Fatalf("a rejected call ran: %#v", got)
	}

	// Without the checker the binding itself refuses them.
	plain, err := rt.NewSession(ctx, monty.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close(ctx)
	for code, want := range map[string]string{
		"github_search_issues('flaky')":              "keyword arguments only",
		"github_search_issues()":                     "missing required keyword argument 'query'",
		"github_search_issues(query='x', nope=1)":    "unexpected keyword argument 'nope'",
		"github_search_issues(query='x', extra=b'')": "not JSON-compatible",
	} {
		_, err := plain.Run(ctx, code, reg.Options(monty.RunOptions{}))
		var exc *monty.Exception
		if !errors.As(err, &exc) || exc.Type != "TypeError" || !strings.Contains(exc.Message, want) {
			t.Errorf("%s: err = %v", code, err)
		}
	}
}

func TestIdentifier(t *testing.T) {
	for in, want := range map[string]string{
		"read": "read", "github-search.issues": "github_search_issues", "9lives": "_9lives",
		"class": "class_", "": "_", "naïve": "na_ve", "type": "type", "-A": "_A",
	} {
		if got := tools.Identifier(in); got != want {
			t.Errorf("Identifier(%q) = %q, want %q", in, got, want)
		}
	}
}
