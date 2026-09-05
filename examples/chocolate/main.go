// Command chocolate runs the README example from upstream Monty: model-written
// code calls a Go function and prints a result.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	monty "github.com/adrianliechti/go-monty"
)

const code = `
kcal = nutrition('chocolate bar')['kcal']
hours = kcal * 4184 / (bulb_watts * 3600)
print(f'a chocolate bar powers a {bulb_watts} W bulb for {hours:.1f} hours')
{'kcal': kcal, 'hours': round(hours, 1)}
`

func main() {
	ctx := context.Background()

	cacheDir := filepath.Join(os.TempDir(), "go-monty-cache")
	start := time.Now()
	rt, err := monty.NewRuntime(ctx, monty.WithCacheDir(cacheDir))
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)
	fmt.Printf("monty %s ready in %v\n", rt.Version(), time.Since(start).Round(time.Millisecond))

	session, err := rt.NewSession(ctx, monty.SessionOptions{
		Limits: &monty.Limits{MaxDuration: time.Second, MaxMemory: 64 << 20},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close(ctx)

	start = time.Now()
	result, err := session.Run(ctx, code, monty.RunOptions{
		Inputs: map[string]any{"bulb_watts": 10},
		Functions: map[string]monty.Function{
			"nutrition": func(ctx context.Context, call *monty.Call) (any, error) {
				food, _ := call.Args[0].(string)
				if food != "chocolate bar" {
					return nil, monty.Raise("KeyError", "unknown food %q", food)
				}
				return map[string]any{"kcal": 230}, nil
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("result %v in %v\n", result, time.Since(start).Round(time.Microsecond))
}
