// vismod-eval is the evaluation harness CLI: it measures a configured
// vismod against a corpus the operator supplies. This binary is separate
// from vismod on purpose — the harness never moderates anything itself,
// and nothing here is on the serving path.
//
// Only `corpus convert` exists so far. The run driver, the collector and
// the scorer land in their own changes.
package main

import "os"

func main() {
	if err := newRootCmd(os.Stdout).Execute(); err != nil {
		// cobra has already written the error to stderr.
		os.Exit(1)
	}
}
