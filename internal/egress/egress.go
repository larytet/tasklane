// Package egress is the one HTTP client for server-side fetches of admin- or user-supplied URLs (SPEC 17.1): Git provider
// base URLs, and later Jira and import-from-URL. It blocks loopback, link-local, metadata and private ranges unless the
// connection allows them, resolves a name once and connects to the address it checked (so DNS cannot change its mind
// between the check and the connection), limits redirects and re-checks every hop, forwards no credentials to another
// host, and bounds time and response size.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var ErrBlocked = errors.New("egress: address not allowed")

type Policy struct {
	AllowPrivate bool     // loopback, link-local and private ranges; metadata addresses stay blocked regardless
	AllowHosts   []string // when not empty, only these host names (exact, lower-case) may be contacted
	MaxRedirects int      // default 3
	Timeout      time.Duration
	MaxBody      int64 // default 32 MiB, applied by Read
	// Resolver lets tests control name resolution; default is the system resolver.
	Resolver func(ctx context.Context, host string) ([]net.IP, error)
	// Dial lets tests intercept the connection to the checked address.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p Policy) maxRedirects() int {
	if p.MaxRedirects == 0 {
		return 3
	}
	return p.MaxRedirects
}

var metadata = []string{"169.254.169.254", "fd00:ec2::254", "100.100.100.200"}

// Check reports whether an address may be contacted under the policy.
func (p Policy) Check(ip net.IP) error {
	for _, m := range metadata {
		if ip.Equal(net.ParseIP(m)) {
			return fmt.Errorf("%w: %s is a cloud metadata address", ErrBlocked, ip)
		}
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("%w: %s", ErrBlocked, ip)
	}
	if (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) && !p.AllowPrivate {
		return fmt.Errorf("%w: %s is a private or loopback address (allow it on the connection if that is intended)", ErrBlocked, ip)
	}
	return nil
}

func (p Policy) hostAllowed(host string) bool {
	if len(p.AllowHosts) == 0 {
		return true
	}
	h := strings.ToLower(host)
	for _, a := range p.AllowHosts {
		if h == a {
			return true
		}
	}
	return false
}

// Client returns an http.Client that enforces the policy.
func (p Policy) Client() *http.Client {
	resolve := p.Resolver
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	dial := p.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = d.DialContext
	}
	tr := &http.Transport{
		Proxy: nil, // a proxy would connect on our behalf to an address we have not checked
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if !p.hostAllowed(host) {
				return nil, fmt.Errorf("%w: host %s is not on the allowlist", ErrBlocked, host)
			}
			ips := []net.IP{net.ParseIP(host)}
			if ips[0] == nil { // a name: resolve once, check every answer, and connect to a checked address only
				if ips, err = resolve(ctx, host); err != nil {
					return nil, err
				}
			}
			var last error
			for _, ip := range ips {
				if err := p.Check(ip); err != nil {
					return nil, err // one bad answer among good ones is enough to refuse: rebinding tricks mix them
				}
			}
			for _, ip := range ips {
				c, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > p.maxRedirects() {
			return fmt.Errorf("egress: more than %d redirects", p.maxRedirects())
		}
		if req.URL.Host != via[0].URL.Host { // never carry credentials to another host
			req.Header.Del("Authorization")
			req.Header.Del("Private-Token")
			req.Header.Del("PRIVATE-TOKEN")
		}
		return nil // the dialer re-checks the address of every hop
	}}
}

// ReadBody reads at most the policy's limit and fails when the response is larger.
func (p Policy) ReadBody(r io.Reader) ([]byte, error) {
	max := p.MaxBody
	if max == 0 {
		max = 32 << 20
	}
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("egress: response larger than %d bytes", max)
	}
	return b, nil
}
