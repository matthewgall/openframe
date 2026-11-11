// Command openframe is the unified OpenFrame CLI.
//
// Sub-commands:
//
//	openframe server   run the central management server
//	openframe frame    run the frame application
//	openframe pair     show or initialise frame pairing state
//	openframe doctor   diagnose the local system
//	openframe status   show local OpenFrame status
//	openframe version  print version information
//
// Version/commit/date are injected at build time (see Makefile).
package main

import (
	"os"

	"github.com/matthewgall/openframe/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:]))
}
