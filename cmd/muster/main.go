// Command muster is the entrypoint for the muster coordination bus.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: muster <serve|debug> [args]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		fmt.Fprintln(os.Stderr, "serve: not implemented yet")
		os.Exit(1)
	case "debug":
		fmt.Fprintln(os.Stderr, "debug: not implemented yet")
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "muster: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}
