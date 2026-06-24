package schedule

import (
	"encoding/json"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func Test_LastFiredState_RoundTrip(t *testing.T) {
	// lastFiredState is persisted to trigger_state as JSON and must
	// round-trip cleanly across a process restart, otherwise the new
	// process can't tell whether today's schedule has already fired
	// and would re-fire on the next tick — exactly the failure mode
	// the previous in-memory-only implementation was vulnerable to.
	RegisterTestingT(t)

	orig := lastFiredState{At: time.Date(2026, 6, 9, 8, 0, 0, 0, time.UTC)}
	raw, err := json.Marshal(orig)
	Expect(err).NotTo(HaveOccurred())

	var back lastFiredState
	Expect(json.Unmarshal(raw, &back)).To(Succeed())
	Expect(back.At.Equal(orig.At)).To(BeTrue())
}

func Test_LastFiredState_RoundTripWithTimezone(t *testing.T) {
	// Re-anchoring across timezones: store London time, read back, then
	// project into the schedule's location to verify the wall-clock
	// matches. This is the exact path readLastFired walks when a daily
	// schedule with a non-UTC timezone restarts.
	RegisterTestingT(t)

	london, err := time.LoadLocation("Europe/London")
	Expect(err).NotTo(HaveOccurred())
	at := time.Date(2026, 6, 9, 8, 0, 0, 0, london)

	raw, err := json.Marshal(lastFiredState{At: at})
	Expect(err).NotTo(HaveOccurred())

	var back lastFiredState
	Expect(json.Unmarshal(raw, &back)).To(Succeed())

	rehomed := back.At.In(london)
	Expect(rehomed.Hour()).To(Equal(8))
	Expect(rehomed.Minute()).To(Equal(0))
}

func Test_ShouldFire_Interval_Minutes(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: "5", Unit: "minutes"}
	now := time.Now()

	// Not enough time passed
	Expect(ShouldFire(cfg, now.Add(-3*time.Minute), now)).To(BeFalse())

	// Enough time passed
	Expect(ShouldFire(cfg, now.Add(-6*time.Minute), now)).To(BeTrue())
}

func Test_ShouldFire_Interval_Hours(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: "2", Unit: "hours"}
	now := time.Now()

	Expect(ShouldFire(cfg, now.Add(-1*time.Hour), now)).To(BeFalse())
	Expect(ShouldFire(cfg, now.Add(-3*time.Hour), now)).To(BeTrue())
}

func Test_ShouldFire_Interval_Days(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: "1", Unit: "days"}
	now := time.Now()

	Expect(ShouldFire(cfg, now.Add(-12*time.Hour), now)).To(BeFalse())
	Expect(ShouldFire(cfg, now.Add(-25*time.Hour), now)).To(BeTrue())
}

func Test_ShouldFire_Interval_ZeroInterval(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: "0", Unit: "minutes"}
	Expect(ShouldFire(cfg, time.Now().Add(-time.Hour), time.Now())).To(BeFalse())
}

func Test_ShouldFire_Interval_InvalidUnit(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: "5", Unit: "weeks"}
	Expect(ShouldFire(cfg, time.Now().Add(-time.Hour), time.Now())).To(BeFalse())
}

func Test_ShouldFire_Daily(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "daily", TimeOfDay: "09:00"}

	// Now is 09:30, last fired at 08:00 — should fire
	today := time.Now()
	now := time.Date(today.Year(), today.Month(), today.Day(), 9, 30, 0, 0, time.Local)
	lastFired := time.Date(today.Year(), today.Month(), today.Day(), 8, 0, 0, 0, time.Local)

	Expect(ShouldFire(cfg, lastFired, now)).To(BeTrue())
}

func Test_ShouldFire_Daily_AlreadyFired(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "daily", TimeOfDay: "09:00"}

	today := time.Now()
	now := time.Date(today.Year(), today.Month(), today.Day(), 10, 0, 0, 0, time.Local)
	lastFired := time.Date(today.Year(), today.Month(), today.Day(), 9, 5, 0, 0, time.Local)

	Expect(ShouldFire(cfg, lastFired, now)).To(BeFalse())
}

func Test_ShouldFire_Daily_BeforeTime(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "daily", TimeOfDay: "14:00"}

	today := time.Now()
	now := time.Date(today.Year(), today.Month(), today.Day(), 8, 0, 0, 0, time.Local)
	lastFired := time.Date(today.Year(), today.Month(), today.Day()-1, 14, 5, 0, 0, time.Local)

	Expect(ShouldFire(cfg, lastFired, now)).To(BeFalse())
}

func Test_ShouldFire_Weekly_CorrectDay(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	// Find next Monday
	today := time.Now()
	for today.Weekday() != time.Monday {
		today = today.AddDate(0, 0, 1)
	}

	cfg := ScheduleConfig{
		Mode:       "weekly",
		TimeOfDay:  "10:00",
		DaysOfWeek: "monday",
	}

	now := time.Date(today.Year(), today.Month(), today.Day(), 10, 30, 0, 0, time.Local)
	lastFired := time.Date(today.Year(), today.Month(), today.Day(), 8, 0, 0, 0, time.Local)

	Expect(ShouldFire(cfg, lastFired, now)).To(BeTrue())
}

func Test_ShouldFire_Weekly_WrongDay(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	// Find next Tuesday
	today := time.Now()
	for today.Weekday() != time.Tuesday {
		today = today.AddDate(0, 0, 1)
	}

	cfg := ScheduleConfig{
		Mode:       "weekly",
		TimeOfDay:  "10:00",
		DaysOfWeek: "monday,friday",
	}

	now := time.Date(today.Year(), today.Month(), today.Day(), 10, 30, 0, 0, time.Local)
	lastFired := time.Date(today.Year(), today.Month(), today.Day(), 8, 0, 0, 0, time.Local)

	Expect(ShouldFire(cfg, lastFired, now)).To(BeFalse())
}

// --- Monthly (specific dates) ---------------------------------------------

func Test_ShouldFire_Monthly_OnDate(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "monthly", TimeOfDay: "09:00", DaysOfMonth: "1,15,28"}

	// 28th at 09:30, last fired 08:00 same day — should fire.
	now := time.Date(2026, 6, 28, 9, 30, 0, 0, time.Local)
	lastFired := time.Date(2026, 6, 28, 8, 0, 0, 0, time.Local)
	Expect(ShouldFire(cfg, lastFired, now)).To(BeTrue())

	// 15th also configured.
	now15 := time.Date(2026, 6, 15, 9, 30, 0, 0, time.Local)
	last15 := time.Date(2026, 6, 15, 8, 0, 0, 0, time.Local)
	Expect(ShouldFire(cfg, last15, now15)).To(BeTrue())
}

func Test_ShouldFire_Monthly_WrongDate(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "monthly", TimeOfDay: "09:00", DaysOfMonth: "28"}

	// 27th is not in the set — must not fire regardless of the time.
	now := time.Date(2026, 6, 27, 9, 30, 0, 0, time.Local)
	lastFired := time.Date(2026, 6, 27, 8, 0, 0, 0, time.Local)
	Expect(ShouldFire(cfg, lastFired, now)).To(BeFalse())
}

func Test_ShouldFire_Monthly_AlreadyFired(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "monthly", TimeOfDay: "09:00", DaysOfMonth: "28"}

	now := time.Date(2026, 6, 28, 10, 0, 0, 0, time.Local)
	lastFired := time.Date(2026, 6, 28, 9, 5, 0, 0, time.Local) // already fired after 09:00
	Expect(ShouldFire(cfg, lastFired, now)).To(BeFalse())
}

func Test_ShouldFire_Monthly_31stSkippedInFebruary(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	// The confirmed edge case: a schedule on the 31st must NOT fire in
	// February — the 31st does not exist, and the last day (28th) is not
	// the 31st, so the month is skipped entirely.
	cfg := ScheduleConfig{Mode: "monthly", TimeOfDay: "09:00", DaysOfMonth: "31"}

	feb28 := time.Date(2026, 2, 28, 9, 30, 0, 0, time.Local) // last day of Feb 2026
	Expect(feb28.Day()).To(Equal(28))
	lastFired := time.Date(2026, 2, 28, 8, 0, 0, 0, time.Local)
	Expect(ShouldFire(cfg, lastFired, feb28)).To(BeFalse())

	// Same schedule DOES fire on a real 31st (e.g. July).
	jul31 := time.Date(2026, 7, 31, 9, 30, 0, 0, time.Local)
	lastJul := time.Date(2026, 7, 31, 8, 0, 0, 0, time.Local)
	Expect(ShouldFire(cfg, lastJul, jul31)).To(BeTrue())
}

func Test_ShouldFire_Monthly_LastDay(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "monthly", TimeOfDay: "09:00", DaysOfMonth: "last"}

	// June has 30 days — fires on the 30th, not the 29th.
	jun30 := time.Date(2026, 6, 30, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2026, 6, 30, 8, 0, 0, 0, time.Local), jun30)).To(BeTrue())
	jun29 := time.Date(2026, 6, 29, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2026, 6, 29, 8, 0, 0, 0, time.Local), jun29)).To(BeFalse())

	// February non-leap (2026): last day is the 28th.
	feb28 := time.Date(2026, 2, 28, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2026, 2, 28, 8, 0, 0, 0, time.Local), feb28)).To(BeTrue())

	// February leap (2024): last day is the 29th, so the 28th must not fire.
	leap29 := time.Date(2024, 2, 29, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2024, 2, 29, 8, 0, 0, 0, time.Local), leap29)).To(BeTrue())
	leap28 := time.Date(2024, 2, 28, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2024, 2, 28, 8, 0, 0, 0, time.Local), leap28)).To(BeFalse())
}

// --- Monthly weekday (Nth weekday) -----------------------------------------
//
// June 2026 begins on a Monday, giving Mondays on the 1st, 8th, 15th, 22nd and
// 29th and Fridays on the 5th, 12th, 19th and 26th.

func Test_ShouldFire_MonthlyWeekday_FirstMonday(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "monthly_weekday", TimeOfDay: "09:00", WeekOrdinal: "first", Weekday: "monday"}

	now := time.Date(2026, 6, 1, 9, 30, 0, 0, time.Local)
	Expect(now.Weekday()).To(Equal(time.Monday))
	Expect(ShouldFire(cfg, time.Date(2026, 6, 1, 8, 0, 0, 0, time.Local), now)).To(BeTrue())

	// The second Monday (8th) must not fire for a "first" schedule.
	second := time.Date(2026, 6, 8, 9, 30, 0, 0, time.Local)
	Expect(second.Weekday()).To(Equal(time.Monday))
	Expect(ShouldFire(cfg, time.Date(2026, 6, 8, 8, 0, 0, 0, time.Local), second)).To(BeFalse())
}

func Test_ShouldFire_MonthlyWeekday_LastFriday(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "monthly_weekday", TimeOfDay: "09:00", WeekOrdinal: "last", Weekday: "friday"}

	last := time.Date(2026, 6, 26, 9, 30, 0, 0, time.Local) // last Friday of June 2026
	Expect(last.Weekday()).To(Equal(time.Friday))
	Expect(ShouldFire(cfg, time.Date(2026, 6, 26, 8, 0, 0, 0, time.Local), last)).To(BeTrue())

	// An earlier Friday (19th) must not fire for a "last" schedule.
	earlier := time.Date(2026, 6, 19, 9, 30, 0, 0, time.Local)
	Expect(earlier.Weekday()).To(Equal(time.Friday))
	Expect(ShouldFire(cfg, time.Date(2026, 6, 19, 8, 0, 0, 0, time.Local), earlier)).To(BeFalse())
}

func Test_ShouldFire_MonthlyWeekday_FifthMissing(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	// July 2026 has only four Mondays (6th, 13th, 20th, 27th), so a "fifth
	// Monday" schedule never fires that month.
	cfg := ScheduleConfig{Mode: "monthly_weekday", TimeOfDay: "09:00", WeekOrdinal: "fifth", Weekday: "monday"}

	fourth := time.Date(2026, 7, 27, 9, 30, 0, 0, time.Local)
	Expect(fourth.Weekday()).To(Equal(time.Monday))
	Expect(ShouldFire(cfg, time.Date(2026, 7, 27, 8, 0, 0, 0, time.Local), fourth)).To(BeFalse())

	// A "fourth Monday" schedule DOES fire on the 27th.
	cfg.WeekOrdinal = "fourth"
	Expect(ShouldFire(cfg, time.Date(2026, 7, 27, 8, 0, 0, 0, time.Local), fourth)).To(BeTrue())
}

// --- Yearly ----------------------------------------------------------------

func Test_ShouldFire_Yearly_OnDate(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "yearly", TimeOfDay: "09:00", MonthOfYear: "1", DayOfMonth: "1"}

	now := time.Date(2026, 1, 1, 9, 30, 0, 0, time.Local)
	lastFired := time.Date(2025, 12, 31, 23, 0, 0, 0, time.Local)
	Expect(ShouldFire(cfg, lastFired, now)).To(BeTrue())

	// Wrong day must not fire.
	jan2 := time.Date(2026, 1, 2, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2026, 1, 2, 8, 0, 0, 0, time.Local), jan2)).To(BeFalse())

	// Wrong month must not fire.
	feb1 := time.Date(2026, 2, 1, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2026, 2, 1, 8, 0, 0, 0, time.Local), feb1)).To(BeFalse())
}

func Test_ShouldFire_Yearly_Feb29SkippedInNonLeap(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "yearly", TimeOfDay: "09:00", MonthOfYear: "2", DayOfMonth: "29"}

	// 2026 is not a leap year: the 28th (last day of Feb) is not the 29th,
	// so the schedule is skipped entirely.
	feb28 := time.Date(2026, 2, 28, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2026, 2, 28, 8, 0, 0, 0, time.Local), feb28)).To(BeFalse())

	// 2024 is a leap year: it fires on the 29th.
	feb29 := time.Date(2024, 2, 29, 9, 30, 0, 0, time.Local)
	Expect(ShouldFire(cfg, time.Date(2024, 2, 29, 8, 0, 0, 0, time.Local), feb29)).To(BeTrue())
}

// --- Weekly backward compatibility with the multi_select string format ------

func Test_ShouldFire_Weekly_MultiSelectFormat(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	// The editor's multi_select stores an ordered comma-separated string,
	// identical to the previous free-text format. A full-week selection must
	// still match a Monday.
	cfg := ScheduleConfig{
		Mode:       "weekly",
		TimeOfDay:  "10:00",
		DaysOfWeek: "monday,tuesday,wednesday,thursday,friday,saturday,sunday",
	}

	monday := time.Date(2026, 6, 1, 10, 30, 0, 0, time.Local)
	Expect(monday.Weekday()).To(Equal(time.Monday))
	Expect(ShouldFire(cfg, time.Date(2026, 6, 1, 8, 0, 0, 0, time.Local), monday)).To(BeTrue())
}

// --- Helper unit tests ------------------------------------------------------

func Test_DaysInMonth(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	Expect(daysInMonth(time.Date(2026, 2, 10, 0, 0, 0, 0, time.Local))).To(Equal(28))  // non-leap Feb
	Expect(daysInMonth(time.Date(2024, 2, 10, 0, 0, 0, 0, time.Local))).To(Equal(29))  // leap Feb
	Expect(daysInMonth(time.Date(2026, 4, 10, 0, 0, 0, 0, time.Local))).To(Equal(30))  // April
	Expect(daysInMonth(time.Date(2026, 12, 10, 0, 0, 0, 0, time.Local))).To(Equal(31)) // December
}

func Test_ParseWeekday(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	wd, ok := parseWeekday("Monday")
	Expect(ok).To(BeTrue())
	Expect(wd).To(Equal(time.Monday))

	wd, ok = parseWeekday("  sunday ")
	Expect(ok).To(BeTrue())
	Expect(wd).To(Equal(time.Sunday))

	_, ok = parseWeekday("notaday")
	Expect(ok).To(BeFalse())
}

func Test_ShouldFire_UnknownMode(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "custom"}
	Expect(ShouldFire(cfg, time.Now().Add(-time.Hour), time.Now())).To(BeFalse())
}

func Test_ParseTimeOfDay(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	h, m := parseTimeOfDay("09:30")
	Expect(h).To(Equal(9))
	Expect(m).To(Equal(30))

	h, m = parseTimeOfDay("23:59")
	Expect(h).To(Equal(23))
	Expect(m).To(Equal(59))

	h3, m3 := parseTimeOfDay("invalid")
	Expect(h3).To(Equal(-1))
	Expect(m3).To(Equal(-1))

	h4, m4 := parseTimeOfDay("25:00")
	Expect(h4).To(Equal(-1))
	Expect(m4).To(Equal(-1))
}
