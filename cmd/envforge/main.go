// envforge — ephemeral per-PR environments on one local host, with a
// teardown guarantee that is proven by chaos, not promised.
//
// Usage:
//
//	envforge up     --name pr-123 --spec stack.json [--hold]
//	envforge down   --name pr-123
//	envforge list
//	envforge verify --name pr-123
//	envforge audit  [--name pr-123]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"envforge/internal/engine"
)

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "envforge:", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: envforge <up|down|list|verify|audit> [flags]")
		os.Exit(2)
	}
	eng := engine.New(engine.DefaultConfig())
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "up":
		fs := flag.NewFlagSet("up", flag.ExitOnError)
		name := fs.String("name", "", "environment name (default: spec name)")
		specPath := fs.String("spec", "", "path to spec JSON")
		hold := fs.Bool("hold", false, "stay alive after provisioning (chaos idle phase)")
		fs.Parse(args)
		if *specPath == "" {
			fatal(fmt.Errorf("--spec is required"))
		}
		res, err := eng.Provision(*name, *specPath, *hold)
		if err != nil {
			fatal(err)
		}
		out, _ := json.Marshal(res)
		fmt.Printf("env %s ready in %d ms, ports %v\n", res.Name, res.DurationMs, res.Ports)
		fmt.Println(string(out))

	case "down":
		fs := flag.NewFlagSet("down", flag.ExitOnError)
		name := fs.String("name", "", "environment name")
		fs.Parse(args)
		if *name == "" {
			fatal(fmt.Errorf("--name is required"))
		}
		if err := eng.Teardown(*name); err != nil {
			fatal(err)
		}
		fmt.Printf("env %s torn down\n", *name)

	case "list":
		recs, err := eng.List()
		if err != nil {
			fatal(err)
		}
		if len(recs) == 0 {
			fmt.Println("no live environments")
			return
		}
		for _, r := range recs {
			fmt.Printf("%-16s %-12s phase=%-14s ports=%v created=%s\n",
				r.Name, r.State, r.Phase, r.Ports, r.CreatedAt.Format("15:04:05"))
		}

	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		name := fs.String("name", "", "environment name")
		fs.Parse(args)
		if *name == "" {
			fatal(fmt.Errorf("--name is required"))
		}
		drift, err := eng.Verify(*name)
		if err != nil {
			fatal(err)
		}
		if len(drift) == 0 {
			fmt.Printf("env %s: no drift\n", *name)
			return
		}
		for _, d := range drift {
			fmt.Println("DRIFT:", d)
		}
		os.Exit(2)

	case "audit":
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		name := fs.String("name", "", "scope audit to one (torn-down) env")
		fs.Parse(args)
		leaks, err := eng.Audit(*name)
		if err != nil {
			fatal(err)
		}
		if len(leaks) == 0 {
			fmt.Println("AUDIT CLEAN: no orphaned processes, ports, or directories")
			return
		}
		for _, l := range leaks {
			fmt.Println("LEAK:", l)
		}
		os.Exit(2)

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}
