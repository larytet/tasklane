package domain

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The security policy of an organization (SPEC 17.4): who may sign in from where, how long a session may last, and whether a
// second factor is compulsory. It is enforced at sign-in and again on every request, so tightening it takes effect at once for
// people who are already signed in (on this node; other nodes read it within a few seconds).

var (
	ErrIPNotAllowed         = errors.New("this network is not allowed for this organization")
	ErrSecondFactorRequired = errors.New("this organization requires a second factor: set one up to continue")
)

const policyCacheFor = 5 * time.Second

type SecurityPolicy struct {
	RequireSecondFactor bool     `json:"require_second_factor"`
	IPAllowlist         []string `json:"ip_allowlist"`
	SessionMaxHours     int      `json:"session_max_hours"`
	// AllowedEmailDomains limits the addresses people of the organization can have (empty: any). PeopleOutsideDomains counts the
	// active people who already have another address: they stay, but nobody like them can be added or re-activated.
	AllowedEmailDomains  []string `json:"allowed_email_domains"`
	PeopleOutsideDomains *int     `json:"people_outside_allowed_domains,omitempty"`
	// WithoutSecondFactor is how many active people in the organization would be held at the enrolment page by RequireSecondFactor.
	WithoutSecondFactor *int `json:"people_without_second_factor,omitempty"`
}

type policyEntry struct {
	p    SecurityPolicy
	nets []*net.IPNet
	at   time.Time
}

type factorEntry struct {
	has bool
	at  time.Time
}

type policyCache struct {
	mu      sync.Mutex
	orgs    map[string]policyEntry
	factors map[string]factorEntry
}

func (s *Service) pc() *policyCache {
	s.policyOnce.Do(func() { s.policies = &policyCache{orgs: map[string]policyEntry{}, factors: map[string]factorEntry{}} })
	return s.policies
}

// forgetPolicy drops the remembered policy of an organization on this node and tells the others to do the same; a node that does not
// hear (no bus, or an older release) keeps its copy for policyCacheFor at most.
func (s *Service) forgetPolicy(org string) {
	s.forgetPolicyHere(org)
	s.busSend(busMsg{Kind: "forget", Forget: &forgetBusIn{Org: org}})
}

func (s *Service) forgetPolicyHere(org string) {
	c := s.pc()
	c.mu.Lock()
	delete(c.orgs, org)
	c.mu.Unlock()
}

// forgetFactor drops what is remembered about whether a person has a second factor (they just added or removed one).
func (s *Service) forgetFactor(userID string) {
	s.forgetFactorHere(userID)
	s.busSend(busMsg{Kind: "forget", Forget: &forgetBusIn{User: userID}})
}

func (s *Service) forgetFactorHere(userID string) {
	c := s.pc()
	c.mu.Lock()
	delete(c.factors, userID)
	c.mu.Unlock()
}

func parseCIDRs(list []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range list {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") { // a single address
			if ip := net.ParseIP(c); ip != nil {
				if ip.To4() != nil {
					c += "/32"
				} else {
					c += "/128"
				}
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, invalid("%q is not an address or a network such as 203.0.113.0/24", c)
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Service) policyOf(ctx context.Context, org string) (policyEntry, error) {
	c := s.pc()
	c.mu.Lock()
	if e, ok := c.orgs[org]; ok && s.Now().Sub(e.at) < policyCacheFor {
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()
	var e policyEntry
	err := s.DB.QueryRow(ctx, `SELECT require_second_factor,ip_allowlist,session_max_hours,allowed_email_domains FROM organization WHERE id::text=$1`, org).Scan(&e.p.RequireSecondFactor, &e.p.IPAllowlist, &e.p.SessionMaxHours, &e.p.AllowedEmailDomains)
	if err != nil {
		return e, err
	}
	if e.nets, err = parseCIDRs(e.p.IPAllowlist); err != nil {
		return e, err
	}
	e.at = s.Now()
	c.mu.Lock()
	c.orgs[org] = e
	c.mu.Unlock()
	return e, nil
}

func ipIn(nets []*net.IPNet, ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// GetSecurityPolicy reads the organization's policy. Organization administrators see their own; an instance administrator may name
// another organization.
func (s *Service) GetSecurityPolicy(ctx context.Context, a Actor, org string) (SecurityPolicy, error) {
	org, err := s.policyTarget(a, org)
	if err != nil {
		return SecurityPolicy{}, err
	}
	e, err := s.policyOf(ctx, org)
	if err != nil {
		return SecurityPolicy{}, ErrNotFound
	}
	p := e.p
	if p.IPAllowlist == nil {
		p.IPAllowlist = []string{}
	}
	var n int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM app_user u WHERE u.org_id::text=$1 AND u.active AND u.kind='human' AND NOT EXISTS (SELECT 1 FROM user_mfa m WHERE m.user_id=u.id AND m.enabled) AND NOT EXISTS (SELECT 1 FROM webauthn_credential w WHERE w.user_id=u.id)`, org).Scan(&n)
	p.WithoutSecondFactor = &n
	if p.AllowedEmailDomains == nil {
		p.AllowedEmailDomains = []string{}
	}
	var outside int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM app_user u WHERE u.org_id::text=$1 AND u.active AND u.kind='human' AND cardinality($2::text[])>0 AND NOT (lower(split_part(u.email,'@',2)) = ANY($2::text[]))`, org, p.AllowedEmailDomains).Scan(&outside)
	p.PeopleOutsideDomains = &outside
	return p, nil
}

func (s *Service) policyTarget(a Actor, org string) (string, error) {
	if !a.Admin && !a.OrgAdmin {
		return "", ErrForbidden
	}
	if org == "" || org == a.OrgID() {
		return a.OrgID(), nil
	}
	if !a.Admin { // an organization administrator never reads or changes another organization
		return "", ErrNotFound
	}
	return org, nil
}

// SetSecurityPolicy replaces the policy. An organization administrator who limits the networks must be on one of them (the change
// would lock them out otherwise); the instance administrator is never subject to the list and so cannot be locked out by it.
func (s *Service) SetSecurityPolicy(ctx context.Context, a Actor, org string, in SecurityPolicy, callerIP string) error {
	org, err := s.policyTarget(a, org)
	if err != nil {
		return err
	}
	if in.SessionMaxHours < 0 || in.SessionMaxHours > 24*365 {
		return invalid("the longest session is between 0 (no limit) and 8760 hours")
	}
	nets, err := parseCIDRs(in.IPAllowlist)
	if err != nil {
		return err
	}
	if len(in.IPAllowlist) > 100 {
		return invalid("at most 100 networks")
	}
	if len(nets) > 0 && !a.Admin && !ipIn(nets, callerIP) {
		return invalid("your own address (%s) is not on the list: this would lock you out", callerIP)
	}
	domains, err := cleanDomains(in.AllowedEmailDomains)
	if err != nil {
		return err
	}
	// the same guard for a second factor: an administrator without one would be held at the enrolment page by their own change
	if in.RequireSecondFactor && org == a.OrgID() && !s.hasSecondFactorNow(ctx, a.ID) {
		return invalid("set up your own second factor first: requiring one would hold you at the enrolment page")
	}
	list := make([]string, 0, len(nets))
	for _, n := range nets {
		list = append(list, n.String())
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE organization SET require_second_factor=$2, ip_allowlist=$3, session_max_hours=$4, allowed_email_domains=$5 WHERE id::text=$1`, org, in.RequireSecondFactor, list, in.SessionMaxHours, domains)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "security.policy_changed", org, map[string]any{"require_second_factor": in.RequireSecondFactor, "ip_allowlist": list, "session_max_hours": in.SessionMaxHours, "allowed_email_domains": domains})
	})
	s.forgetPolicy(org)
	return err
}

// ipDenied says whether sign-in or use from this address is refused for a person of the organization. The instance administrator
// is exempt, so a mistaken list can always be repaired.
func (s *Service) ipDenied(ctx context.Context, org, ip string, instanceAdmin bool) bool {
	if instanceAdmin {
		return false
	}
	e, err := s.policyOf(ctx, org)
	if err != nil || len(e.nets) == 0 {
		return false
	}
	return !ipIn(e.nets, ip)
}

func (s *Service) hasSecondFactor(ctx context.Context, userID string) bool {
	c := s.pc()
	c.mu.Lock()
	if e, ok := c.factors[userID]; ok && s.Now().Sub(e.at) < policyCacheFor {
		c.mu.Unlock()
		return e.has
	}
	c.mu.Unlock()
	var has bool
	s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_mfa WHERE user_id::text=$1 AND enabled) OR EXISTS (SELECT 1 FROM webauthn_credential WHERE user_id::text=$1)`, userID).Scan(&has)
	c.mu.Lock()
	c.factors[userID] = factorEntry{has, s.Now()}
	c.mu.Unlock()
	return has
}

// hasSecondFactorNow is hasSecondFactor without the short memory, for decisions that must not act on a stale answer.
func (s *Service) hasSecondFactorNow(ctx context.Context, userID string) bool {
	s.forgetFactor(userID)
	return s.hasSecondFactor(ctx, userID)
}

// EnforcePolicy is called for every authenticated request. It refuses use from a network the organization does not allow, and holds
// a person whose organization requires a second factor, and who has none, to the pages that set one up (exempt says whether this
// request is one of them). Single sign-on sessions and tokens are not held: the provider authenticates the first, and a token is an
// explicit credential the person made while they had a session.
func (s *Service) EnforcePolicy(ctx context.Context, a Actor, ip, token string, viaCookie, exempt bool) error {
	if a.ID == "" {
		return nil
	}
	e, err := s.policyOf(ctx, a.OrgID())
	if err != nil {
		return nil // the policy cannot be read: do not turn a database fault into a lock-out
	}
	if len(e.nets) > 0 && !a.Admin && !ipIn(e.nets, ip) {
		return ErrIPNotAllowed
	}
	if e.p.RequireSecondFactor && viaCookie && a.Kind != "agent" && !exempt && !s.hasSecondFactor(ctx, a.ID) {
		var via string
		s.DB.QueryRow(ctx, `SELECT via FROM session WHERE token_hash=$1`, hashToken(token)).Scan(&via)
		if via != "sso" {
			return ErrSecondFactorRequired
		}
	}
	return nil
}

// EnforceNetwork is EnforcePolicy for credentials that are not held to the second-factor rule (the MCP endpoint).
func (s *Service) EnforceNetwork(ctx context.Context, a Actor, ip string) error {
	return s.EnforcePolicy(ctx, a, ip, "", false, true)
}

// ClientIP is the address a request came from: the connecting peer, or, when that is one of the configured trusted proxies, the
// nearest address in X-Forwarded-For that is not itself a proxy. A header from anyone else is ignored (it is trivial to forge).
func (s *Service) ClientIP(remoteAddr, forwardedFor string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if len(s.TrustedProxies) == 0 || !ipIn(s.TrustedProxies, host) {
		return host
	}
	parts := strings.Split(forwardedFor, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		if net.ParseIP(p) == nil {
			return host // not an address: do not guess
		}
		if !ipIn(s.TrustedProxies, p) {
			return p
		}
	}
	return host
}

var reDomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// cleanDomains normalises the allowed email domains: lower case, without a leading "@", exact host names (no wildcards, no
// addresses), at most 50.
func cleanDomains(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, d := range in {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d == "" {
			continue
		}
		if !reDomain.MatchString(d) || len(d) > 253 {
			return nil, invalid("%q is not a domain name such as example.com (no wildcards, no addresses)", d)
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) > 50 {
		return nil, invalid("at most 50 domains")
	}
	return out, nil
}
