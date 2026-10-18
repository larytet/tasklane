package calendar

import "time"

// Working time (SPEC 15, service levels): minutes counted only inside the working day. A working day is the window that begins at the
// calendar's Start (09:00 when unset) and lasts that day's working hours; non-working days and holidays have no window. The window's end
// is its start plus its length, so on a day the clocks change it is that many real hours after the start.

func (c Calendar) startMinutes() int {
	t, err := time.Parse("15:04", c.Start)
	if err != nil {
		return 9 * 60
	}
	return t.Hour()*60 + t.Minute()
}

// window is the working window of a local date, or ok=false when the date has no working time.
func (c Calendar) window(y int, m time.Month, d int) (from, to time.Time, ok bool) {
	h := c.HoursOn(y, m, d)
	if h <= 0 {
		return time.Time{}, time.Time{}, false
	}
	sm := c.startMinutes()
	from = time.Date(y, m, d, sm/60, sm%60, 0, 0, c.loc())
	return from, from.Add(time.Duration(h * float64(time.Hour))), true
}

// WorkingMinutesBetween is the working minutes in [from, to).
func (c Calendar) WorkingMinutesBetween(from, to time.Time) float64 {
	if !to.After(from) {
		return 0
	}
	l := c.loc()
	fy, fm, fd := from.In(l).Date()
	ty, tm, td := to.In(l).Date()
	last := time.Date(ty, tm, td, 0, 0, 0, 0, time.UTC)
	total := 0.0
	for cur := time.Date(fy, fm, fd, 0, 0, 0, 0, time.UTC); !cur.After(last); cur = cur.AddDate(0, 0, 1) {
		ws, we, ok := c.window(cur.Year(), cur.Month(), cur.Day())
		if !ok {
			continue
		}
		lo, hi := ws, we
		if from.After(lo) {
			lo = from
		}
		if to.Before(hi) {
			hi = to
		}
		if hi.After(lo) {
			total += hi.Sub(lo).Minutes()
		}
	}
	return total
}

// AddWorkingMinutes is the instant at which `minutes` of working time have passed after `from`; the zero time when there is no working
// time within ten years (a calendar whose every day is a holiday).
func (c Calendar) AddWorkingMinutes(from time.Time, minutes float64) time.Time {
	if minutes <= 0 {
		return from
	}
	l := c.loc()
	fy, fm, fd := from.In(l).Date()
	remaining := time.Duration(minutes * float64(time.Minute))
	cur := time.Date(fy, fm, fd, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3660; i++ {
		if ws, we, ok := c.window(cur.Year(), cur.Month(), cur.Day()); ok {
			lo := ws
			if from.After(lo) {
				lo = from
			}
			if we.After(lo) {
				if avail := we.Sub(lo); avail >= remaining {
					return lo.Add(remaining)
				} else {
					remaining -= avail
				}
			}
		}
		cur = cur.AddDate(0, 0, 1)
	}
	return time.Time{}
}
