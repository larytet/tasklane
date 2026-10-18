package egress_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/larytet/tasklane/internal/egress"
)

func get(c *http.Client, url string, hdr map[string]string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return c.Do(req)
}

func TestEGRESS_PrivateAddressesAreBlockedUnlessAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	if _, err := get(egress.Policy{}.Client(), srv.URL, nil); !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("loopback was reachable by default: %v", err)
	}
	resp, err := get(egress.Policy{AllowPrivate: true}.Client(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, ip := range []string{"10.1.2.3", "192.168.0.9", "172.16.5.5", "127.0.0.1", "::1", "169.254.1.1", "fe80::1", "0.0.0.0", "fc00::1"} {
		if err := (egress.Policy{}).Check(net.ParseIP(ip)); err == nil {
			t.Errorf("%s allowed by default", ip)
		}
		if err := (egress.Policy{AllowPrivate: true}).Check(net.ParseIP(ip)); err != nil && ip != "0.0.0.0" {
			t.Errorf("%s refused though private addresses are allowed: %v", ip, err)
		}
	}
	// the cloud metadata address is never reachable, even when private ranges are allowed
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "100.100.100.200"} {
		if err := (egress.Policy{AllowPrivate: true}).Check(net.ParseIP(ip)); err == nil {
			t.Errorf("metadata address %s allowed", ip)
		}
	}
	if err := (egress.Policy{}).Check(net.ParseIP("93.184.216.34")); err != nil {
		t.Errorf("a public address was refused: %v", err)
	}
}

func TestEGRESS_NamesAreResolvedOnceAndEveryAnswerIsChecked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var lookups int32
	var dialed []string
	p := egress.Policy{
		Resolver: func(ctx context.Context, host string) ([]net.IP, error) {
			atomic.AddInt32(&lookups, 1)
			return []net.IP{net.ParseIP("93.184.216.34")}, nil // public as far as the check can tell
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port) // the test server stands in for that public address
		},
	}
	resp, err := get(p.Client(), "http://git.example.test:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&lookups) != 1 || len(dialed) != 1 || !strings.HasPrefix(dialed[0], "93.184.216.34:") {
		t.Fatalf("lookups %d dialed %v: the connection must go to the address that was checked", lookups, dialed)
	}
	// a rebinding answer that mixes a public and a private address is refused outright
	p.Resolver = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.5")}, nil
	}
	if _, err := get(p.Client(), "http://git.example.test:"+port+"/", nil); !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("%v", err)
	}
	// a name that resolves to a private address is refused
	p.Resolver = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.168.1.10")}, nil
	}
	if _, err := get(p.Client(), "http://internal.example.test:"+port+"/", nil); !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("%v", err)
	}
}

func TestEGRESS_RedirectsAreLimitedRecheckedAndStripCredentials(t *testing.T) {
	var seen string
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Write([]byte("b"))
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, b.URL, 302) }))
	defer a.Close()
	p := egress.Policy{AllowPrivate: true}
	resp, err := get(p.Client(), a.URL, map[string]string{"Authorization": "Bearer secret"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen != "" {
		t.Fatalf("a credential followed a redirect to another host: %q", seen)
	}
	// a redirect loop stops
	var loop *httptest.Server
	loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, loop.URL, 302) }))
	defer loop.Close()
	if _, err := get(p.Client(), loop.URL, nil); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("%v", err)
	}
	// a public name that redirects to a private address is blocked at the hop
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1:1/", 302) }))
	defer hop.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(hop.URL, "http://"))
	pp := egress.Policy{
		Resolver: func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, n, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, "127.0.0.1:"+port)
		},
	}
	if _, err := get(pp.Client(), "http://public.example.test:"+port+"/", nil); !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("a redirect into the private range was followed: %v", err)
	}
}

func TestEGRESS_AllowlistAndBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 5000)) }))
	defer srv.Close()
	if _, err := get(egress.Policy{AllowPrivate: true, AllowHosts: []string{"git.corp.example"}}.Client(), srv.URL, nil); !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("a host off the allowlist was reachable: %v", err)
	}
	p := egress.Policy{AllowPrivate: true, MaxBody: 1000}
	resp, _ := get(p.Client(), srv.URL, nil)
	defer resp.Body.Close()
	if _, err := p.ReadBody(resp.Body); err == nil {
		t.Fatal("an oversized response was read")
	}
}
