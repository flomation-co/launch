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
