// vismod is the one binary for both modes: one-shot CLI (scan) and
// long-running containerized worker (serve).
package main

import "github.com/vismod/vismod/internal/cli"

func main() { cli.Execute() }
