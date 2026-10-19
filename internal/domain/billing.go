package domain

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/egress"
)

// Billing for the hosted edition (SPEC 17.9). Tasklane does not take payment: it prices what an organization used and hands the invoice to whatever bills
// customers (an accounting system, a payment provider) through a signed webhook. Everything is inactive unless `hosted` is on; prices are zero until the
// operator sets them, and an organization with no plan cannot be invoiced.
//
// An invoice is made for one calendar month from the daily usage snapshots (RecordUsage): the base price, the average number of seats, the average
// gigabytes (GiB) of attachments, and the most active customers in the month (customers are priced apart from seats). Money is whole cents, and every
// line is rounded half up on its own. The invoice keeps the plan and prices it was made under and is never recalculated; to change one it is voided and made again.

type InvoiceLine struct {
	Item        string `json:"item"`
	Quantity    string `json:"quantity"` // as printed: "12.40 seats on average"
	UnitCents   int64  `json:"unit_cents"`
	AmountCents int64  `json:"amount_cents"`
}

type Invoice struct {
	ID         string        `json:"id"`
	Number     string        `json:"number"`
	OrgID      string        `json:"org_id"`
	Org        string        `json:"organization"`
	Period     string        `json:"period"`
	Status     string        `json:"status"`
	Currency   string        `json:"currency"`
	TotalCents int64         `json:"total_cents"`
	Lines      []InvoiceLine `json:"lines"`
	Usage      InvoiceUsage  `json:"usage"`
	Plan       OrgPlan       `json:"plan"`
	Created    time.Time     `json:"created_at"`
	Sent       *time.Time    `json:"sent_at,omitempty"`
	VoidReason string        `json:"void_reason,omitempty"`
}

type InvoiceUsage struct {
	Days          int     `json:"days_recorded"`
	DaysInPeriod  int     `json:"days_in_period"`
	SeatsAverage  float64 `json:"seats_average"`
	SeatsPeak     int     `json:"seats_peak"`
	GiBAverage    float64 `json:"gib_average"`
	CustomersPeak int     `json:"active_customers_peak"`
	ProjectsPeak  int     `json:"projects_peak"`
}

var periodRe = regexp.MustCompile(`^(\d{4})-(0[1-9]|1[0-2])$`)

// BillingSink receives a finished invoice (an accounting system, a payment provider). It is optional.
type BillingSink interface {
	Send(ctx context.Context, inv Invoice) error
}

// WebhookBilling posts the invoice as JSON to a URL, signed with HMAC-SHA-256 of the body in X-Tasklane-Signature, through the egress policy.
type WebhookBilling struct {
	URL, Secret  string
	AllowPrivate bool
}

func (w WebhookBilling) Send(ctx context.Context, inv Invoice) error {
	b, _ := json.Marshal(inv)
	m := hmac.New(sha256.New, []byte(w.Secret))
	m.Write(b)
	req, err := http.NewRequestWithContext(ctx, "POST", w.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tasklane-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
	req.Header.Set("Idempotency-Key", inv.Number) // a repeat of the same invoice is the receiver's to recognise
	c := egress.Policy{AllowPrivate: w.AllowPrivate, MaxBody: 1 << 20}.Client()
	c.Timeout = 15 * time.Second
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the billing endpoint answered %d", resp.StatusCode)
	}
	return nil
}

func round(num, den int64) int64 { // half up, for non-negative numbers
	if den == 0 {
		return 0
	}
	return (2*num + den) / (2 * den)
}

// SetPrices sets an organization's prices (the plan must exist); the operator only.
type Prices struct {
	Currency      string `json:"currency"`
	BaseCents     int64  `json:"price_base_cents"`
	SeatCents     int64  `json:"price_seat_cents"`
	GiBCents      int64  `json:"price_gib_cents"`
	CustomerCents int64  `json:"price_customer_cents"`
}

func (s *Service) SetPrices(ctx context.Context, a Actor, orgID string, p Prices) (OrgPlan, error) {
	if err := s.hostedOnly(a); err != nil {
		return OrgPlan{}, err
	}
	p.Currency = strings.ToLower(strings.TrimSpace(p.Currency))
	if p.Currency == "" {
		p.Currency = "usd"
	}
	if len(p.Currency) != 3 {
		return OrgPlan{}, invalid("a currency is a three letter code such as usd or eur")
	}
	if p.BaseCents < 0 || p.SeatCents < 0 || p.GiBCents < 0 || p.CustomerCents < 0 || p.BaseCents > 1e12 || p.SeatCents > 1e9 || p.GiBCents > 1e9 || p.CustomerCents > 1e9 {
		return OrgPlan{}, invalid("prices are zero or more, in whole cents")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE org_plan SET currency=$2,price_base_cents=$3,price_seat_cents=$4,price_gib_cents=$5,price_customer_cents=$6,updated_at=now() WHERE org_id::text=$1`,
			orgID, p.Currency, p.BaseCents, p.SeatCents, p.GiBCents, p.CustomerCents)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invalid("give the organization a plan before its prices")
		}
		return s.audit(ctx, tx, a, "org.prices_set", orgID, map[string]any{"currency": p.Currency, "base": p.BaseCents, "seat": p.SeatCents, "gib": p.GiBCents, "customer": p.CustomerCents})
	})
	if err != nil {
		return OrgPlan{}, err
	}
	return s.orgPlan(ctx, orgID), nil
}

func (s *Service) pricesOf(ctx context.Context, orgID string) (Prices, bool) {
	var p Prices
	err := s.DB.QueryRow(ctx, `SELECT currency,price_base_cents,price_seat_cents,price_gib_cents,price_customer_cents FROM org_plan WHERE org_id::text=$1`, orgID).Scan(&p.Currency, &p.BaseCents, &p.SeatCents, &p.GiBCents, &p.CustomerCents)
	return p, err == nil
}

// GenerateInvoice prices a finished or running calendar month. The month's snapshots must exist; a month that has not ended yields an invoice that says so only by its usage figures.
func (s *Service) GenerateInvoice(ctx context.Context, a Actor, orgID, period string) (Invoice, error) {
	if err := s.hostedOnly(a); err != nil {
		return Invoice{}, err
	}
	m := periodRe.FindStringSubmatch(period)
	if m == nil {
		return Invoice{}, invalid("the period is a month such as 2026-10")
	}
	if !s.orgExists(ctx, orgID) {
		return Invoice{}, ErrNotFound
	}
	pr, ok := s.pricesOf(ctx, orgID)
	if !ok {
		return Invoice{}, invalid("this organization has no plan: set one, and its prices, first")
	}
	start, _ := time.Parse("2006-01", period)
	days := start.AddDate(0, 1, 0).Sub(start).Hours() / 24
	var u InvoiceUsage
	var sumSeats, sumBytes int64
	err := s.DB.QueryRow(ctx, `SELECT count(*),COALESCE(sum(seats),0),COALESCE(max(seats),0),COALESCE(sum(storage_bytes),0),COALESCE(max(active_customers),0),COALESCE(max(projects),0)
		FROM usage_snapshot WHERE org_id::text=$1 AND day >= $2::date AND day < ($2::date + interval '1 month')`, orgID, start.Format("2006-01-02")).
		Scan(&u.Days, &sumSeats, &u.SeatsPeak, &sumBytes, &u.CustomersPeak, &u.ProjectsPeak)
	if err != nil {
		return Invoice{}, err
	}
	if u.Days == 0 {
		return Invoice{}, invalid("no usage was recorded for %s: there is nothing to invoice", period)
	}
	u.DaysInPeriod = int(days)
	u.SeatsAverage = float64(sumSeats) / float64(u.Days)
	u.GiBAverage = float64(sumBytes) / float64(u.Days) / (1 << 30)
	plan := s.orgPlan(ctx, orgID)

	var lines []InvoiceLine
	add := func(item, qty string, unit, amount int64) {
		if amount != 0 || unit != 0 {
			lines = append(lines, InvoiceLine{Item: item, Quantity: qty, UnitCents: unit, AmountCents: amount})
		}
	}
	add("Base", "1 month", pr.BaseCents, pr.BaseCents)
	add("Seats", fmt.Sprintf("%.2f on average", u.SeatsAverage), pr.SeatCents, round(sumSeats*pr.SeatCents, int64(u.Days)))
	// gibibytes in hundredths, to keep the arithmetic in integers: (average bytes * price) / 2^30
	add("Attachment storage", fmt.Sprintf("%.2f GiB on average", u.GiBAverage), pr.GiBCents, round(sumBytes/int64(u.Days)*pr.GiBCents, 1<<30))
	add("Active customers", fmt.Sprintf("%d at most", u.CustomersPeak), pr.CustomerCents, int64(u.CustomersPeak)*pr.CustomerCents)
	var total int64
	for _, l := range lines {
		total += l.AmountCents
	}
	lb, _ := json.Marshal(lines)
	ub, _ := json.Marshal(u)
	pb, _ := json.Marshal(plan)
	var inv Invoice
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var seq int
		tx.QueryRow(ctx, `SELECT count(*)+1 FROM invoice WHERE period=$1`, period).Scan(&seq)
		number := fmt.Sprintf("TL-%s-%04d", strings.ReplaceAll(period, "-", ""), seq)
		if err := tx.QueryRow(ctx, `INSERT INTO invoice(org_id,number,period,currency,total_cents,lines,usage,plan) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`,
			orgID, number, period, pr.Currency, total, lb, ub, pb).Scan(&inv.ID); err != nil {
			if strings.Contains(err.Error(), "invoice_period_live") {
				return &ConflictError{"an invoice for " + period + " exists: void it to make another"}
			}
			return err
		}
		return s.audit(ctx, tx, a, "invoice.created", number, map[string]any{"org": orgID, "period": period, "total_cents": total})
	})
	if err != nil {
		return Invoice{}, err
	}
	return s.loadInvoice(ctx, inv.ID)
}

func (s *Service) loadInvoice(ctx context.Context, id string) (Invoice, error) {
	var inv Invoice
	var lb, ub, pb []byte
	err := s.DB.QueryRow(ctx, `SELECT i.id::text,i.number,i.org_id::text,o.name,i.period,i.status,i.currency,i.total_cents,i.lines,i.usage,i.plan,i.created_at,i.sent_at,i.void_reason
		FROM invoice i JOIN organization o ON o.id=i.org_id WHERE i.id::text=$1 OR i.number=$1`, id).
		Scan(&inv.ID, &inv.Number, &inv.OrgID, &inv.Org, &inv.Period, &inv.Status, &inv.Currency, &inv.TotalCents, &lb, &ub, &pb, &inv.Created, &inv.Sent, &inv.VoidReason)
	if err != nil {
		return inv, ErrNotFound
	}
	json.Unmarshal(lb, &inv.Lines)
	json.Unmarshal(ub, &inv.Usage)
	json.Unmarshal(pb, &inv.Plan)
	return inv, nil
}

func (s *Service) mayReadInvoices(a Actor, orgID string) bool {
	return a.Admin || (a.OrgAdmin && a.OrgID() == orgID)
}

// ListInvoices lists an organization's invoices, newest first. The operator may list any; an organization's administrator their own.
func (s *Service) ListInvoices(ctx context.Context, a Actor, orgID string) ([]Invoice, error) {
	if !s.Hosted {
		return nil, ErrNotHosted
	}
	if !s.mayReadInvoices(a, orgID) {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM invoice WHERE org_id::text=$1 ORDER BY period DESC, number DESC`, orgID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []Invoice{}
	for _, id := range ids {
		inv, err := s.loadInvoice(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}

// GetInvoice returns one invoice by number.
func (s *Service) GetInvoice(ctx context.Context, a Actor, orgID, number string) (Invoice, error) {
	if !s.Hosted {
		return Invoice{}, ErrNotHosted
	}
	if !s.mayReadInvoices(a, orgID) {
		return Invoice{}, ErrForbidden
	}
	inv, err := s.loadInvoice(ctx, number)
	if err != nil || inv.OrgID != orgID {
		return Invoice{}, ErrNotFound
	}
	return inv, nil
}

// SendInvoice hands an open invoice to the billing sink and marks it sent. A failure leaves it open to try again; the sink recognises a repeat by its number.
func (s *Service) SendInvoice(ctx context.Context, a Actor, orgID, number string) (Invoice, error) {
	if err := s.hostedOnly(a); err != nil {
		return Invoice{}, err
	}
	inv, err := s.GetInvoice(ctx, a, orgID, number)
	if err != nil {
		return inv, err
	}
	if inv.Status == "void" {
		return inv, invalid("a void invoice is not sent")
	}
	if s.Billing == nil {
		return inv, invalid("no billing endpoint is configured (billing_webhook_url)")
	}
	if err := s.Billing.Send(ctx, inv); err != nil {
		return inv, &UpstreamError{Msg: "the invoice was not delivered: " + err.Error()}
	}
	s.tx(ctx, func(tx pgx.Tx) error {
		tx.Exec(ctx, `UPDATE invoice SET status='sent', sent_at=COALESCE(sent_at,$2) WHERE id::text=$1`, inv.ID, s.Now())
		return s.audit(ctx, tx, a, "invoice.sent", number, map[string]any{"org": orgID})
	})
	return s.loadInvoice(ctx, inv.ID)
}

// VoidInvoice cancels an invoice (with a reason) so that the month can be invoiced again.
func (s *Service) VoidInvoice(ctx context.Context, a Actor, orgID, number, reason string) (Invoice, error) {
	if err := s.hostedOnly(a); err != nil {
		return Invoice{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Invoice{}, invalid("say why the invoice is void")
	}
	inv, err := s.GetInvoice(ctx, a, orgID, number)
	if err != nil {
		return inv, err
	}
	if inv.Status == "void" {
		return inv, nil
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE invoice SET status='void', void_reason=$2 WHERE id::text=$1`, inv.ID, reason); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "invoice.void", number, map[string]any{"org": orgID, "reason": reason})
	})
	if err != nil {
		return inv, err
	}
	return s.loadInvoice(ctx, inv.ID)
}

// InvoiceCSV renders an invoice for people and spreadsheets.
func InvoiceCSV(inv Invoice) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Write([]string{"invoice", inv.Number, "organization", inv.Org, "period", inv.Period, "currency", strings.ToUpper(inv.Currency), "status", inv.Status})
	w.Write([]string{"item", "quantity", "unit_cents", "amount_cents"})
	for _, l := range inv.Lines {
		w.Write([]string{l.Item, l.Quantity, fmt.Sprint(l.UnitCents), fmt.Sprint(l.AmountCents)})
	}
	w.Write([]string{"total", "", "", fmt.Sprint(inv.TotalCents)})
	w.Flush()
	return b.String()
}

// UpstreamError is a failure of something this installation depends on and does not run (HTTP 502).
type UpstreamError struct{ Msg string }

func (e *UpstreamError) Error() string { return e.Msg }

// RunBillingOnce makes and delivers the invoices that are owed: for every organization with a plan and a price, the month before this one, once, as soon as
// the month is over, and the earlier months (up to a year back) in which usage was recorded and nothing was invoiced. An invoice that was made but could not be delivered is delivered at a later call. Nothing is made for an organization with no
// prices (nothing to charge) or no recorded usage in that month. Safe from every node: the one live invoice per month is the database's rule.
func (s *Service) RunBillingOnce(ctx context.Context) (made, sent int) {
	if !s.Hosted {
		return
	}
	operator := Actor{Admin: true, Label: "billing"}
	prev := s.Now().UTC().AddDate(0, -1, 0).Format("2006-01")
	rows, err := s.DB.Query(ctx, `SELECT org_id::text FROM org_plan WHERE price_base_cents+price_seat_cents+price_gib_cents+price_customer_cents > 0`)
	if err != nil {
		return
	}
	var orgs []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		orgs = append(orgs, id)
	}
	rows.Close()
	for _, org := range orgs {
		// months missed while no node was running (up to a year back): usage was recorded and no invoice, not even a voided one, exists
		for _, month := range s.unbilledMonths(ctx, org, prev) {
			if inv, gerr := s.GenerateInvoice(ctx, operator, org, month); gerr == nil {
				made++
				if s.Billing != nil {
					if _, err := s.SendInvoice(ctx, operator, org, inv.Number); err == nil {
						sent++
					}
				}
			}
		}
		var have string
		var status string
		err := s.DB.QueryRow(ctx, `SELECT number,status FROM invoice WHERE org_id::text=$1 AND period=$2 AND status<>'void'`, org, prev).Scan(&have, &status)
		if err != nil {
			inv, gerr := s.GenerateInvoice(ctx, operator, org, prev)
			if gerr != nil {
				continue // no usage recorded for the month, or someone else made it a moment ago
			}
			made++
			have, status = inv.Number, inv.Status
		}
		if status == "open" && s.Billing != nil {
			if _, err := s.SendInvoice(ctx, operator, org, have); err == nil {
				sent++
			}
		}
	}
	return
}

// unbilledMonths lists, oldest first, the months before prev (back to twelve months before it) in which the organization has usage
// recorded and no invoice at all.
func (s *Service) unbilledMonths(ctx context.Context, org, prev string) []string {
	rows, err := s.DB.Query(ctx, `SELECT to_char(m,'YYYY-MM') FROM (
		SELECT DISTINCT date_trunc('month', day) AS m FROM usage_snapshot WHERE org_id::text=$1
			AND day >= (to_date($2||'-01','YYYY-MM-DD') - interval '12 months') AND day < to_date($2||'-01','YYYY-MM-DD')) u
		WHERE NOT EXISTS (SELECT 1 FROM invoice i WHERE i.org_id::text=$1 AND i.period=to_char(u.m,'YYYY-MM')) ORDER BY m`, org, prev)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		rows.Scan(&m)
		out = append(out, m)
	}
	return out
}

// RunBilling runs RunBillingOnce every hour (hosted installations only).
func (s *Service) RunBilling(ctx context.Context, logf func(string, ...any)) {
	if !s.Hosted {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if m, n := s.RunBillingOnce(ctx); m+n > 0 {
			logf("billing: %d invoice(s) made, %d delivered", m, n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
