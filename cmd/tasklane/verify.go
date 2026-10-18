package main

import (
	"fmt"
	"os"

	"github.com/larytet/tasklane/internal/release"
)

// verifyRelease prints what was checked and returns the exit code: 0 only when the release verifies.
func verifyRelease(pubFile, dir string) int {
	pub, err := os.ReadFile(pubFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read the public key:", err)
		return 2
	}
	rep, err := release.Verify(dir, pub)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, a := range rep.Archives {
		fmt.Printf("  %s  sha256 %s  version %s  %s  %d modules\n", a.Name, a.SHA256[:16], a.Version, a.Arch, a.Modules)
	}
	if !rep.OK() {
		for _, p := range rep.Problems {
			fmt.Println("PROBLEM: " + p)
		}
		fmt.Printf("%d problem(s): do not install this release\n", len(rep.Problems))
		return 1
	}
	fmt.Printf("verified: signed by key %s; %d archive(s) match the signed checksums, and each binary matches its SBOM and NOTICE\n", rep.KeyID, len(rep.Archives))
	return 0
}
