// Command burndown is the overnight automation entry point invoked by launchd.
//
// This is the skeleton — it prints the version and exits. Subsequent commits
// add the real subcommands: run (one nightly cycle), digest, status, pause,
// resume.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jdfalk/overnight-burndown/internal/version"
)

func main() {
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *versionFlag || (flag.NArg() == 1 && flag.Arg(0) == "version") {
		fmt.Println(version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "burndown: subcommand not implemented yet")
	fmt.Fprintln(os.Stderr, "see PLAN.md for the build sequence")
	os.Exit(2)
}
