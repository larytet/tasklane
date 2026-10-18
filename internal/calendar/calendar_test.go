package calendar

import (
	"testing"
	"time"
)

func mustCal(t *testing.T, preset, tz string) Calendar {
	c, err := Preset(preset, tz)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func at(t *testing.T, tz, s string) time.Time {
	l, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	v, err := time.ParseInLocation("2006-01-02 15:04", s, l)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// CORE-13: week boundaries fall at local midnight on the right start day, for several work weeks and zones.
func TestCORE13_BoundariesAndWorkingDays(t *testing.T) {
	cases := []struct {
		preset, tz, now string
		start           string
		days            int
	}{
		{"mon-fri", "Europe/Berlin", "2026-10-14 15:00", "2026-10-12 00:00", 5},
		{"sun-thu", "Asia/Jerusalem", "2026-10-21 15:00", "2026-10-18 00:00", 5}, // Sunday start, Sun-Thu: the spec's own example
		{"mon-sat", "America/New_York", "2026-10-24 23:59", "2026-10-19 00:00", 6},
		{"sun-thu", "Asia/Kolkata", "2026-10-18 00:00", "2026-10-18 00:00", 5},   // half-hour offset
		{"mon-fri", "Asia/Kathmandu", "2026-10-25 23:59", "2026-10-19 00:00", 5}, // 45-minute offset
		{"mon-fri", "Pacific/Auckland", "2026-10-19 00:00", "2026-10-19 00:00", 5},
	}
	for _, c := range cases {
		cal := mustCal(t, c.preset, c.tz)
		start, end := cal.Bounds(at(t, c.tz, c.now), 1)
		if want := at(t, c.tz, c.start); !start.Equal(want) {
			t.Errorf("%s %s: start %v want %v", c.preset, c.tz, start, want)
		}
		if got := end.Sub(start); got != 7*24*time.Hour { // none of these weeks crosses a clock change
			t.Errorf("%s %s: length %v", c.preset, c.tz, got)
		}
		if n := cal.WorkingDaysBetween(start, end); n != c.days {
			t.Errorf("%s %s: %d working days, want %d", c.preset, c.tz, n, c.days)
		}
	}
	// week start Saturday, an unusual but legal setting
	sat := Calendar{TimeZone: "UTC", WeekStart: time.Saturday, WorkingDays: days(time.Sunday, 5), Hours: 8, Numbering: "none"}
	if s, _ := sat.Bounds(at(t, "UTC", "2026-10-21 10:00"), 1); s.Weekday() != time.Saturday || s.Day() != 17 {
		t.Errorf("saturday start: %v", s)
	}
	// the last instant belongs to the sprint and the next instant to the next one
	cal := mustCal(t, "mon-fri", "Europe/Berlin")
	_, end := cal.Bounds(at(t, "Europe/Berlin", "2026-10-21 10:00"), 1)
	if s2, _ := cal.Bounds(end.Add(-time.Second), 1); s2.Equal(end) {
		t.Error("the last second fell in the next sprint")
	}
	if s2, _ := cal.Bounds(end, 1); !s2.Equal(end) {
		t.Error("the boundary instant did not start the next sprint")
	}
	// bi-weekly and custom lengths
	s, e := cal.Bounds(at(t, "Europe/Berlin", "2026-10-21 10:00"), 2)
	if cal.WorkingDaysBetween(s, e) != 10 {
		t.Error("two weeks should have ten working days")
	}
}

func TestCORE13_DaylightSavingAndZones(t *testing.T) {
	cal := mustCal(t, "mon-fri", "Europe/Berlin")
	// clocks go back on 2026-10-25: the week starting Monday 19th is 7 days + 1 hour
	s, e := cal.Bounds(at(t, "Europe/Berlin", "2026-10-21 10:00"), 1)
	if e.Sub(s) != 7*24*time.Hour+time.Hour {
		t.Errorf("fall back week: %v", e.Sub(s))
	}
	// forward: 2026-03-29, the week of the 23rd is 7 days - 1 hour
	s, e = cal.Bounds(at(t, "Europe/Berlin", "2026-03-25 10:00"), 1)
	if e.Sub(s) != 7*24*time.Hour-time.Hour {
		t.Errorf("spring forward week: %v", e.Sub(s))
	}
	if h, m, _ := e.In(cal.loc()).Clock(); h != 0 || m != 0 {
		t.Errorf("the boundary is not local midnight: %v", e)
	}
	// a Sunday-start board whose week starts on the day the clocks change
	us := mustCal(t, "sun-thu", "America/New_York")
	s, e = us.Bounds(at(t, "America/New_York", "2026-03-10 10:00"), 1) // DST begins Sunday 2026-03-08
	if s.Day() != 8 || e.Sub(s) != 7*24*time.Hour-time.Hour {
		t.Errorf("%v %v", s, e.Sub(s))
	}
	// midnight that does not exist (clocks skip 00:00 in Sao Paulo on 2018-11-04): the day starts at the first instant that exists
	br := Calendar{TimeZone: "America/Sao_Paulo", WeekStart: time.Sunday, WorkingDays: days(time.Monday, 5), Hours: 8, Numbering: "none"}
	s, _ = br.Bounds(at(t, "America/Sao_Paulo", "2018-11-06 10:00"), 1)
	if y, m, d := s.In(br.loc()).Date(); y != 2018 || m != 11 || d != 4 || s.In(br.loc()).Hour() > 1 {
		t.Errorf("skipped midnight: %v", s)
	}
	if !s.After(at(t, "America/Sao_Paulo", "2018-11-03 23:59")) {
		t.Error("the week starts before the previous day ended")
	}
	// stored as UTC instants: the snapshot survives a zone database change
	if s.UTC().Location() != time.UTC {
		t.Error("not UTC")
	}
}

func TestCORE13_WeekNumbering(t *testing.T) {
	iso := mustCal(t, "mon-fri", "UTC")
	for in, want := range map[string][2]int{
		"2026-12-31 12:00": {2026, 53}, // 2026 has an ISO week 53
		"2027-01-03 12:00": {2026, 53},
		"2027-01-04 12:00": {2027, 1},
		"2024-12-30 12:00": {2025, 1}, // the Monday belongs to next year's week 1
	} {
		y, w, ok := iso.WeekNumber(at(t, "UTC", in))
		if !ok || y != want[0] || w != want[1] {
			t.Errorf("iso %s: %d-W%d", in, y, w)
		}
	}
	us := mustCal(t, "sun-thu", "UTC") // first-day-of-year: the week containing 1 January is week 1
	for in, want := range map[string][2]int{
		"2026-01-01 12:00": {2026, 1}, // Thursday
		"2026-01-04 12:00": {2026, 2}, // the next Sunday
		"2026-12-27 12:00": {2027, 1}, // the week of 27 Dec 2026 holds 1 Jan 2027
		"2026-12-20 12:00": {2026, 52},
	} {
		y, w, ok := us.WeekNumber(at(t, "UTC", in))
		if !ok || y != want[0] || w != want[1] {
			t.Errorf("first-day %s: %d-W%d want %v", in, y, w, want)
		}
	}
	none := Calendar{TimeZone: "UTC", WeekStart: time.Sunday, WorkingDays: days(time.Sunday, 5), Hours: 8, Numbering: "none"}
	if _, _, ok := none.WeekNumber(at(t, "UTC", "2026-10-18 12:00")); ok {
		t.Error("numbering none returned a number")
	}
	// ISO numbering with a Sunday start is refused rather than silently wrong
	bad := us
	bad.Numbering = "iso"
	if bad.Validate() == nil {
		t.Error("ISO numbers on Sunday weeks were accepted")
	}
}

func TestCORE13_NamingTokens(t *testing.T) {
	iso := mustCal(t, "mon-fri", "Europe/Berlin")
	s, e := iso.Bounds(at(t, "Europe/Berlin", "2026-10-21 10:00"), 1)
	if got := iso.SprintName("", s, e, 3); got != "Week 2026-W43" { // the spec's example
		t.Errorf("%q", got)
	}
	il := mustCal(t, "sun-thu", "Asia/Jerusalem")
	s, e = il.Bounds(at(t, "Asia/Jerusalem", "2026-10-21 10:00"), 1)
	if got := il.SprintName("", s, e, 1); got != "Week of 2026-10-18" { // and the other
		t.Errorf("%q", got)
	}
	if got := il.SprintName("Sprint {n}: {start} to {end} ({year}-{week})", s, e, 7); got != "Sprint 7: 2026-10-18 to 2026-10-24 (2026-43)" {
		t.Errorf("%q", got)
	}
	none := Calendar{TimeZone: "UTC", WeekStart: time.Sunday, WorkingDays: days(time.Sunday, 5), Hours: 8, Numbering: "none"}
	s, e = none.Bounds(at(t, "UTC", "2026-10-21 10:00"), 1)
	if got := none.SprintName("W{week}", s, e, 1); got != "W2026-10-18" {
		t.Errorf("a week token without numbering should use the start date: %q", got)
	}
}

func TestCORE13_HolidaysHoursAndCapacity(t *testing.T) {
	cal := mustCal(t, "mon-fri", "Europe/Berlin")
	cal.Holidays = []string{"2026-10-21"}
	cal.DayHours = map[string]float64{"Friday": 4}
	s, e := cal.Bounds(at(t, "Europe/Berlin", "2026-10-21 10:00"), 1)
	if n := cal.WorkingDaysBetween(s, e); n != 4 {
		t.Errorf("%d working days with one holiday", n)
	}
	if h := cal.HoursBetween(s, e); h != 3*8+4 { // Mon, Tue, Thu at 8, Friday at 4, Wednesday off
		t.Errorf("%v hours", h)
	}
	if d := cal.DaysLeft(at(t, "Europe/Berlin", "2026-10-22 09:00"), e); d != 2 { // Thu and Fri
		t.Errorf("days left %d", d)
	}
	if d := cal.DaysLeft(at(t, "Europe/Berlin", "2026-10-24 09:00"), e); d != 0 { // Saturday: nothing left
		t.Errorf("days left on the weekend %d", d)
	}
	if cal.DaysLeft(e, e) != 0 {
		t.Error("after the end")
	}
	// capacity uses the person's own calendar: a colleague in Israel works Sun-Thu with their own holiday
	il := mustCal(t, "sun-thu", "Asia/Jerusalem")
	il.Holidays = []string{"2026-10-19"}
	if h := Capacity(il, s, e, 0); h != 32 { // Sun-Thu: Monday is a holiday, so Tue, Wed, Thu and the following Sunday are left = 4 days
		t.Errorf("israeli capacity %v", h)
	}
	if h := Capacity(cal, s, e, 100); h != 0 {
		t.Error("capacity went negative")
	}
	// holidays are normalized for snapshots
	cal.Holidays = []string{"2026-12-25", "2026-10-21", "2026-10-21"}
	if n := cal.Normalize(); len(n.Holidays) != 2 || n.Holidays[0] != "2026-10-21" {
		t.Errorf("%v", n.Holidays)
	}
	// validation
	for name, c := range map[string]Calendar{
		"zone":    {TimeZone: "Nowhere/City", WorkingDays: days(time.Monday, 5), Hours: 8, Numbering: "none"},
		"days":    {TimeZone: "UTC", Hours: 8, Numbering: "none"},
		"hours":   {TimeZone: "UTC", WorkingDays: days(time.Monday, 5), Hours: 30, Numbering: "none"},
		"holiday": {TimeZone: "UTC", WorkingDays: days(time.Monday, 5), Hours: 8, Numbering: "none", Holidays: []string{"31/12"}},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Preset("nonsense", "UTC"); err == nil {
		t.Error("an unknown preset was accepted")
	}
}

// PORT-08 (calendar-aware clocks): minutes are counted inside the working window only; weekends, holidays and daylight-saving changes.
func TestSLA_WorkingMinutes(t *testing.T) {
	c, err := Preset("mon-fri", "Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	c.Start = "09:00" // 09:00 to 17:00
	loc := c.LocationOrUTC()
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, loc) }
	// Wednesday 2026-03-04: 10:00 to 12:30 is 150 minutes; outside the window counts nothing
	if got := c.WorkingMinutesBetween(at(2026, 3, 4, 10, 0), at(2026, 3, 4, 12, 30)); got != 150 {
		t.Fatalf("in a day: %v", got)
	}
	if got := c.WorkingMinutesBetween(at(2026, 3, 4, 6, 0), at(2026, 3, 4, 8, 59)); got != 0 {
		t.Fatalf("before the day: %v", got)
	}
	// Friday 16:00 to Monday 10:00: 60 + 60
	if got := c.WorkingMinutesBetween(at(2026, 3, 6, 16, 0), at(2026, 3, 9, 10, 0)); got != 120 {
		t.Fatalf("over a weekend: %v", got)
	}
	// adding: 60 minutes from Friday 16:30 ends Monday 09:30; 8 hours from Monday 09:00 ends 17:00 the same day
	if got := c.AddWorkingMinutes(at(2026, 3, 6, 16, 30), 60); !got.Equal(at(2026, 3, 9, 9, 30)) {
		t.Fatalf("add over a weekend: %v", got)
	}
	if got := c.AddWorkingMinutes(at(2026, 3, 9, 9, 0), 480); !got.Equal(at(2026, 3, 9, 17, 0)) {
		t.Fatalf("a whole day: %v", got)
	}
	if got := c.AddWorkingMinutes(at(2026, 3, 9, 3, 0), 30); !got.Equal(at(2026, 3, 9, 9, 30)) {
		t.Fatalf("starting before the day: %v", got)
	}
	// a holiday has no working time
	c.Holidays = []string{"2026-03-09"}
	if got := c.AddWorkingMinutes(at(2026, 3, 6, 16, 30), 60); !got.Equal(at(2026, 3, 10, 9, 30)) {
		t.Fatalf("over a holiday: %v", got)
	}
	if got := c.WorkingMinutesBetween(at(2026, 3, 9, 0, 0), at(2026, 3, 10, 0, 0)); got != 0 {
		t.Fatalf("a holiday counts: %v", got)
	}
	// the clocks go forward on Sunday 2026-03-29: Monday's window is unaffected and the add works across it
	c.Holidays = nil
	if got := c.AddWorkingMinutes(at(2026, 3, 27, 16, 0), 120); !got.Equal(at(2026, 3, 30, 10, 0)) {
		t.Fatalf("across the change: %v", got)
	}
	// a calendar that never works gives no answer
	h := Calendar{TimeZone: "UTC", WorkingDays: []time.Weekday{time.Monday}, Hours: 8, Numbering: "none"}
	for d := 0; d < 4000; d += 7 {
		h.Holidays = append(h.Holidays, time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).AddDate(0, 0, d).Format("2006-01-02"))
	}
	if got := h.AddWorkingMinutes(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 10); !got.IsZero() {
		t.Fatalf("no working time: %v", got)
	}
	// a start that runs the day past midnight is refused
	c.Start = "20:00"
	if err := c.Validate(); err == nil {
		t.Fatal("a window past midnight was accepted")
	}
	c.Start = "9am"
	if err := c.Validate(); err == nil {
		t.Fatal("a bad start was accepted")
	}
}
