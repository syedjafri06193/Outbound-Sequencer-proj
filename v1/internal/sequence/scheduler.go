// Package sequence schedules steps into recipient-local business hours.
package sequence

import (
	"math/rand"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
)

// HolidayCalendar knows which days are business days where the recipient
// is.
//
// Per-country, not global. Emailing a German prospect on a German public
// holiday, or a US prospect on July 4th, is a small but real signal that
// nobody is paying attention.
type HolidayCalendar interface {
	IsHoliday(country string, day time.Time) bool
}

// Scheduler computes send times.
type Scheduler struct {
	cal HolidayCalendar
	rng *rand.Rand
}

func NewScheduler(cal HolidayCalendar, seed int64) *Scheduler {
	return &Scheduler{cal: cal, rng: rand.New(rand.NewSource(seed))}
}

// IsBusinessDay reports whether a local day is a working day.
func (s *Scheduler) IsBusinessDay(country string, t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	if s.cal != nil && s.cal.IsHoliday(country, t) {
		return false
	}
	return true
}

// MaxSearchDays bounds the forward scan.
//
// Without it, a calendar that marks every day a holiday -- a
// misconfiguration, or a country code nobody implemented -- spins forever
// inside the scheduler. A bounded scan that gives up loudly is better than
// a hung worker.
const MaxSearchDays = 400

// NextSendTime returns the next moment inside the recipient's local
// business window.
//
// The jitter is applied BEFORE the window-end check, not after.
//
// The obvious implementation adds jitter to the returned time as a last
// step, which can push a send past the end of business hours: a slot found
// at 16:58 plus up to 30 minutes of jitter lands at 17:20. That breaks the
// very invariant the function exists to guarantee, and it does so only for
// sends near the window edge -- so it survives casual testing and shows up
// as a trickle of 5pm emails nobody can account for.
//
// Here the jitter is added to the candidate and the result is re-checked,
// so the returned time is always inside the window.
func (s *Scheduler) NextSendTime(after time.Time, tz *time.Location, country string, w model.Window) (time.Time, bool) {
	if tz == nil {
		tz = time.UTC
	}
	t := after.In(tz)

	for day := 0; day < MaxSearchDays; day++ {
		if !s.IsBusinessDay(country, t) {
			t = startOfDay(t.AddDate(0, 0, 1), tz)
			continue
		}

		start := atHour(t, w.StartHour, tz)
		end := atHour(t, w.EndHour, tz)

		if t.Before(start) {
			t = start
		}
		if !t.Before(end) {
			t = startOfDay(t.AddDate(0, 0, 1), tz)
			continue
		}

		// Jitter, bounded so the result cannot cross the window end.
		// Sends landing at exactly 09:00:00.000 across hundreds of
		// recipients is a machine signature.
		remaining := end.Sub(t)
		jitter := w.JitterMax
		if jitter > remaining {
			jitter = remaining
		}
		if jitter > 0 {
			t = t.Add(time.Duration(s.rng.Int63n(int64(jitter))))
		}

		// Re-check after jitter. A DST transition inside the window can
		// move the wall-clock end, so the arithmetic above is necessary
		// but not sufficient.
		if !t.Before(atHour(t, w.EndHour, tz)) {
			t = startOfDay(t.AddDate(0, 0, 1), tz)
			continue
		}
		return t, true
	}

	return time.Time{}, false
}

// startOfDay returns local midnight of the following day's date.
//
// Constructed from the date parts rather than by truncation, because
// truncating to 24 hours is wrong across a DST transition: a 23-hour day
// truncates into the previous day and a 25-hour day lands mid-morning.
func startOfDay(t time.Time, tz *time.Location) time.Time {
	y, m, d := t.In(tz).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, tz)
}

// atHour returns the given local hour on t's date.
//
// On a spring-forward day the requested hour may not exist -- 02:00 does
// not happen in most US zones on the transition date. time.Date normalises
// forward, which is the behaviour we want: the send lands at the first
// moment that does exist.
func atHour(t time.Time, hour int, tz *time.Location) time.Time {
	y, m, d := t.In(tz).Date()
	return time.Date(y, m, d, hour, 0, 0, 0, tz)
}

// StepDelay returns when a step becomes eligible, before window snapping.
func StepDelay(previousSend time.Time, step model.Step) time.Time {
	return previousSend.Add(step.DelayAfter)
}

// ScheduleStep combines the delay with the window snap.
func (s *Scheduler) ScheduleStep(
	previousSend time.Time,
	step model.Step,
	tz *time.Location,
	country string,
	w model.Window,
) (time.Time, bool) {
	return s.NextSendTime(StepDelay(previousSend, step), tz, country, w)
}

// StaticCalendar is a simple per-country holiday set, keyed on the local
// date in YYYY-MM-DD form.
type StaticCalendar struct {
	holidays map[string]map[string]bool
}

func NewStaticCalendar() *StaticCalendar {
	return &StaticCalendar{holidays: make(map[string]map[string]bool)}
}

func (c *StaticCalendar) Add(country string, days ...time.Time) {
	m := c.holidays[country]
	if m == nil {
		m = make(map[string]bool)
		c.holidays[country] = m
	}
	for _, d := range days {
		m[d.Format("2006-01-02")] = true
	}
}

func (c *StaticCalendar) IsHoliday(country string, day time.Time) bool {
	m := c.holidays[country]
	if m == nil {
		return false
	}
	return m[day.Format("2006-01-02")]
}
