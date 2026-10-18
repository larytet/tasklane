package domain

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The cluster version gate (SPEC 17.8, UPG-03, UPG-04). Every node writes the version it runs; a new behaviour that older nodes
// cannot handle is a gate, and a gate stays off until every live node reports at least the version it needs. Then it turns on by
// itself, or, for a risky one, waits for an administrator. Once on it stays on, so a node older than an active gate refuses to
// start (the rollback is refused with the reason, not allowed to corrupt data).

const (
	nodeLiveFor    = 30 * time.Second
	nodeBeatEvery  = 10 * time.Second
	gateOff        = "off"
	gatePending    = "pending"
	gateOn         = "on"
	MailRetentionG = "mail_retention" // the purge of inbound mail leaves a hashed Message-ID that older nodes do not recognise
)

// Gate is a behaviour that needs every live node to be new enough.
type Gate struct {
	Name       string `json:"name"`
	MinVersion string `json:"min_version"`
	Risky      bool   `json:"risky"` // an administrator confirms it after it qualifies
	Doc        string `json:"doc"`
}

// BuiltinGates are the gates this release defines.
var BuiltinGates = []Gate{
	{Name: MailRetentionG, MinVersion: "0.1.0", Doc: "Purging inbound mail replaces its Message-ID by a hash; a node that does not know the hash would treat a redelivery as new mail"},
}

type Node struct {
	ID       string    `json:"id"`
	Version  string    `json:"version"`
	Started  time.Time `json:"started_at"`
	Seen     time.Time `json:"last_seen_at"`
	Live     bool      `json:"live"`
	Draining bool      `json:"draining"`
}

type GateState struct {
	Gate
	State       string     `json:"state"`
	Qualifies   bool       `json:"qualifies"`
	Blocking    []string   `json:"blocking_nodes,omitempty"`
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
}

type Cluster struct {
	Nodes      []Node      `json:"nodes"`
	MinVersion string      `json:"min_version"`
	Gates      []GateState `json:"gates"`
}

// CompareVersions orders semantic versions (1.2.3, with an optional v and a pre-release suffix). A "-dev" suffix is tip-of-tree
// and counts as the release it leads up to, so development builds are not stuck below their own gates; other pre-releases sort
// below the release. It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	pa, pra := splitVersion(a)
	pb, prb := splitVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pra == prb:
		return 0
	case pra == "":
		return 1
	case prb == "":
		return -1
	case pra < prb:
		return -1
	}
	return 1
}

func splitVersion(v string) (core [3]int, pre string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "+"); i >= 0 {
		v = v[:i]
	}
	if i := strings.Index(v, "-"); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	if pre == "dev" {
		pre = ""
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		core[i], _ = strconv.Atoi(p)
	}
	return
}

func (s *Service) gates() []Gate {
	if s.Gates != nil {
		return s.Gates
	}
	return BuiltinGates
}

// RegisterNode records this node as running its version; it is repeated every few seconds, and a node that stops is dropped from
// the quorum after half a minute (or at once when it leaves gracefully).
func (s *Service) RegisterNode(ctx context.Context) error {
	if s.NodeID == "" || s.NodeVersion == "" {
		return nil
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO cluster_node(id,version,started_at,seen_at,draining) VALUES($1,$2,$3,$3,false)
		ON CONFLICT (id) DO UPDATE SET version=$2, seen_at=$3, draining=false`, s.NodeID, s.NodeVersion, s.Now())
	return err
}

// LeaveCluster removes this node (a graceful stop), so the gates do not wait for it.
func (s *Service) LeaveCluster(ctx context.Context) {
	if s.NodeID != "" {
		s.DB.Exec(ctx, `DELETE FROM cluster_node WHERE id=$1`, s.NodeID)
	}
}

// Drain marks this node as leaving: it still counts for the version (it is running) but a rolling upgrade can see it going.
func (s *Service) Drain(ctx context.Context) {
	if s.NodeID != "" {
		s.DB.Exec(ctx, `UPDATE cluster_node SET draining=true WHERE id=$1`, s.NodeID)
	}
}

func (s *Service) liveNodes(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]Node, error) {
	rows, err := q.Query(ctx, `SELECT id,version,started_at,seen_at,draining FROM cluster_node ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Version, &n.Started, &n.Seen, &n.Draining); err != nil {
			return nil, err
		}
		n.Live = s.Now().Sub(n.Seen) < nodeLiveFor
		out = append(out, n)
	}
	return out, rows.Err()
}

// ClusterMinVersion is the oldest version among the live nodes ("" when none is registered).
func (s *Service) ClusterMinVersion(ctx context.Context) (string, error) {
	ns, err := s.liveNodes(ctx, s.DB)
	if err != nil {
		return "", err
	}
	min := ""
	for _, n := range ns {
		if n.Live && (min == "" || CompareVersions(n.Version, min) < 0) {
			min = n.Version
		}
	}
	return min, nil
}

// EvaluateGates turns on the gates every live node qualifies for. A risky gate becomes "pending" and waits for ConfirmGate.
// A gate that is on stays on.
func (s *Service) EvaluateGates(ctx context.Context) error {
	ns, err := s.liveNodes(ctx, s.DB)
	if err != nil {
		return err
	}
	for _, g := range s.gates() {
		if _, err := s.DB.Exec(ctx, `INSERT INTO feature_gate(name,min_version,risky) VALUES($1,$2,$3) ON CONFLICT (name) DO UPDATE SET min_version=$2, risky=$3`, g.Name, g.MinVersion, g.Risky); err != nil {
			return err
		}
		var state string
		if err := s.DB.QueryRow(ctx, `SELECT state FROM feature_gate WHERE name=$1`, g.Name).Scan(&state); err != nil {
			return err
		}
		if state == gateOn {
			continue
		}
		qual, live := true, 0
		for _, n := range ns {
			if n.Live {
				live++
				if CompareVersions(n.Version, g.MinVersion) < 0 {
					qual = false
				}
			}
		}
		if live == 0 {
			qual = false // nobody has said what they run
		}
		switch {
		case qual && !g.Risky:
			_, err = s.DB.Exec(ctx, `UPDATE feature_gate SET state='on', activated_at=$2 WHERE name=$1 AND state<>'on'`, g.Name, s.Now())
		case qual:
			_, err = s.DB.Exec(ctx, `UPDATE feature_gate SET state='pending' WHERE name=$1 AND state='off'`, g.Name)
		default:
			_, err = s.DB.Exec(ctx, `UPDATE feature_gate SET state='off' WHERE name=$1 AND state='pending'`, g.Name) // a node below the version joined again
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// GateOn says whether a behaviour is active cluster-wide.
func (s *Service) GateOn(ctx context.Context, name string) bool {
	var state string
	if s.DB.QueryRow(ctx, `SELECT state FROM feature_gate WHERE name=$1`, name).Scan(&state) != nil {
		return false
	}
	return state == gateOn
}

// ConfirmGate is the administrator's yes for a risky gate that qualifies.
func (s *Service) ConfirmGate(ctx context.Context, a Actor, name string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	if err := s.EvaluateGates(ctx); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM feature_gate WHERE name=$1 FOR UPDATE`, name).Scan(&state); err != nil {
			return ErrNotFound
		}
		switch state {
		case gateOn:
			return nil
		case gateOff:
			min, _ := s.ClusterMinVersion(ctx)
			return &ConflictError{fmt.Sprintf("not every node is new enough yet (the oldest live node runs %s)", min)}
		}
		if _, err := tx.Exec(ctx, `UPDATE feature_gate SET state='on', activated_at=$2, confirmed_by=$3 WHERE name=$1`, name, s.Now(), nilIfEmpty(a.ID)); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "cluster.gate.activated", name, map[string]any{})
	})
}

// ClusterStatus is the administrator's view: nodes, the oldest version, and each gate.
func (s *Service) ClusterStatus(ctx context.Context, a Actor) (Cluster, error) {
	if err := adminOnly(a); err != nil {
		return Cluster{}, err
	}
	if err := s.EvaluateGates(ctx); err != nil {
		return Cluster{}, err
	}
	ns, err := s.liveNodes(ctx, s.DB)
	if err != nil {
		return Cluster{}, err
	}
	c := Cluster{Nodes: ns}
	if c.Nodes == nil {
		c.Nodes = []Node{}
	}
	c.MinVersion, _ = s.ClusterMinVersion(ctx)
	for _, g := range s.gates() {
		gs := GateState{Gate: g}
		s.DB.QueryRow(ctx, `SELECT state,activated_at FROM feature_gate WHERE name=$1`, g.Name).Scan(&gs.State, &gs.ActivatedAt)
		gs.Qualifies = true
		for _, n := range ns {
			if n.Live && CompareVersions(n.Version, g.MinVersion) < 0 {
				gs.Qualifies = false
				gs.Blocking = append(gs.Blocking, n.ID+" ("+n.Version+")")
			}
		}
		c.Gates = append(c.Gates, gs)
	}
	return c, nil
}

// CheckNodeAgainstGates refuses a node that is older than a behaviour already switched on: running it would have an old node read
// data written the new way. It names the gate and the version needed, so the operator knows rolling back is not an option.
func (s *Service) CheckNodeAgainstGates(ctx context.Context, version string) error {
	rows, err := s.DB.Query(ctx, `SELECT name,min_version FROM feature_gate WHERE state='on' ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, min string
		rows.Scan(&name, &min)
		if CompareVersions(version, min) < 0 {
			return fmt.Errorf("version %s is older than the behaviour %q, which is already on for this cluster and needs %s or later; run %s or later, or restore from a backup taken before it was activated", version, name, min, min)
		}
	}
	return rows.Err()
}

// RunCluster keeps this node registered and the gates evaluated until the context ends, and leaves the cluster on the way out.
func (s *Service) RunCluster(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(nodeBeatEvery)
	defer t.Stop()
	for {
		if err := s.RegisterNode(ctx); err != nil && ctx.Err() == nil {
			logf("cluster: %v", err)
		} else if err := s.EvaluateGates(ctx); err != nil && ctx.Err() == nil {
			logf("cluster gates: %v", err)
		}
		select {
		case <-ctx.Done():
			s.LeaveCluster(context.WithoutCancel(ctx))
			return
		case <-t.C:
		}
	}
}
