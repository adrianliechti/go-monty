// Command monty runs a Python script in the Monty sandbox.
//
//	monty script.py
//	monty -e "print(1 + 1)"
//	echo "x = 1" | monty -
//	monty -mount /data=./data -mount /ref=./docs:ro -env LANG=en script.py
//	monty -mount /work=./repo:overlay -typecheck -stubs tools.pyi agent.py
//
// The result of the script's last expression statement is printed to stdout;
// print() output goes to stdout as it happens, tracebacks to stderr.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	monty "github.com/adrianliechti/go-monty"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	os.Exit(run())
}

func run() int {
	var (
		code       = flag.String("e", "", "code to run instead of a script file")
		mounts     multiFlag
		envs       multiFlag
		inheritEnv = flag.Bool("inherit-env", false, "expose this process's environment to the script")
		timeout    = flag.Duration("timeout", 30*time.Second, "execution time limit (0 = none)")
		memory     = flag.String("memory", "256M", "memory limit, e.g. 64M or 1G (0 = none)")
		typeCheck  = flag.Bool("typecheck", false, "type-check the script before running it")
		stubsFile  = flag.String("stubs", "", "stub file (.pyi) declaring host functions for -typecheck")
		scriptName = flag.String("script-name", "", "file name shown in tracebacks")
		cacheDir   = flag.String("cache", defaultCacheDir(), "compilation cache directory (empty = none)")
		asJSON     = flag.Bool("json", false, "print the result as JSON")
		quiet      = flag.Bool("quiet", false, "do not print the result, only print() output")
	)
	flag.Var(&mounts, "mount", "mount a host directory: /virtual=hostdir[:ro|:overlay] (repeatable)")
	flag.Var(&envs, "env", "environment variable KEY=VALUE visible to the script (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: monty [flags] script.py | -e code | -\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	source, name, err := readSource(*code, flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "monty:", err)
		return 2
	}
	if *scriptName != "" {
		name = *scriptName
	}

	ctx := context.Background()
	var opts []monty.RuntimeOption
	if *cacheDir != "" {
		opts = append(opts, monty.WithCacheDir(*cacheDir))
	}
	rt, err := monty.NewRuntime(ctx, opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "monty:", err)
		return 3
	}
	defer rt.Close(ctx)

	limits := &monty.Limits{MaxDuration: *timeout}
	if limits.MaxMemory, err = parseSize(*memory); err != nil {
		fmt.Fprintln(os.Stderr, "monty: -memory:", err)
		return 2
	}
	sessionOpts := monty.SessionOptions{ScriptName: name, Limits: limits, TypeCheck: *typeCheck, TypeCheckFormat: monty.TypeCheckConcise}
	if *stubsFile != "" {
		stubs, err := os.ReadFile(*stubsFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "monty:", err)
			return 2
		}
		sessionOpts.TypeCheckStubs = string(stubs)
	}

	handlers, closers, err := buildHandlers(mounts, envs, *inheritEnv)
	for _, c := range closers {
		defer c()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "monty:", err)
		return 2
	}

	session, err := rt.NewSession(ctx, sessionOpts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "monty:", err)
		return 3
	}
	defer session.Close(ctx)

	result, err := session.Run(ctx, source, monty.RunOptions{OS: monty.Handlers(handlers...)})
	var exc *monty.Exception
	var typing *monty.TypingError
	switch {
	case errors.As(err, &typing):
		fmt.Fprintln(os.Stderr, typing.Diagnostics)
		return 1
	case errors.As(err, &exc):
		fmt.Fprintln(os.Stderr, exc.Traceback)
		return 1
	case err != nil:
		fmt.Fprintln(os.Stderr, "monty:", err)
		return 3
	}
	if *quiet || result == nil {
		return 0
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(os.Stdout, "%v\n", result)
		}
		return 0
	}
	fmt.Println(format(result))
	return 0
}

func readSource(code, arg string) (source, name string, err error) {
	switch {
	case code != "":
		return code, "<command>", nil
	case arg == "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), "<stdin>", err
	case arg != "":
		b, err := os.ReadFile(arg)
		return string(b), filepath.Base(arg), err
	}
	return "", "", errors.New("no script given (file, -e code, or - for stdin)")
}

func defaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "go-monty")
}

func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := uint64(1)
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

func buildHandlers(mounts, envs []string, inheritEnv bool) ([]monty.OSHandler, []func(), error) {
	var handlers []monty.OSHandler
	var closers []func()
	for _, m := range mounts {
		virtual, host, ok := strings.Cut(m, "=")
		if !ok || !strings.HasPrefix(virtual, "/") {
			return handlers, closers, fmt.Errorf("-mount %q: expected /virtual=hostdir[:ro|:overlay]", m)
		}
		mode := ""
		if i := strings.LastIndex(host, ":"); i > 0 && (host[i+1:] == "ro" || host[i+1:] == "overlay") {
			host, mode = host[:i], host[i+1:]
		}
		root, err := monty.DirFS(host)
		if err != nil {
			return handlers, closers, fmt.Errorf("-mount %q: %w", m, err)
		}
		closers = append(closers, func() { _ = root.Close() })
		var fsys fs.FS = root
		switch mode {
		case "ro":
			fsys = monty.ReadOnly(root)
		case "overlay":
			fsys = monty.NewOverlay(root)
		}
		handlers = append(handlers, monty.NewFS(virtual, fsys))
	}
	vars := map[string]string{}
	if inheritEnv {
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				vars[k] = v
			}
		}
	}
	for _, kv := range envs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return handlers, closers, fmt.Errorf("-env %q: expected KEY=VALUE", kv)
		}
		vars[k] = v
	}
	handlers = append(handlers, monty.Env(vars), monty.Clock(time.Now))
	return handlers, closers, nil
}

// format renders a result the way Python's repr would, roughly.
func format(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return strconv.Quote(x)
	case []any:
		return "[" + join(x) + "]"
	case monty.Tuple:
		return "(" + join(x) + ")"
	case monty.Set:
		return "{" + join(x) + "}"
	case map[string]any:
		parts := make([]string, 0, len(x))
		for k, val := range x {
			parts = append(parts, strconv.Quote(k)+": "+format(val))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case monty.Dict:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, format(item.Key)+": "+format(item.Value))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func join(items []any) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = format(item)
	}
	return strings.Join(parts, ", ")
}
