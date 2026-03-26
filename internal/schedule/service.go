package schedule

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

const (
	tickInterval = 15 * time.Second
)

// ScheduleConfig represents the configuration stored in a schedule trigger's Data field.
type ScheduleConfig struct {
	Mode                 string `json:"mode"`                            // "interval", "daily", "weekly"
	Interval             string `json:"interval,omitempty"`              // e.g. "15"
	Unit                 string `json:"unit,omitempty"`                  // "minutes", "hours", "days"
	TimeOfDay            string `json:"time_of_day,omitempty"`           // "HH:MM" 24-hour format
	DaysOfWeek           string `json:"days_of_week,omitempty"`          // "monday,wednesday"
	Timezone             string `json:"timezone,omitempty"`              // IANA timezone e.g. "Europe/London"
	ExcludeBankHolidays  string `json:"exclude_bank_holidays,omitempty"` // "true" to skip UK bank holidays
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

	mu           sync.Mutex
	lastFired    map[string]time.Time
	bankHolidays map[string]bool // "YYYY-MM-DD" → true
}

func NewService(cfg *config.Config, triggerSvc *trigger.Service, db *persistence.Service) *Service {
	s := &Service{
		config:       cfg,
		trigger:      triggerSvc,
		db:           db,
		lastFired:    make(map[string]time.Time),
		bankHolidays: make(map[string]bool),
	}

	s.loadBankHolidays()
	go s.watch()

	return s
}

const bankHolidayURL = "https://www.gov.uk/bank-holidays.json"

func (s *Service) loadBankHolidays() {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(bankHolidayURL)
	if err != nil {
		log.WithError(err).Warn("unable to fetch UK bank holidays — bank holiday exclusion will not work")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.WithError(err).Warn("unable to read bank holidays response")
		return
	}

	var data bankHolidayResponse
	if err := json.Unmarshal(body, &data); err != nil {
		log.WithError(err).Warn("unable to parse bank holidays JSON")
		return
	}

	s.mu.Lock()
	for _, event := range data.EnglandAndWales.Events {
		s.bankHolidays[event.Date] = true
	}
	s.mu.Unlock()

	log.WithField("count", len(data.EnglandAndWales.Events)).Info("loaded UK bank holidays")
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

	s.mu.Lock()
	last, seen := s.lastFired[tr.ID]
	s.mu.Unlock()

	if !seen {
		// First time seeing this trigger — record state without firing
		s.mu.Lock()
		s.lastFired[tr.ID] = now
		s.mu.Unlock()
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

	s.mu.Lock()
	s.lastFired[tr.ID] = now
	s.mu.Unlock()

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
