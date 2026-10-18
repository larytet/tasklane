// Command notice writes NOTICE and SBOM.txt for the modules linked into the tasklane binary.
package main

import (
	"fmt"
	"os"

	"github.com/larytet/tasklane/internal/licenses"
)

func main() {
	mods, err := licenses.Linked(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile("NOTICE", []byte(licenses.Notice(mods)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) > 1 {
		os.WriteFile(os.Args[1], []byte(licenses.SBOM(mods)), 0o644)
	}
	fmt.Printf("NOTICE written for %d modules\n", len(mods))
}
