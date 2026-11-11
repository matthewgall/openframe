package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/matthewgall/openframe/internal/version"
)

func runVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	info := version.Current()
	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(info)
		return ExitOK
	}
	fmt.Print(info.String())
	return ExitOK
}
