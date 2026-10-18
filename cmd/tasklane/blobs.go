package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/domain"
)

// blobsCmd implements `tasklane blobs migrate [--verify] | verify | rewrap`.
func blobsCmd(ctx context.Context, cfg config.Config) {
	sub := ""
	if len(os.Args) > 2 {
		sub = os.Args[2]
	}
	st := mustOpen(ctx, cfg)
	defer st.Close()
	var shas []string
	rows, err := st.Pool.Query(ctx, `SELECT sha256 FROM blob ORDER BY sha256`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		shas = append(shas, s)
	}
	rows.Close()
	switch sub {
	case "migrate":
		primary, secondary, err := blob.OpenPair(cfg)
		if err != nil {
			log.Fatal(err)
		}
		if secondary == nil {
			log.Fatal("set blob_fallback (and blob_fallback_dir for fs): it names the old backend to copy from")
		}
		verify := false
		for _, a := range os.Args[3:] {
			verify = verify || a == "--verify"
		}
		rep := blob.Migrate(ctx, secondary, primary, shas, blob.MigrateOpts{Verify: verify, Progress: func(d, t int) {
			if d%100 == 0 || d == t {
				fmt.Fprintf(os.Stderr, "%d of %d\n", d, t)
			}
		}})
		fmt.Printf("%d objects: %d copied, %d already there, %d failed in %s\n", rep.Total, rep.Copied, rep.Skipped, len(rep.Failed), rep.Duration.Round(1e6))
		for _, f := range rep.Failed {
			fmt.Println("  " + f)
		}
		if len(rep.Failed) > 0 {
			os.Exit(1)
		}
	case "verify": // every object the database refers to must be in the configured store and hash to its name
		store, err := blob.NewFromConfig(cfg)
		if err != nil {
			log.Fatal(err)
		}
		bad := 0
		for _, sha := range shas {
			o, err := store.Open(sha)
			if err == nil {
				h := sha256.New()
				_, err = io.Copy(h, o)
				o.Close()
				if err == nil && hex.EncodeToString(h.Sum(nil)) != sha {
					err = fmt.Errorf("its bytes do not hash to its name")
				}
			}
			if err != nil {
				bad++
				fmt.Printf("  %s: %v\n", sha, err)
			}
		}
		fmt.Printf("%d objects checked, %d bad\n", len(shas), bad)
		if bad > 0 {
			os.Exit(1)
		}
	case "tier": // apply the tiering rules now, until nothing more matches
		store, err := blob.NewFromConfig(cfg)
		if err != nil {
			log.Fatal(err)
		}
		if blob.AsTiered(store) == nil {
			log.Fatal("set blob_cold_backend (and blob_cold_dir for fs) first")
		}
		svc := domain.New(st.Pool)
		if err := svc.ConfigureBlobs(ctx, &domain.Blobs{Store: store}); err != nil {
			log.Fatal(err)
		}
		moved, failed, bytes := 0, 0, int64(0)
		for {
			r, err := svc.RunTiering(ctx, 200)
			if err != nil {
				log.Fatal(err)
			}
			moved, failed, bytes = moved+r.Moved, failed+r.Failed, bytes+r.Bytes
			if r.Moved == 0 {
				if r.Error != "" {
					fmt.Println("  " + r.Error)
				}
				break
			}
		}
		fmt.Printf("%d objects moved to the cold tier (%d bytes), %d failed\n", moved, bytes, failed)
		if failed > 0 {
			os.Exit(1)
		}
	case "rewrap":
		p, _, err := blob.OpenPair(cfg)
		if err != nil {
			log.Fatal(err)
		}
		enc, ok := p.(*blob.Encrypted)
		if !ok {
			log.Fatal("set blob_encryption_keys and blob_encryption_current first")
		}
		rep := enc.RewrapAll(shas)
		fmt.Printf("%d objects: %d moved to key %s, %d failed\n", rep.Total, rep.Changed, cfg.BlobEncCurrent, len(rep.Failed))
		for _, f := range rep.Failed {
			fmt.Println("  " + f)
		}
		if len(rep.Failed) > 0 {
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: tasklane blobs migrate [--verify] | verify | rewrap | tier")
		os.Exit(2)
	}
}
