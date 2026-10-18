// Package calendar holds the work calendar (SPEC 7.2): time zone, week start day, working days and hours, holidays and
// week numbering. Sprint boundaries are computed in local time, so a day may be 23 or 25 hours across a daylight-saving
// change, and are stored as UTC instants together with a snapshot of the calendar.
package calendar

import (
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // the zone database ships in the binary: boundaries must not depend on the host
)

// Calendar is a value that can be stored as JSON and snapshotted with a sprint.
type Calendar struct {
	TimeZone    string             `json:"time_zone"`           // IANA name
	WeekStart   time.Weekday       `json:"week_start"`          // any day
	WorkingDays []time.Weekday     `json:"working_days"`        // any subset
	Hours       float64            `json:"hours_per_day"`       // default 8
	DayHours    map[string]float64 `json:"day_hours,omitempty"` // optional per weekday ("Friday": 4)
	Holidays    []string           `json:"holidays,omitempty"`  // local dates, YYYY-MM-DD
	Numbering   string             `json:"week_numbering"`      // "iso", "first-day" or "none"
}

// Preset returns a calendar for one of the common work weeks: mon-fri, sun-thu, mon-sat.
func Preset(name, tz string) (Calendar, error) {
	c := Calendar{TimeZone: tz, Hours: 8}
	switch name {
	case "mon-fri":
		c.WeekStart, c.WorkingDays, c.Numbering = time.Monday, days(time.Monday, 5), "iso"
	case "sun-thu":
		c.WeekStart, c.WorkingDays, c.Numbering = time.Sunday, days(time.Sunday, 5), "first-day"
	case "mon-sat":
		c.WeekStart, c.WorkingDays, c.Numbering = time.Monday, days(time.Monday, 6), "iso"
	default:
		return c, fmt.Errorf("unknown preset %q (use mon-fri, sun-thu or mon-sat)", name)
	}
	return c, c.Validate()
}

func days(first time.Weekday, n int) []time.Weekday {
	var out []time.Weekday
	for i := 0; i < n; i++ {
		out = append(out, time.Weekday((int(first)+i)%7))
	}
	return out
}

func (c Calendar) Validate() error {
	if _, err := time.LoadLocation(c.TimeZone); err != nil || c.TimeZone == "" {
		return fmt.Errorf("unknown time zone %q", c.TimeZone)
	}
	if len(c.WorkingDays) == 0 {
		return fmt.Errorf("a calendar needs at least one working day")
	}
	if c.Hours <= 0 || c.Hours > 24 {
		return fmt.Errorf("hours per day must be between 0 and 24")
	}
	switch c.Numbering {
	case "iso", "first-day", "none":
	default:
		return fmt.Errorf("week numbering must be iso, first-day or none")
	}
	if c.Numbering == "iso" && c.WeekStart != time.Monday {
		return fmt.Errorf("ISO 8601 week numbers need weeks that start on Monday; use first-day or none")
	}
	for _, h := range c.Holidays {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			return fmt.Errorf("holiday %q is not a date (YYYY-MM-DD)", h)
		}
	}
	return nil
}

func (c Calendar) loc() *time.Location {
	l, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		return time.UTC
	}
	return l
}

// midnight is the first instant of a local date. When the zone skips midnight (clocks jump forward at 00:00) Go's
// time.Date may return an instant on the previous day, so step forward to the first instant that really is on the date.
func (c Calendar) midnight(y int, m time.Month, d int) time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, c.loc())
	want := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3*60; i++ {
		ty, tm, td := t.Date()
		if time.Date(ty, tm, td, 12, 0, 0, 0, time.UTC).Equal(want) {
			break
		}
		t = t.Add(time.Minute)
	}
	return t
}

// LocationOrUTC is the calendar's zone (UTC when the name cannot be loaded).
func (c Calendar) LocationOrUTC() *time.Location { return c.loc() }

// StartOfDay is the first instant of the local day containing t.
func (c Calendar) StartOfDay(t time.Time) time.Time {
	y, m, d := t.In(c.loc()).Date()
	return c.midnight(y, m, d)
}

// WeekStartOf is the first instant of the week (local, on the week start day) containing t.
func (c Calendar) WeekStartOf(t time.Time) time.Time {
	l := t.In(c.loc())
	back := (int(l.Weekday()) - int(c.WeekStart) + 7) % 7
	y, m, d := l.Date()
	return c.midnight(y, m, d-back)
}

// Bounds returns the sprint that covers `weeks` weeks beginning with the week that contains t: start inclusive, end
// exclusive (the next week start). The last instant of the sprint is end minus one second.
func (c Calendar) Bounds(t time.Time, weeks int) (start, end time.Time) {
	if weeks < 1 {
		weeks = 1
	}
	start = c.WeekStartOf(t)
	y, m, d := start.Date()
	end = c.midnight(y, m, d+7*weeks)
	return start, end
}

func (c Calendar) isHoliday(y int, m time.Month, d int) bool {
	key := fmt.Sprintf("%04d-%02d-%02d", y, m, d)
	for _, h := range c.Holidays {
		if h == key {
			return true
		}
	}
	return false
}

func (c Calendar) works(wd time.Weekday) bool {
	for _, w := range c.WorkingDays {
		if w == wd {
			return true
		}
	}
	return false
}

// HoursOn is the working hours of one local date: zero on non-working days and holidays.
func (c Calendar) HoursOn(y int, m time.Month, d int) float64 {
	wd := time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Weekday()
	if !c.works(wd) || c.isHoliday(y, m, d) {
		return 0
	}
	if h, ok := c.DayHours[wd.String()]; ok {
		return h
	}
	return c.Hours
}

// WorkingDaysBetween counts the working days in [from, to) in local dates.
func (c Calendar) WorkingDaysBetween(from, to time.Time) int {
	n := 0
	c.eachDay(from, to, func(y int, m time.Month, d int) {
		if c.HoursOn(y, m, d) > 0 {
			n++
		}
	})
	return n
}

// HoursBetween sums the working hours in [from, to).
func (c Calendar) HoursBetween(from, to time.Time) float64 {
	h := 0.0
	c.eachDay(from, to, func(y int, m time.Month, d int) { h += c.HoursOn(y, m, d) })
	return h
}

func (c Calendar) eachDay(from, to time.Time, f func(int, time.Month, int)) {
	l := c.loc()
	fy, fm, fd := from.In(l).Date()
	ty, tm, td := to.In(l).Date()
	end := time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC)
	for cur := time.Date(fy, fm, fd, 0, 0, 0, 0, time.UTC); cur.Before(end); cur = cur.AddDate(0, 0, 1) {
		f(cur.Year(), cur.Month(), cur.Day())
	}
}

// DaysLeft is the number of working days remaining in a sprint from now (today included while it is a working day).
func (c Calendar) DaysLeft(now, end time.Time) int {
	if !now.Before(end) {
		return 0
	}
	return c.WorkingDaysBetween(c.StartOfDay(now), end)
}

// WeekNumber returns the year and week of t under the calendar's numbering, and false when numbering is "none".
func (c Calendar) WeekNumber(t time.Time) (year, week int, ok bool) {
	l := t.In(c.loc())
	switch c.Numbering {
	case "iso":
		y, w := l.ISOWeek()
		return y, w, true
	case "first-day":
		ws := c.WeekStartOf(t)
		y, m, d := ws.Date()
		we := time.Date(y, m, d+6, 12, 0, 0, 0, time.UTC) // the last day of this week
		if we.Year() != y {                               // a week that straddles the new year is week 1 of the new year
			return we.Year(), 1, true
		}
		first := c.WeekStartOf(time.Date(y, 1, 1, 12, 0, 0, 0, c.loc()))
		fy, fm, fd := first.Date()
		diff := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Sub(time.Date(fy, fm, fd, 0, 0, 0, 0, time.UTC))
		return y, int(diff.Hours()/24)/7 + 1, true
	}
	return 0, 0, false
}

// DefaultNamePattern follows SPEC 7.2: ISO calendars name sprints by week number, others by start date.
func (c Calendar) DefaultNamePattern() string {
	if c.Numbering == "iso" {
		return "Week {year}-W{week}"
	}
	return "Week of {start}"
}

// SprintName fills the tokens {year}, {week}, {start}, {end} and {n}; a week token without numbering falls back to the start date.
func (c Calendar) SprintName(pattern string, start, end time.Time, n int) string {
	if pattern == "" {
		pattern = c.DefaultNamePattern()
	}
	y, w, ok := c.WeekNumber(start)
	last := end.Add(-time.Second).In(c.loc())
	s := start.In(c.loc())
	r := strings.NewReplacer("{start}", s.Format("2006-01-02"), "{end}", last.Format("2006-01-02"), "{n}", fmt.Sprint(n))
	out := r.Replace(pattern)
	if ok {
		out = strings.NewReplacer("{year}", fmt.Sprint(y), "{week}", fmt.Sprintf("%02d", w)).Replace(out)
	} else {
		out = strings.NewReplacer("{year}", fmt.Sprint(s.Year()), "{week}", s.Format("2006-01-02")).Replace(out)
	}
	return out
}

// Capacity is the hours one person can give to [from, to) under their own calendar, minus hours already booked off.
func Capacity(person Calendar, from, to time.Time, offHours float64) float64 {
	h := person.HoursBetween(from, to) - offHours
	if h < 0 {
		return 0
	}
	return h
}

// Normalize sorts and de-duplicates holidays so snapshots compare equal.
func (c Calendar) Normalize() Calendar {
	seen := map[string]bool{}
	var hs []string
	for _, h := range c.Holidays {
		if !seen[h] {
			seen[h] = true
			hs = append(hs, h)
		}
	}
	sort.Strings(hs)
	c.Holidays = hs
	wd := append([]time.Weekday(nil), c.WorkingDays...)
	sort.Slice(wd, func(i, j int) bool { return wd[i] < wd[j] })
	c.WorkingDays = wd
	return c
}
