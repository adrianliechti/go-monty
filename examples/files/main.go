// Command files mounts a real host directory read-write at /data and an
// in-memory scratch space at /tmp, then lets a script work on the files.
//
//	go run ./examples/files [dir]    # default: examples/files/data
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

const script = `
from pathlib import Path
import json

data = Path('/data')
files = sorted(p.name for p in data.iterdir() if p.is_file())
print('files:', files)

rows = [line.split(',') for line in (data / 'people.csv').read_text().splitlines() if line]
ages = {name: int(age) for name, age in rows}

# Work in scratch space first, then publish one result file next to the data.
scratch = Path('/tmp/report.json')
scratch.write_text(json.dumps({'count': len(ages), 'average_age': sum(ages.values()) / len(ages)}))
(data / 'report.json').write_text(scratch.read_text())

with open('/data/notes.md', 'a') as f:
    f.write('\nReport written to report.json\n')

{'report': json.loads(scratch.read_text()), 'notes_lines': len((data / 'notes.md').read_text().splitlines())}
`

func main() {
	ctx := context.Background()
	dir := filepath.Join("examples", "files", "data")
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}

	rt, err := monty.NewRuntime(ctx, monty.WithCacheDir(filepath.Join(os.TempDir(), "go-monty-cache")))
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)

	hostDir, err := monty.DirFS(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer hostDir.Close()

	session, err := rt.NewSession(ctx, monty.SessionOptions{
		Limits: &monty.Limits{MaxDuration: time.Second, MaxMemory: 64 << 20},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close(ctx)

	result, err := session.Run(ctx, script, monty.RunOptions{
		OS: monty.Handlers(
			monty.NewFS("/data", hostDir),
			monty.NewFS("/tmp", monty.NewMemFS(nil)),
			monty.Clock(time.Now),
		),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("result:", result)
}
