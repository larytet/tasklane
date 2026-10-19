package domain_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// snapshots writes one day's usage for an organization directly, as RecordUsage would on that day.
func snapshot(t *testing.T, e *testutil.Env, org, day string, seats int, bytes int64, customers int) {
	t.Helper()
	if _, err := e.Store.Pool.Exec(ctx, `INSERT INTO usage_snapshot(org_id,day,seats,projects,issues,storage_bytes,active_customers) VALUES($1::uuid,$2::date,$3,1,10,$4,$5)`, org, day, seats, bytes, customers); err != nil {
		t.Fatal(err)
	}
}

// HOST-02: an invoice prices a month's snapshots exactly, keeps what it was made under, is made once, is delivered signed, and is read only by those who may.
func TestHOST02_InvoicesFromUsage(t *testing.T) {
	e, org, boss := hostedEnv(t)
	if _, err := e.Svc.SetPrices(ctx, e.Admin, org, domain.Prices{BaseCents: 1000}); !isInvalid(err) {
		t.Fatalf("prices before a plan: %v", err)
	}
	if _, err := e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{Plan: "team"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.SetPrices(ctx, boss, org, domain.Prices{BaseCents: 1000}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a tenant set its own prices: %v", err)
	}
	for _, bad := range []domain.Prices{{Currency: "dollars"}, {BaseCents: -1}, {SeatCents: 1 << 40}} {
		if _, err := e.Svc.SetPrices(ctx, e.Admin, org, bad); !isInvalid(err) {
			t.Fatalf("bad prices %+v: %v", bad, err)
		}
	}
	pl, err := e.Svc.SetPrices(ctx, e.Admin, org, domain.Prices{Currency: "EUR", BaseCents: 2900, SeatCents: 850, GiBCents: 50, CustomerCents: 25})
	if err != nil || pl.Prices == nil || pl.Prices.Currency != "eur" || pl.Prices.SeatCents != 850 {
		t.Fatalf("%+v %v", pl, err)
	}
	if _, err := e.Svc.GenerateInvoice(ctx, e.Admin, org, "2026-09"); !isInvalid(err) || !strings.Contains(err.Error(), "no usage") {
		t.Fatalf("an invoice with nothing recorded: %v", err)
	}
	for _, bad := range []string{"", "2026-13", "26-09", "2026-9"} {
		if _, err := e.Svc.GenerateInvoice(ctx, e.Admin, org, bad); !isInvalid(err) {
			t.Fatalf("period %q: %v", bad, err)
		}
	}
	// three days of September: seats 3, 4, 4 (average 11/3 = 3.666..), storage 1 GiB, 2 GiB, 3 GiB (average 2 GiB), customers 5, 9, 7 (peak 9)
	gib := int64(1) << 30
	snapshot(t, e, org, "2026-09-01", 3, 1*gib, 5)
	snapshot(t, e, org, "2026-09-02", 4, 2*gib, 9)
	snapshot(t, e, org, "2026-09-03", 4, 3*gib, 7)
	snapshot(t, e, org, "2026-10-01", 50, 99*gib, 99) // another month: not part of it
	inv, err := e.Svc.GenerateInvoice(ctx, e.Admin, org, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	// base 2900; seats 11*850/3 = 3116.67 -> 3117 (half up, not truncated); storage 2 GiB * 50 = 100; customers 9 * 25 = 225
	want := map[string]int64{"Base": 2900, "Seats": 3117, "Attachment storage": 100, "Active customers": 225}
	var total int64
	for _, l := range inv.Lines {
		if l.AmountCents != want[l.Item] {
			t.Errorf("%s: %d cents, want %d", l.Item, l.AmountCents, want[l.Item])
		}
		total += l.AmountCents
	}
	if len(inv.Lines) != 4 || inv.TotalCents != total || total != 2900+3117+100+225 || inv.Currency != "eur" || inv.Number != "TL-202609-0001" || inv.Status != "open" {
		t.Fatalf("invoice: %+v", inv)
	}
	if inv.Usage.Days != 3 || inv.Usage.SeatsPeak != 4 || inv.Usage.CustomersPeak != 9 || inv.Usage.DaysInPeriod != 30 {
		t.Fatalf("usage: %+v", inv.Usage)
	}
	// it keeps the prices it was made under
	e.Svc.SetPrices(ctx, e.Admin, org, domain.Prices{Currency: "eur", BaseCents: 9999, SeatCents: 850})
	got, err := e.Svc.GetInvoice(ctx, boss, org, inv.Number)
	if err != nil || got.TotalCents != inv.TotalCents || got.Plan.Prices == nil || got.Plan.Prices.BaseCents != 2900 {
		t.Fatalf("an invoice changed after the prices did: %+v %v", got, err)
	}
	// once a month; a void one can be made again
	if _, err := e.Svc.GenerateInvoice(ctx, e.Admin, org, "2026-09"); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("a second invoice for the month: %v", err)
	}
	if _, err := e.Svc.VoidInvoice(ctx, e.Admin, org, inv.Number, " "); !isInvalid(err) {
		t.Fatalf("void without a reason: %v", err)
	}
	if v, err := e.Svc.VoidInvoice(ctx, e.Admin, org, inv.Number, "wrong price"); err != nil || v.Status != "void" || v.VoidReason != "wrong price" {
		t.Fatalf("%+v %v", v, err)
	}
	again, err := e.Svc.GenerateInvoice(ctx, e.Admin, org, "2026-09")
	if err != nil || again.Number != "TL-202609-0002" || again.TotalCents == inv.TotalCents {
		t.Fatalf("after a void: %+v %v", again, err)
	}
	// who may read
	if _, err := e.Svc.ListInvoices(ctx, e.Alice, org); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member listed invoices: %v", err)
	}
	if _, err := e.Svc.ListInvoices(ctx, boss, domain.DefaultOrg); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("another organization's invoices: %v", err)
	}
	if l, err := e.Svc.ListInvoices(ctx, boss, org); err != nil || len(l) != 2 || l[0].Number != "TL-202609-0002" {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := e.Svc.GetInvoice(ctx, e.Admin, domain.DefaultOrg, again.Number); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an invoice read under another organization: %v", err)
	}
	csv := domain.InvoiceCSV(again)
	if !strings.Contains(csv, "TL-202609-0002") || !strings.Contains(csv, "Seats,") || !strings.Contains(csv, "total,,,") {
		t.Fatalf("csv:\n%s", csv)
	}
}

func TestHOST02_DeliveryIsSignedAndRetryable(t *testing.T) {
	e, org, boss := hostedEnv(t)
	e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{Plan: "team"})
	e.Svc.SetPrices(ctx, e.Admin, org, domain.Prices{BaseCents: 500})
	snapshot(t, e, org, "2026-09-01", 1, 0, 0)
	inv, err := e.Svc.GenerateInvoice(ctx, e.Admin, org, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.SendInvoice(ctx, e.Admin, org, inv.Number); !isInvalid(err) || !strings.Contains(err.Error(), "no billing endpoint") {
		t.Fatalf("no sink: %v", err)
	}
	var mu sync.Mutex
	var bodies [][]byte
	var sigs, keys []string
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		bodies, sigs, keys = append(bodies, b), append(sigs, r.Header.Get("X-Tasklane-Signature")), append(keys, r.Header.Get("Idempotency-Key"))
		if fail {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	e.Svc.Billing = domain.WebhookBilling{URL: srv.URL, Secret: "s3cret", AllowPrivate: true}
	_, err = e.Svc.SendInvoice(ctx, e.Admin, org, inv.Number)
	var ue *domain.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("a refused delivery: %v", err)
	}
	if got, _ := e.Svc.GetInvoice(ctx, e.Admin, org, inv.Number); got.Status != "open" {
		t.Fatalf("a failed delivery marked the invoice %s", got.Status)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	sent, err := e.Svc.SendInvoice(ctx, e.Admin, org, inv.Number)
	if err != nil || sent.Status != "sent" || sent.Sent == nil {
		t.Fatalf("%+v %v", sent, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || keys[1] != inv.Number {
		t.Fatalf("%d deliveries, keys %v", len(bodies), keys)
	}
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write(bodies[1])
	if sigs[1] != "sha256="+hex.EncodeToString(m.Sum(nil)) {
		t.Fatalf("the signature does not verify: %q", sigs[1])
	}
	if !strings.Contains(string(bodies[1]), `"total_cents":500`) {
		t.Fatalf("the payload: %s", bodies[1])
	}
	if _, err := e.Svc.SendInvoice(ctx, boss, org, inv.Number); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a tenant sent an invoice: %v", err)
	}
	e.Svc.VoidInvoice(ctx, e.Admin, org, inv.Number, "test")
	if _, err := e.Svc.SendInvoice(ctx, e.Admin, org, inv.Number); !isInvalid(err) {
		t.Fatalf("a void invoice was sent: %v", err)
	}
}

func TestHOST02_SelfHostedHasNoBilling(t *testing.T) {
	e := testutil.New(t)
	if _, err := e.Svc.GenerateInvoice(ctx, e.Admin, domain.DefaultOrg, "2026-09"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an invoice on a self-hosted installation: %v", err)
	}
	if _, err := e.Svc.SetPrices(ctx, e.Admin, domain.DefaultOrg, domain.Prices{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("prices on a self-hosted installation: %v", err)
	}
	if _, err := e.Svc.ListInvoices(ctx, e.Admin, domain.DefaultOrg); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("invoices on a self-hosted installation: %v", err)
	}
	var n int
	e.Store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM invoice`).Scan(&n)
	if n != 0 {
		t.Fatal("invoice rows on a self-hosted installation")
	}
	_ = time.Now
}
