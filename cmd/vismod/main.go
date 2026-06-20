// Command vismod is the single binary for the visual content moderation
// pipeline. It runs both as a one-shot CLI (`vismod scan`) and a long-running
// worker (`vismod serve`) via subcommands.
package main

import (
	"fmt"
	"os"

	"github.com/matthupy/vismod/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
