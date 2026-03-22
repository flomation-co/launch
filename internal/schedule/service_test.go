package schedule

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func Test_ShouldFire_Interval_Minutes(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: 5, Unit: "minutes"}
	now := time.Now()

	// Not enough time passed
	Expect(ShouldFire(cfg, now.Add(-3*time.Minute), now)).To(BeFalse())

	// Enough time passed
	Expect(ShouldFire(cfg, now.Add(-6*time.Minute), now)).To(BeTrue())
}

func Test_ShouldFire_Interval_Hours(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: 2, Unit: "hours"}
	now := time.Now()

	Expect(ShouldFire(cfg, now.Add(-1*time.Hour), now)).To(BeFalse())
	Expect(ShouldFire(cfg, now.Add(-3*time.Hour), now)).To(BeTrue())
}

func Test_ShouldFire_Interval_Days(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: 1, Unit: "days"}
	now := time.Now()

	Expect(ShouldFire(cfg, now.Add(-12*time.Hour), now)).To(BeFalse())
	Expect(ShouldFire(cfg, now.Add(-25*time.Hour), now)).To(BeTrue())
}

func Test_ShouldFire_Interval_ZeroInterval(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: 0, Unit: "minutes"}
	Expect(ShouldFire(cfg, time.Now().Add(-time.Hour), time.Now())).To(BeFalse())
}

func Test_ShouldFire_Interval_InvalidUnit(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg := ScheduleConfig{Mode: "interval", Interval: 5, Unit: "weeks"}
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
		DaysOfWeek: []string{"monday"},
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
		DaysOfWeek: []string{"monday", "friday"},
	}

	now := time.Date(today.Year(), today.Month(), today.Day(), 10, 30, 0, 0, time.Local)
	lastFired := time.Date(today.Year(), today.Month(), today.Day(), 8, 0, 0, 0, time.Local)

	Expect(ShouldFire(cfg, lastFired, now)).To(BeFalse())
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

	h, m = parseTimeOfDay("invalid")
	Expect(h).To(Equal(-1))
	Expect(m).To(Equal(-1))

	h, m = parseTimeOfDay("25:00")
	Expect(h).To(Equal(-1))
	Expect(m).To(Equal(-1))
}
