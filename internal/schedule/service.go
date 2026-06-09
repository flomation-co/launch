package schedule

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

//go:embed bank-holidays.json
var bankHolidaysData []byte

const (
	tickInterval  = 15 * time.Second
	leaseDuration = 2 * time.Minute

	// stateKeyLastFired is the trigger_state row Launch persists per
	// trigger to record when the schedule last fired. Without this, an
	// in-memory map would lose the day's fire-record on every restart
	// and any restart that happens to land in the fire-window of a daily
	// schedule would re-fire it.
	stateKeyLastFired = "last_fired"
)

// lastFiredState is the JSON shape persisted under stateKeyLastFired.
type lastFiredState struct {
	At time.Time `json:"at"`
}

// ScheduleConfig represents the configuration stored in a schedule trigger's Data field.
type ScheduleConfig struct {
	Mode                string `json:"mode"`                            // "interval", "daily", "weekly"
	Interval            string `json:"interval,omitempty"`              // e.g. "15"
	Unit                string `json:"unit,omitempty"`                  // "minutes", "hours", "days"
	TimeOfDay           string `json:"time_of_day,omitempty"`           // "HH:MM" 24-hour format
	DaysOfWeek          string `json:"days_of_week,omitempty"`          // "monday,wednesday"
	Timezone            string `json:"timezone,omitempty"`              // IANA timezone e.g. "Europe/London"
	ExcludeBankHolidays string `json:"exclude_bank_holidays,omitempty"` // "true" to skip UK bank holidays
}

// SchedulePayload is the payload sent when a schedule trigger fires.
type SchedulePayload struct {
	TriggeredAt  string `json:"triggered_at"`
	ScheduleMode string `json:"schedule_mode"`
}

// bankHolidayResponse models the GOV.UK bank-holidays.json response.
type bankHolidayResponse struct {
	EnglandAndWales struct {
		Events []struct {
			Date string `json:"date"` // "YYYY-MM-DD"
		} `json:"events"`
	} `json:"england-and-wales"`
}

type Service struct {
	config  *config.Config
	trigger *trigger.Service
	db      *persistence.Service

	// instanceID identifies this Launch process when acquiring trigger
	// leases. Generated once at startup; never persisted across
	// restarts (a new instanceID after restart is exactly what lets the
	// new process eventually take over an expired lease).
	instanceID string

	mu           sync.Mutex
	lastFired    map[string]time.Time // cache of stateKeyLastFired per trigger ID
	bankHolidays map[string]bool      // "YYYY-MM-DD" → true
}

func NewService(cfg *config.Config, triggerSvc *trigger.Service, db *persistence.Service) *Service {
	s := &Service{
		config:       cfg,
		trigger:      triggerSvc,
		db:           db,
		instanceID:   uuid.New().String(),
		lastFired:    make(map[string]time.Time),
		bankHolidays: make(map[string]bool),
	}

	s.loadBankHolidays()
	go s.watch()

	return s
}

func (s *Service) loadBankHolidays() {
	var data bankHolidayResponse
	if err := json.Unmarshal(bankHolidaysData, &data); err != nil {
		log.WithError(err).Warn("unable to parse embedded bank holidays JSON")
		return
	}

	s.mu.Lock()
	for _, event := range data.EnglandAndWales.Events {
		s.bankHolidays[event.Date] = true
	}
	s.mu.Unlock()

	log.WithField("count", len(data.EnglandAndWales.Events)).Info("loaded UK bank holidays from embedded data")
}

func (s *Service) isBankHoliday(t time.Time) bool {
	dateStr := fmt.Sprintf("%04d-%02d-%02d", t.Year(), t.Month(), t.Day())
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bankHolidays[dateStr]
}

func (s *Service) watch() {
	time.Sleep(5 * time.Second)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for range ticker.C {
		s.poll()
	}
}

func (s *Service) poll() {
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeScheduled)
	if err != nil {
		log.WithError(err).Warn("unable to fetch schedule triggers")
		return
	}

	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg ScheduleConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"error":      err,
		}).Error("unable to parse schedule trigger config")
		return
	}

	now := time.Now()

	// Load timezone if specified
	loc := time.Local
	if cfg.Timezone != "" {
		parsed, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			log.WithFields(log.Fields{
				"trigger_id": tr.ID,
				"timezone":   cfg.Timezone,
				"error":      err,
			}).Warn("invalid timezone, using local")
		} else {
			loc = parsed
		}
	}

	now = now.In(loc)

	// Acquire a per-trigger lease before any decision/state mutation.
	// With multiple Launch instances the lease ensures only one
	// evaluates this trigger per tick; with a single instance the call
	// always succeeds and is effectively free. Either way the
	// lease-acquire + persisted lastFired pair makes the firing
	// decision authoritative across restarts and horizontal scale.
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, leaseDuration)
	if err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"error":      err,
		}).Warn("schedule trigger: unable to acquire lease, skipping tick")
		return
	}
	if !acquired {
		// Another Launch instance owns this trigger right now — they
		// will evaluate and fire if needed.
		return
	}

	// Hydrate lastFired from trigger_state on first sight per process,
	// then keep it in memory for cheap subsequent reads. We deliberately
	// do NOT treat absence of state as "fire now" — bootstrap silently
	// records the current time and bails (same first-seen semantics as
	// before), but persists it so a restart 10 seconds later doesn't
	// re-fire just because the in-memory map is empty.
	last, seen, err := s.readLastFired(tr.ID, now)
	if err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"error":      err,
		}).Warn("schedule trigger: unable to read last_fired state, skipping tick")
		return
	}

	if !seen {
		if err := s.writeLastFired(tr.ID, now); err != nil {
			log.WithFields(log.Fields{
				"trigger_id": tr.ID,
				"error":      err,
			}).Warn("schedule trigger: unable to persist bootstrap last_fired")
		}
		return
	}

	// Check bank holiday exclusion
	if cfg.ExcludeBankHolidays == "true" && s.isBankHoliday(now) {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"date":       now.Format("2006-01-02"),
		}).Debug("skipping schedule trigger — UK bank holiday")
		return
	}

	if !ShouldFire(cfg, last, now) {
		return
	}

	log.WithFields(log.Fields{
		"trigger_id": tr.ID,
		"mode":       cfg.Mode,
	}).Info("schedule trigger fired")

	// Persist BEFORE the HTTP fire so a crash between the two leaves us
	// in the "already fired" state, not the "fire again on restart"
	// state. The API's TriggerExecution is itself idempotent at the
	// execution-row level only insofar as it accepts every request, so
	// the safer failure mode is one missed reminder, not six duplicates.
	if err := s.writeLastFired(tr.ID, now); err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"error":      err,
		}).Warn("schedule trigger: unable to persist last_fired before fire — aborting to avoid re-fire on restart")
		return
	}

	payload := SchedulePayload{
		TriggeredAt:  now.Format(time.RFC3339),
		ScheduleMode: cfg.Mode,
	}

	if err := s.trigger.Trigger(tr, payload); err != nil {
		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"error":      err,
		}).Warn("unable to fire schedule trigger")
	}
}

// readLastFired returns the persisted last-fired timestamp for the given
// trigger, populating the in-memory cache on first call per process.
//
// Returns (zero, false, nil) when no state row exists yet — the caller
// treats this as "first sight, just record now and bail" (preserving
// the previous boot-time-don't-fire behaviour). Returns (t, true, nil)
// when a previous fire has been recorded.
func (s *Service) readLastFired(triggerID string, now time.Time) (time.Time, bool, error) {
	s.mu.Lock()
	cached, ok := s.lastFired[triggerID]
	s.mu.Unlock()
	if ok {
		return cached, true, nil
	}

	state, err := s.db.GetTriggerState(triggerID)
	if err != nil {
		return time.Time{}, false, err
	}
	raw, ok := state[stateKeyLastFired]
	if !ok || len(raw) == 0 {
		return time.Time{}, false, nil
	}

	var lf lastFiredState
	if err := json.Unmarshal(raw, &lf); err != nil {
		// Treat corrupt state as missing — bootstrap will overwrite it.
		log.WithFields(log.Fields{
			"trigger_id": triggerID,
			"raw":        string(raw),
			"error":      err,
		}).Warn("schedule trigger: corrupt last_fired state, treating as missing")
		return time.Time{}, false, nil
	}

	// Re-anchor the loaded time into the same location as `now` so
	// the daily/weekly target-time arithmetic works the same way it did
	// when the value was originally written. We store RFC3339 with the
	// configured timezone offset, so the location round-trips correctly
	// here.
	loaded := lf.At.In(now.Location())

	s.mu.Lock()
	s.lastFired[triggerID] = loaded
	s.mu.Unlock()

	return loaded, true, nil
}

// writeLastFired persists last_fired to trigger_state and updates the
// in-memory cache. Atomic upsert at the DB level.
func (s *Service) writeLastFired(triggerID string, at time.Time) error {
	raw, err := json.Marshal(lastFiredState{At: at})
	if err != nil {
		return err
	}
	if err := s.db.UpsertTriggerState(triggerID, stateKeyLastFired, raw); err != nil {
		return err
	}

	s.mu.Lock()
	s.lastFired[triggerID] = at
	s.mu.Unlock()
	return nil
}

// ShouldFire determines whether a schedule trigger should fire based on its
// configuration, the last time it fired, and the current time.
func ShouldFire(cfg ScheduleConfig, lastFired time.Time, now time.Time) bool {
	switch cfg.Mode {
	case "interval":
		return shouldFireInterval(cfg, lastFired, now)
	case "daily":
		return shouldFireDaily(cfg, lastFired, now)
	case "weekly":
		return shouldFireWeekly(cfg, lastFired, now)
	default:
		return false
	}
}

func shouldFireInterval(cfg ScheduleConfig, lastFired time.Time, now time.Time) bool {
	interval, err := strconv.Atoi(cfg.Interval)
	if err != nil || interval <= 0 {
		return false
	}

	var duration time.Duration
	switch cfg.Unit {
	case "minutes":
		duration = time.Duration(interval) * time.Minute
	case "hours":
		duration = time.Duration(interval) * time.Hour
	case "days":
		duration = time.Duration(interval) * 24 * time.Hour
	default:
		return false
	}

	return now.Sub(lastFired) >= duration
}

func shouldFireDaily(cfg ScheduleConfig, lastFired time.Time, now time.Time) bool {
	if cfg.TimeOfDay == "" {
		return false
	}

	targetHour, targetMin := parseTimeOfDay(cfg.TimeOfDay)
	if targetHour < 0 {
		return false
	}

	// Build today's target time
	target := time.Date(now.Year(), now.Month(), now.Day(), targetHour, targetMin, 0, 0, now.Location())

	// Has the target time passed today, and we haven't fired since before it?
	return now.After(target) && lastFired.Before(target)
}

func shouldFireWeekly(cfg ScheduleConfig, lastFired time.Time, now time.Time) bool {
	if cfg.TimeOfDay == "" || cfg.DaysOfWeek == "" {
		return false
	}

	// Split comma-separated days
	days := strings.Split(cfg.DaysOfWeek, ",")

	// Check if today is one of the configured days
	todayName := strings.ToLower(now.Weekday().String())
	dayMatch := false
	for _, d := range days {
		if strings.ToLower(strings.TrimSpace(d)) == todayName {
			dayMatch = true
			break
		}
	}

	if !dayMatch {
		return false
	}

	return shouldFireDaily(cfg, lastFired, now)
}

func parseTimeOfDay(tod string) (int, int) {
	parts := strings.SplitN(tod, ":", 2)
	if len(parts) != 2 {
		return -1, -1
	}

	hour := 0
	min := 0
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return -1, -1
		}
		hour = hour*10 + int(c-'0')
	}
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			return -1, -1
		}
		min = min*10 + int(c-'0')
	}

	if hour > 23 || min > 59 {
		return -1, -1
	}

	return hour, min
}
