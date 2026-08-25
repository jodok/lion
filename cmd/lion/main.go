// Command lion is a task-first LinkedIn CLI.
package main

import (
	"os"

	"github.com/jodok/lion/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
