package config

import (
	"fmt"
	"sort"
	"sync"
)

// HotKeys are the settings a running server takes up from a reload (SIGHUP). Everything else (listeners, stores, secrets, the cluster) is
// read once at start, and a reload that changes one of them says that a restart is needed instead of pretending.
var HotKeys = map[string]bool{
	"password_min_length":        true,
	"mail_raw_retention_days":    true,
	"mail_bounce_retention_days": true,
	"min_client_version":         true,
	"trusted_proxies":            true,
}

// Reloader re-reads the configuration with the options the server started with.
type Reloader struct {
	opts Options
	mu   sync.Mutex
	last map[string]string
}

// ReloadResult says what a reload did: the hot settings that changed (applied) and the others that changed on disk (they take effect at the next restart).
type ReloadResult struct {
	Applied     []string
	RestartOnly []string
}

// NewReloader remembers the settings the server started with.
func NewReloader(o Options) (*Reloader, error) {
	es, err := Resolve(o)
	if err != nil {
		return nil, err
	}
	return &Reloader{opts: o, last: values(es)}, nil
}

func values(es []Entry) map[string]string {
	m := map[string]string{}
	for _, e := range es {
		m[e.Key.Name] = e.Value
	}
	return m
}

// Reload resolves the configuration again. An invalid configuration is rejected as a whole and the previous snapshot is kept. The returned
// Config is the new one in full; the caller applies only what ReloadResult.Applied lists.
func (r *Reloader) Reload() (Config, ReloadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var res ReloadResult
	es, err := Resolve(r.opts)
	if err != nil {
		return Config{}, res, fmt.Errorf("the new configuration is not valid, the running settings are kept: %w", err)
	}
	cfg, err := Load(r.opts)
	if err != nil {
		return Config{}, res, fmt.Errorf("the new configuration is not valid, the running settings are kept: %w", err)
	}
	now := values(es)
	for k, v := range now {
		if r.last[k] == v {
			continue
		}
		if HotKeys[k] {
			res.Applied = append(res.Applied, k)
		} else {
			res.RestartOnly = append(res.RestartOnly, k)
		}
	}
	sort.Strings(res.Applied)
	sort.Strings(res.RestartOnly)
	// what the file says now is the new baseline for hot keys; restart-only keys keep reporting until the server restarts with them
	for _, k := range res.Applied {
		r.last[k] = now[k]
	}
	return cfg, res, nil
}
