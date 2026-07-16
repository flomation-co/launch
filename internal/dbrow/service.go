// Package dbrow implements the "new database row" poll trigger.
//
// It mirrors the S3 poll service (internal/s3): a background loop lists all
// database-row triggers, claims each under a short lease (so only one Launch
// instance polls a given trigger), and fires the flow once per newly-inserted
// row. "New" is defined by a monotonic cursor column the user nominates — an
// auto-increment id or a created_at/updated_at timestamp. The high-water mark is
// persisted in trigger_state, so a restart resumes from the last row seen rather
// than re-firing history.
//
// Security notes: the database password (and any other field) may be a
// ${secrets.X} reference, resolved via the API at poll time so it never rests in
// Launch. Table and column names cannot be bound as query parameters, so they
// are validated against a strict identifier grammar and quoted per-dialect
// before interpolation; the watermark and filter *values* are always bound.
package dbrow

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	// Pure-Go SQL drivers. CGO_ENABLED=0 is preserved.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

const (
	// DefaultPollInterval is the base loop cadence. Per-trigger poll intervals
	// (which may be longer) are honoured on top of this via __last_polled.
	DefaultPollInterval = 60 * time.Second
	MinPollInterval     = 10 * time.Second
	LeaseDuration       = 2 * time.Minute

	// queryTimeout caps a single connect+query so a slow or unreachable
	// database can't wedge the poll loop.
	queryTimeout = 20 * time.Second

	// batchLimit caps how many new rows a single poll fires for. A backlog
	// drains across subsequent polls as the watermark advances.
	batchLimit = 500

	// State keys in trigger_state.
	sentinelKey   = "__initialized"
	cursorKey     = "__cursor"
	lastPolledKey = "__last_polled"
)

type triggerConfig struct {
	Dialect      string `json:"dialect"`
	Host         string `json:"host"`
	Port         string `json:"port"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Database     string `json:"database"`
	SSLMode      string `json:"ssl_mode"`
	Table        string `json:"table"`
	CursorColumn string `json:"cursor_column"`
	PollInterval string `json:"poll_interval"`
	FilterColumn string `json:"filter_column"`
	FilterValue  string `json:"filter_value"`
}

type Service struct {
	config     *config.Config
	db         *persistence.Service
	trigger    *trigger.Service
	instanceID string
}

func NewService(config *config.Config, db *persistence.Service, trigger *trigger.Service) *Service {
	s := &Service{
		config:     config,
		db:         db,
		trigger:    trigger,
		instanceID: uuid.New().String(),
	}

	go s.watch()

	return s
}

func (s *Service) watch() {
	time.Sleep(5 * time.Second)

	for {
		s.poll()
		time.Sleep(DefaultPollInterval)
	}
}

func (s *Service) poll() {
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeDBRow)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("unable to get database-row triggers")
		return
	}

	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg triggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to parse database-row trigger config")
		return
	}

	// Validate identifiers up front — they are interpolated into SQL, not bound.
	if !validTableName(cfg.Table) {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "table": cfg.Table}).Warn("database-row trigger has an invalid table name")
		return
	}
	if !validIdentifier(cfg.CursorColumn) {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "cursor_column": cfg.CursorColumn}).Warn("database-row trigger has an invalid cursor column")
		return
	}
	if cfg.FilterColumn != "" && !validIdentifier(cfg.FilterColumn) {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "filter_column": cfg.FilterColumn}).Warn("database-row trigger has an invalid filter column")
		return
	}

	// Claim the trigger so only one instance polls it.
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to acquire lease for database-row trigger")
		return
	}
	if !acquired {
		return
	}

	// Load all persisted state for this trigger in one round-trip.
	state, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to get trigger state")
		return
	}

	// Honour the per-trigger poll interval on top of the base loop cadence.
	interval := parseInterval(cfg.PollInterval)
	if shouldSkipForInterval(state, interval, time.Now()) {
		return
	}

	// Resolve any ${secrets.X}/${env.X} references at poll time.
	dialect := cfg.Dialect
	host := s.trigger.ResolveString(tr.ID, cfg.Host)
	port := s.trigger.ResolveString(tr.ID, cfg.Port)
	user := s.trigger.ResolveString(tr.ID, cfg.Username)
	pass := s.trigger.ResolveString(tr.ID, cfg.Password)
	database := s.trigger.ResolveString(tr.ID, cfg.Database)
	filterValue := s.trigger.ResolveString(tr.ID, cfg.FilterValue)

	drv, dsn, err := buildDSN(dialect, host, port, user, pass, database, cfg.SSLMode)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Warn("database-row trigger has invalid connection settings")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	dbConn, err := sql.Open(drv, dsn)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to open database connection")
		return
	}
	defer dbConn.Close()
	dbConn.SetMaxOpenConns(1)

	if err := dbConn.PingContext(ctx); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to reach database")
		return
	}

	// Record that we polled, regardless of outcome, so the interval is respected.
	s.markPolled(tr.ID)

	_, initialised := state[sentinelKey]
	if !initialised {
		s.baseline(ctx, tr, dbConn, dialect, cfg, filterValue)
		return
	}

	// Determine the watermark. It may be absent if the table was empty at
	// baseline — in that case every current row is genuinely new.
	watermark, useCursor := readCursor(state)

	q := buildSelectQuery(dialect, cfg.Table, cfg.CursorColumn, cfg.FilterColumn, useCursor, batchLimit)
	args := buildArgs(useCursor, watermark, cfg.FilterColumn, filterValue)

	rows, err := dbConn.QueryContext(ctx, q, args...)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID, "query": q}).Error("unable to query for new rows")
		return
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to read result columns")
		return
	}

	maxCursor := ""
	fired := 0
	for rows.Next() {
		rowMap, cursorVal, err := scanRow(rows, cols, cfg.CursorColumn)
		if err != nil {
			log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to scan row")
			break
		}
		s.fireTrigger(tr, cfg.Table, rowMap, cursorVal)
		maxCursor = cursorVal
		fired++
	}
	if err := rows.Err(); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("row iteration error")
	}

	// Advance the watermark past the rows we dispatched (rows are cursor-ordered
	// ascending, so the last one is the max). At-most-once on a fire error.
	if fired > 0 && maxCursor != "" {
		s.storeCursor(tr.ID, maxCursor)
		log.WithFields(log.Fields{"trigger_id": tr.ID, "fired": fired, "cursor": maxCursor}).Info("database-row trigger fired for new rows")
	}
}

// baseline records the current high-water mark on the first ever poll so that
// pre-existing rows never fire. If the table is empty, no cursor is stored and
// the next poll treats all rows as new.
func (s *Service) baseline(ctx context.Context, tr *launch.Trigger, dbConn *sql.DB, dialect string, cfg triggerConfig, filterValue string) {
	q := buildMaxQuery(dialect, cfg.Table, cfg.CursorColumn, cfg.FilterColumn)
	var args []interface{}
	if cfg.FilterColumn != "" {
		args = append(args, filterValue)
	}

	var raw interface{}
	if err := dbConn.QueryRowContext(ctx, q, args...).Scan(&raw); err != nil && err != sql.ErrNoRows {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID, "query": q}).Error("unable to baseline database-row trigger")
		return
	}

	if cursor := cursorToString(normaliseValue(raw)); cursor != "" {
		s.storeCursor(tr.ID, cursor)
	}
	s.setState(tr.ID, sentinelKey, map[string]string{"status": "initialised"})
	log.WithFields(log.Fields{"trigger_id": tr.ID}).Info("database-row trigger baselined")
}

func (s *Service) fireTrigger(tr *launch.Trigger, table string, row map[string]interface{}, cursor string) {
	// Spread the row's columns as top-level keys so a downstream node can read
	// `${email}` directly, then layer the reserved metadata on top.
	data := make(map[string]interface{}, len(row)+4)
	for k, v := range row {
		data[k] = v
	}
	data["row"] = row
	data["table"] = table
	data["cursor"] = cursor
	data["triggered_at"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID, "cursor": cursor}).Error("unable to fire database-row trigger")
	}
}

func (s *Service) storeCursor(triggerID, cursor string) {
	s.setState(triggerID, cursorKey, cursor)
}

func (s *Service) markPolled(triggerID string) {
	s.setState(triggerID, lastPolledKey, time.Now().UTC().Format(time.RFC3339Nano))
}

func (s *Service) setState(triggerID, key string, value interface{}) {
	data, err := json.Marshal(value)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": triggerID, "key": key}).Error("unable to marshal trigger state")
		return
	}
	if err := s.db.UpsertTriggerState(triggerID, key, data); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": triggerID, "key": key}).Error("unable to persist trigger state")
	}
}

// scanRow reads the current row into a column→value map and extracts the cursor
// column's value as a string.
func scanRow(rows *sql.Rows, cols []string, cursorCol string) (map[string]interface{}, string, error) {
	vals := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, "", err
	}

	row := make(map[string]interface{}, len(cols))
	cursor := ""
	for i, c := range cols {
		norm := normaliseValue(vals[i])
		row[c] = norm
		if c == cursorCol {
			cursor = cursorToString(vals[i])
		}
	}
	return row, cursor, nil
}

// readCursor returns the persisted watermark and whether one exists.
func readCursor(state map[string]json.RawMessage) (string, bool) {
	raw, ok := state[cursorKey]
	if !ok {
		return "", false
	}
	var cursor string
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return "", false
	}
	if cursor == "" {
		return "", false
	}
	return cursor, true
}

// buildArgs assembles query arguments in the placeholder order used by
// buildSelectQuery: watermark first (when useCursor), then the filter value.
func buildArgs(useCursor bool, watermark, filterCol, filterValue string) []interface{} {
	var args []interface{}
	if useCursor {
		args = append(args, watermark)
	}
	if filterCol != "" {
		args = append(args, filterValue)
	}
	return args
}

// parseInterval reads the user's poll interval, clamping to a sane minimum and
// defaulting when blank or unparseable.
func parseInterval(raw string) time.Duration {
	if raw == "" {
		return DefaultPollInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultPollInterval
	}
	if d < MinPollInterval {
		return MinPollInterval
	}
	return d
}

// shouldSkipForInterval reports whether the trigger was polled more recently
// than its interval allows.
func shouldSkipForInterval(state map[string]json.RawMessage, interval time.Duration, now time.Time) bool {
	raw, ok := state[lastPolledKey]
	if !ok {
		return false
	}
	var ts string
	if err := json.Unmarshal(raw, &ts); err != nil {
		return false
	}
	last, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return false
	}
	return now.Sub(last) < interval
}
