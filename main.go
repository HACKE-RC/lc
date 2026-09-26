// Command lc lists and browses local coding-agent sessions for a repository.
package main

import (
	"os"

	"github.com/HACKE-RC/lc/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
