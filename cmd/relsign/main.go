// Command relsign makes and uses the release signing key (INST-05):
//
//	relsign keygen DIR       writes DIR/release.key (private, keep it secret) and DIR/release.pub (publish it)
//	relsign sign KEY DIR     signs DIR/SHA256SUMS, writing DIR/SHA256SUMS.sig
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/larytet/tasklane/internal/release"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: relsign keygen DIR | relsign sign KEYFILE DIR")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, err := release.GenerateKey()
		if err != nil {
			fatal(err)
		}
		if err := os.MkdirAll(os.Args[2], 0o700); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(os.Args[2], "release.key"), priv, 0o600); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(os.Args[2], "release.pub"), pub, 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("wrote release.key (secret) and release.pub")
	case "sign":
		if len(os.Args) != 4 {
			fatal(fmt.Errorf("usage: relsign sign KEYFILE DIR"))
		}
		key, err := os.ReadFile(os.Args[2])
		if err != nil {
			fatal(err)
		}
		if err := release.Sign(os.Args[3], key); err != nil {
			fatal(err)
		}
		fmt.Println("signed " + filepath.Join(os.Args[3], release.SumsFile))
	default:
		fatal(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "relsign:", err)
	os.Exit(1)
}
