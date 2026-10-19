package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/store"
)

// olderLiveNodes names the live nodes that run a release older than this binary: while one exists, the contract migrations (which
// remove what that release still uses) must not be applied.
func olderLiveNodes(ctx context.Context, svc *domain.Service, mine string) ([]domain.Node, error) {
	ns, err := svc.ClusterNodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.Node
	for _, n := range ns {
		if n.Live && domain.CompareVersions(n.Version, mine) < 0 {
			out = append(out, n)
		}
	}
	return out, nil
}

// finalizeGuard refuses `migrate finalize` while a live node of an older release is registered (unless forced).
func finalizeGuard(ctx context.Context, st *store.Store, force bool) error {
	old, err := olderLiveNodes(ctx, domain.New(st.Pool), releaseVersion())
	if err != nil {
		return nil // no node registry yet (a database from before the cluster tables): nothing to protect
	}
	if len(old) == 0 || force {
		return nil
	}
	var b strings.Builder
	for _, n := range old {
		fmt.Fprintf(&b, " %s (%s)", n.ID, n.Version)
	}
	return fmt.Errorf("refusing to finalize: %d live node(s) still run an older release than %s:%s\nreplace them first, or pass --force if they are really gone", len(old), releaseVersion(), b.String())
}

// upgradePreflightCmd is `tasklane upgrade preflight [--backup DIR]`: what an upgrade to this binary would do, what stands in the way,
// and the order to do it in. It changes nothing; it exits 1 when something blocks.
func upgradePreflightCmd(ctx context.Context, cfg config.Config) {
	backup := "BACKUP_DIR"
	if len(os.Args) == 5 && os.Args[3] == "--backup" {
		backup = os.Args[4]
	} else if len(os.Args) != 3 {
		log.Fatal("usage: tasklane upgrade preflight [--backup DIR]")
	}
	st := mustOpen(ctx, cfg)
	defer st.Close()
	svc := domain.New(st.Pool)
	mine := releaseVersion()
	blockers := 0
	fmt.Printf("this binary: %s\n", mine)

	plan, err := st.Plan(ctx)
	if err != nil {
		fmt.Printf("BLOCKED  the migration history cannot be read: %v\n", err)
		os.Exit(1)
	}
	var expand, held []store.PlanStep
	for _, p := range plan {
		if p.Held {
			held = append(held, p)
		} else {
			expand = append(expand, p)
		}
	}
	if _, err := st.CheckCompat(ctx, store.DefaultPolicy()); err != nil {
		var ie *store.IncompatibleError
		if !errors.As(err, &ie) || !ie.Behind() { // behind is what `migrate` is for; anything else stops the upgrade
			fmt.Printf("BLOCKED  %v\n", err)
			blockers++
		}
	}
	if blockers == 0 {
		fmt.Printf("ok       schema: %d migration(s) to apply now, %d held until finalize\n", len(expand), len(held))
	}
	for _, p := range expand {
		fmt.Printf("           would apply %04d %s [%s%s]\n", p.Version, p.Name, p.Phase, map[bool]string{true: ", no transaction", false: ""}[p.NoTx])
	}
	for _, p := range held {
		fmt.Printf("           held        %04d %s [%s]\n", p.Version, p.Name, p.Phase)
	}

	ns, err := svc.ClusterNodes(ctx)
	live, old := 0, 0
	if err == nil {
		for _, n := range ns {
			if !n.Live {
				continue
			}
			live++
			mark := "current"
			if domain.CompareVersions(n.Version, mine) < 0 {
				old++
				mark = "older, to replace"
			} else if domain.CompareVersions(n.Version, mine) > 0 {
				mark = "NEWER than this binary"
				blockers++
			}
			fmt.Printf("           node %s runs %s (%s)\n", n.ID, n.Version, mark)
		}
	}
	if live == 0 {
		fmt.Println("note     no live node is registered: a single server that is stopped, or a database from before the node registry")
	} else {
		fmt.Printf("ok       %d live node(s), %d to replace\n", live, old)
	}

	fmt.Println("procedure:")
	step := 1
	fmt.Printf("  %d. tasklane backup %s            (verify it with `tasklane verify-backup`)\n", step, backup)
	step++
	if len(expand) > 0 {
		fmt.Printf("  %d. tasklane migrate                       (expand: the running release keeps working)\n", step)
		step++
	}
	fmt.Printf("  %d. replace the nodes one at a time with %s; each one drains first and leaves the others serving\n", step, mine)
	step++
	if len(held) > 0 {
		fmt.Printf("  %d. tasklane migrate finalize              (only when no older node is left and a rollback is no longer wanted)\n", step)
	}
	if blockers > 0 {
		fmt.Printf("%d blocker(s)\n", blockers)
		os.Exit(1)
	}
}
