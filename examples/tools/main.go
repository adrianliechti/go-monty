// Command tools shows "code mode" with the tools package: ordinary Go
// functions become sandbox tools, their signatures become the stub the type
// checker enforces and the text the model is prompted with, and a
// model-style script chains them. Async tools run concurrently under
// asyncio.gather.
//
//	go run ./examples/tools              # run the built-in script
//	go run ./examples/tools script.py    # run your own script against the tools
//	go run ./examples/tools --prompt     # print the tool description for an LLM prompt
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	monty "github.com/adrianliechti/go-monty"
	"github.com/adrianliechti/go-monty/tools"
)

// Customer is what list_customers returns; it becomes a TypedDict in the
// stub, so the checker knows every field.
type Customer struct {
	Name     string  `json:"name"`
	Email    string  `json:"email"`
	City     string  `json:"city"`
	Country  string  `json:"country"`
	Currency string  `json:"currency"`
	Balance  float64 `json:"balance"`
}

var customers = []Customer{
	{"Anna Keller", "anna@example.ch", "Zürich", "CH", "CHF", 1250.50},
	{"Luca Bianchi", "luca@example.ch", "Lugano", "CH", "CHF", -80.00},
	{"Marie Dupont", "marie@example.fr", "Lyon", "FR", "EUR", 410.00},
	{"Jonas Weber", "jonas@example.de", "Berlin", "DE", "EUR", 0.00},
}

var ratesToEUR = map[string]float64{"EUR": 1, "CHF": 1.06, "USD": 0.92}

var weather = map[string]float64{"Zürich": 14.5, "Lugano": 21.0, "Lyon": 18.2, "Berlin": 11.0}

func newRegistry(sent *[]string) *tools.Registry {
	reg := tools.New()
	reg.Add("list_customers", "Customers, optionally filtered by ISO country code.",
		func(country *string) []Customer {
			var out []Customer
			for _, c := range customers {
				if country == nil || c.Country == *country {
					out = append(out, c)
				}
			}
			return out
		}, "country")
	reg.Add("convert_currency", "Converts an amount between currencies at today's rates. Raises ValueError for an unknown currency.",
		func(amount float64, from, to string) (float64, error) {
			fromRate, ok1 := ratesToEUR[from]
			toRate, ok2 := ratesToEUR[to]
			if !ok1 {
				return 0, monty.Raise("ValueError", "unknown currency: %s", from)
			}
			if !ok2 {
				return 0, monty.Raise("ValueError", "unknown currency: %s", to)
			}
			return amount * fromRate / toRate, nil
		}, "amount", "from_currency", "to_currency")
	// Async: several weather lookups in one asyncio.gather overlap.
	reg.AddAsync("get_temperature", "Current temperature in °C for a city. Raises KeyError if the city is unknown.",
		func(ctx context.Context, city string) (float64, error) {
			time.Sleep(100 * time.Millisecond) // a slow API
			temp, ok := weather[city]
			if !ok {
				return 0, monty.Raise("KeyError", "%s", city)
			}
			return temp, nil
		}, "city")
	reg.Add("send_email", "Sends an email. Returns True when accepted for delivery.",
		func(to, subject, body string) bool {
			*sent = append(*sent, fmt.Sprintf("to=%s subject=%q body=%q", to, subject, body))
			return true
		}, "to", "subject", "body")
	reg.Add("now", "The current time in UTC.", func() time.Time { return time.Now().UTC() })
	return reg
}

// The kind of script a model writes when handed Registry.Prompt().
const modelScript = `
import asyncio
import json

customers = list_customers(country='CH')

async def temperatures():
    return await asyncio.gather(*(get_temperature(c['city']) for c in customers))

temps = asyncio.run(temperatures())

report = []
emails_sent = 0
for c, temp in zip(customers, temps):
    eur = convert_currency(c['balance'], c['currency'], 'EUR')
    if eur < 0:
        send_email(
            c['email'],
            'Outstanding balance',
            f"Hi {c['name'].split()[0]}, your account is {abs(eur):.2f} EUR overdrawn.",
        )
        emails_sent += 1
    report.append({'name': c['name'], 'eur': round(eur, 2), 'temp_c': temp})

print(json.dumps(report, indent=2))
{'customers': len(report), 'emails_sent': emails_sent, 'generated': now().isoformat()}
`

// A script with a mistake the type checker catches before anything runs.
const buggyScript = `
total = 0
for c in list_customers():
    total += convert_currency(c['balance'], c['currency'])   # missing to_currency
total
`

func main() {
	ctx := context.Background()

	var sent []string
	reg := newRegistry(&sent)

	if len(os.Args) > 1 && os.Args[1] == "--prompt" {
		fmt.Print(reg.Prompt())
		return
	}
	script := modelScript
	if len(os.Args) > 1 {
		b, err := os.ReadFile(os.Args[1])
		if err != nil {
			log.Fatal(err)
		}
		script = string(b)
	}

	rt, err := monty.NewRuntime(ctx, monty.WithCacheDir(filepath.Join(os.TempDir(), "go-monty-cache")))
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)

	newSession := func() *monty.Session {
		s, err := rt.NewSession(ctx, monty.SessionOptions{
			ScriptName:      "agent.py",
			TypeCheck:       true,
			TypeCheckStubs:  reg.Stubs(),
			TypeCheckFormat: monty.TypeCheckConcise,
			Limits:          &monty.Limits{MaxDuration: 2 * time.Second, MaxMemory: 64 << 20, MaxSuspensions: 200},
		})
		if err != nil {
			log.Fatal(err)
		}
		return s
	}

	fmt.Println("== stubs generated from the Go functions ==")
	fmt.Print(reg.Stubs())

	fmt.Println("\n== running the script ==")
	session := newSession()
	start := time.Now()
	result, err := session.Run(ctx, script, reg.Options(monty.RunOptions{}))
	session.Close(ctx)
	report(result, err)
	fmt.Printf("(%v; two 100 ms weather lookups ran concurrently)\n", time.Since(start).Round(time.Millisecond))
	for _, s := range sent {
		fmt.Println("email:", s)
	}

	if len(os.Args) == 1 {
		fmt.Println("\n== a script with a wrong call, rejected before it runs ==")
		session = newSession()
		result, err = session.Run(ctx, buggyScript, reg.Options(monty.RunOptions{}))
		session.Close(ctx)
		report(result, err)
	}
}

func report(result any, err error) {
	var exc *monty.Exception
	var typing *monty.TypingError
	switch {
	case errors.As(err, &typing):
		fmt.Println("type check failed (feed this back to the model):")
		fmt.Println(typing.Diagnostics)
	case errors.As(err, &exc):
		fmt.Println("python raised:")
		fmt.Println(exc.Traceback)
	case err != nil:
		fmt.Println("sandbox error:", err)
	default:
		fmt.Printf("result: %v\n", result)
	}
}
