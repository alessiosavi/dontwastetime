// Command dwt is the DontWasteTime email-triage daemon and its one-shot tools.
// It is the only place in the module that reads the wall clock.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: dwt <command> [flags]

commands:
  version    print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "dwt %s\n", version)
		return 0
	default:
		fmt.Fprintf(stderr, "dwt: unknown command %q\n%s", args[0], usage)
		return 2
	}
}
