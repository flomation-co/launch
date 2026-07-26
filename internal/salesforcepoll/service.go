// Package salesforcepoll implements the "Salesforce record created or updated"
// poll trigger.
//
// It mirrors internal/dbrow: a background loop lists all salesforce-poll
// triggers, claims each under a short lease (so only one Launch instance polls a
// given trigger), and fires the flow once per record it has not already seen.
// The watermark lives in trigger_state so a restart resumes rather than
// re-firing history.
//
// ── The part that is NOT like dbrow ─────────────────────────────────────────
//
// dbrow advances a single cursor and asks for `cursor > watermark`. That is
// correct only when the cursor is unique per row. Salesforce datetimes are
// SECOND-granular — every value comes back as .000 — and ties genuinely occur;
// two Accounts in the validation org share 2026-07-22T12:31:12.000 exactly,
// because anything touching several records at once (an import, a mass update,
// a cascade) stamps them all in the same second.
//
// With `> watermark`, every record sharing the boundary second after the first
// one is dispatched is dropped, permanently and silently. So this service
// instead asks for `>= watermark` and remembers which ids it already fired AT
// that exact second:
//
//	WHERE SystemModstamp >= :watermark ORDER BY SystemModstamp ASC, Id ASC
//	skip a row when modstamp == watermark AND id ∈ firedAtWatermark
//
// The remembered set is bounded — it is cleared as soon as the watermark moves
// past that second — so it cannot grow without limit. Re-reading the boundary
// second on every poll costs a handful of rows, which is a fair price for not
// losing records.
//
// Security notes: the access token may be a ${secrets.X} reference, resolved via
// the API at poll time so it never rests in Launch. The object name, the field
// list and the optional filter are interpolated into SOQL (which has no bind
// variables over REST), so each is validated against a strict grammar first and
// the watermark is rendered as a SOQL datetime literal by this package, never by
// the operator.
package salesforcepoll

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
)

const (
	// DefaultPollInterval is the base loop cadence. Per-trigger intervals (which
	// may be longer) are honoured on top of this via __last_polled.
	DefaultPollInterval = 60 * time.Second

	// MinPollInterval is deliberately a whole minute rather than dbrow's ten
	// seconds. Each poll spends one call from the org's daily API allowance, and a
	// Developer Edition org has roughly 15,000 — a ten-second poll would be 8,640
	// a day for ONE trigger and would starve everything else the customer runs.
	MinPollInterval = 60 * time.Second

	// TriggerDefaultInterval is what an operator who leaves the field blank gets.
	//
	// FIFTEEN MINUTES, because the default is what almost everyone will run and it
	// is spending someone else's budget. One poll is one call against the org's
	// daily API allowance, so per trigger per day:
	//
	//	every 60s   1,440 calls   ~10 triggers exhaust a Developer org (~15,000/day)
	//	every 5m      288 calls   ~52 triggers
	//	every 15m      96 calls   ~150 triggers
	//
	// The allowance is shared with everything else the customer does in Salesforce
	// — their own integrations, reports, mobile app — so a default that quietly
	// consumes a tenth of it per trigger is not a neutral choice. Fifteen minutes
	// keeps a realistic number of triggers comfortably inside the budget, and an
	// operator who genuinely needs it faster can say so per trigger. The floor is
	// MinPollInterval; there is no way to go below that.
	TriggerDefaultInterval = 15 * time.Minute

	LeaseDuration = 2 * time.Minute

	// queryTimeout caps a single request so a slow org cannot wedge the loop.
	queryTimeout = 30 * time.Second

	// batchLimit caps how many records one poll fires for. A backlog drains
	// across subsequent polls as the watermark advances.
	batchLimit = 200

	apiVersion = "62.0"

	// State keys in trigger_state.
	sentinelKey   = "__initialized"
	cursorKey     = "__cursor"
	firedKey      = "__fired_at_cursor"
	lastPolledKey = "__last_polled"
)

// soqlTimeLayout is Salesforce's datetime literal format. Literals are NOT
// quoted in SOQL; a quoted value is rejected as a malformed date.
const soqlTimeLayout = "2006-01-02T15:04:05Z"

type triggerConfig struct {
	AccessToken  string `json:"access_token"`
	InstanceURL  string `json:"instance_url"`
	Object       string `json:"object"`
	Event        string `json:"event"`
	PollInterval string `json:"poll_interval"`
	Fields       string `json:"fields"`
	Where        string `json:"where"`
}

// cursorField is the SOQL field the trigger orders and filters on. The editor
// stores the field name itself as the event value, so there is no mapping table
// to drift — but an unrecognised value must never reach the query.
func (c triggerConfig) cursorField() string {
	switch c.Event {
	case "CreatedDate", "SystemModstamp":
		return c.Event
	default:
		// A blank or unknown event means "created or changed", which is the more
		// useful default and the one that cannot miss edits.
		return "SystemModstamp"
	}
}

type Service struct {
	config     *config.Config
	db         *persistence.Service
	trigger    *trigger.Service
	client     *http.Client
	instanceID string
}

func NewService(config *config.Config, db *persistence.Service, trigger *trigger.Service) *Service {
	s := &Service{
		config:     config,
		db:         db,
		trigger:    trigger,
		client:     &http.Client{Timeout: queryTimeout},
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
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeSalesforcePoll)
	if err != nil {
		log.WithFields(log.Fields{"error": err}).Error("unable to get salesforce-poll triggers")
		return
	}

	for _, tr := range triggers {
		s.checkTrigger(tr)
	}
}

func (s *Service) checkTrigger(tr *launch.Trigger) {
	var cfg triggerConfig
	if err := json.Unmarshal(tr.Data, &cfg); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to parse salesforce-poll trigger config")
		return
	}

	// Validate everything that reaches SOQL before anything else. SOQL has no
	// bind variables over REST, so these are interpolated and must be provably
	// safe rather than merely plausible.
	if !validObjectName(cfg.Object) {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "object": cfg.Object}).Warn("salesforce-poll trigger has an invalid object name")
		return
	}
	fields, err := fieldList(cfg.Fields, cfg.cursorField())
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Warn("salesforce-poll trigger has an invalid field list")
		return
	}
	if err := validateWhere(cfg.Where); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Warn("salesforce-poll trigger has an unsafe filter")
		return
	}

	// Claim the trigger so only one instance polls it.
	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, LeaseDuration)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to acquire lease for salesforce-poll trigger")
		return
	}
	if !acquired {
		return
	}

	state, err := s.db.GetTriggerState(tr.ID)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to get trigger state")
		return
	}

	interval := parseInterval(cfg.PollInterval)
	if shouldSkipForInterval(state, interval, time.Now()) {
		return
	}

	// Resolve ${secrets.X}/${env.X} at poll time so the token never rests here.
	token := s.trigger.ResolveString(tr.ID, cfg.AccessToken)
	instanceURL := s.trigger.ResolveString(tr.ID, cfg.InstanceURL)
	host, err := normaliseInstanceURL(instanceURL)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Warn("salesforce-poll trigger has an invalid instance URL")
		return
	}
	if strings.TrimSpace(token) == "" {
		log.WithFields(log.Fields{"trigger_id": tr.ID}).Warn("salesforce-poll trigger has no access token")
		return
	}

	// Record that we polled regardless of outcome, so the interval is respected
	// even when the org is unreachable.
	s.markPolled(tr.ID)

	if _, initialised := state[sentinelKey]; !initialised {
		s.baseline(tr, host, token, cfg, fields)
		return
	}

	watermark, haveWatermark := readCursor(state)
	if !haveWatermark {
		// Baselined against an empty object: everything present now is new.
		watermark = time.Time{}
	}
	alreadyFired := readFired(state)

	soql := buildQuery(cfg.Object, fields, cfg.cursorField(), cfg.Where, watermark, haveWatermark)
	records, err := s.query(host, token, soql)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to query Salesforce for new records")
		return
	}

	fired := 0
	newWatermark := watermark
	firedAtNew := alreadyFired
	for _, rec := range records {
		id := stringField(rec, "Id")
		stamp, ok := recordStamp(rec, cfg.cursorField())
		if !ok || id == "" {
			continue
		}
		// The tie guard: this record shares the boundary second with ones already
		// dispatched, and is one of them.
		if haveWatermark && stamp.Equal(watermark) && alreadyFired[id] {
			continue
		}

		s.fireTrigger(tr, cfg, rec, id, stamp)
		fired++

		switch {
		case stamp.After(newWatermark):
			// Moved into a later second — the remembered set belongs to the old
			// one and must not carry over, or it would grow for ever.
			newWatermark = stamp
			firedAtNew = map[string]bool{id: true}
		case stamp.Equal(newWatermark):
			if firedAtNew == nil {
				firedAtNew = map[string]bool{}
			}
			firedAtNew[id] = true
		}
	}

	if fired > 0 {
		s.storeCursor(tr.ID, newWatermark)
		s.storeFired(tr.ID, firedAtNew)
		log.WithFields(log.Fields{
			"trigger_id": tr.ID, "fired": fired,
			"cursor": newWatermark.UTC().Format(soqlTimeLayout), "at_cursor": len(firedAtNew),
		}).Info("salesforce-poll trigger fired for new records")
	}
}

// baseline records the current high-water mark on the first ever poll so that
// pre-existing records never fire. An object that is empty today stores no
// cursor, and the next poll treats everything as new.
//
// It also remembers every id already sitting at that boundary second — without
// that, the first real poll would re-fire them, since the watermark comparison
// is inclusive.
func (s *Service) baseline(tr *launch.Trigger, host, token string, cfg triggerConfig, fields []string) {
	field := cfg.cursorField()
	soql := fmt.Sprintf("SELECT Id, %s FROM %s%s ORDER BY %s DESC LIMIT 1",
		field, cfg.Object, whereClause(cfg.Where, ""), field)

	records, err := s.query(host, token, soql)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).Error("unable to baseline salesforce-poll trigger")
		return
	}

	if len(records) > 0 {
		if stamp, ok := recordStamp(records[0], field); ok {
			s.storeCursor(tr.ID, stamp)
			// Everything already at this exact second must be suppressed.
			if ids, err := s.idsAt(host, token, cfg, field, stamp); err == nil {
				s.storeFired(tr.ID, ids)
			} else {
				log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID}).
					Warn("unable to record the ids at the baseline second — the first poll may re-fire them")
			}
		}
	}
	s.setState(tr.ID, sentinelKey, map[string]string{"status": "initialised"})
	log.WithFields(log.Fields{"trigger_id": tr.ID, "object": cfg.Object}).Info("salesforce-poll trigger baselined")
}

// idsAt lists the record ids sitting at exactly one timestamp, so they can be
// suppressed by the inclusive watermark comparison.
func (s *Service) idsAt(host, token string, cfg triggerConfig, field string, stamp time.Time) (map[string]bool, error) {
	soql := fmt.Sprintf("SELECT Id FROM %s WHERE %s = %s%s LIMIT %d",
		cfg.Object, field, stamp.UTC().Format(soqlTimeLayout), andClause(cfg.Where), batchLimit)
	records, err := s.query(host, token, soql)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(records))
	for _, r := range records {
		if id := stringField(r, "Id"); id != "" {
			ids[id] = true
		}
	}
	return ids, nil
}

func (s *Service) query(host, token, soql string) ([]map[string]interface{}, error) {
	endpoint := fmt.Sprintf("%s/services/data/v%s/query?q=%s", host, apiVersion, url.QueryEscape(soql))

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Salesforce's error envelope is a JSON ARRAY of {message, errorCode}. Pull
		// the first message out so the log says what is wrong rather than dumping
		// the body — and never echo the token.
		return nil, fmt.Errorf("Salesforce API error (%d): %s", resp.StatusCode, firstErrorMessage(body))
	}

	var out struct {
		Records []map[string]interface{} `json:"records"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Records, nil
}

func (s *Service) fireTrigger(tr *launch.Trigger, cfg triggerConfig, rec map[string]interface{}, id string, stamp time.Time) {
	// Spread the record's fields as top-level keys so a downstream node can read
	// `${Email}` directly, then layer the reserved metadata on top — matching the
	// database-row trigger's shape.
	data := make(map[string]interface{}, len(rec)+6)
	for k, v := range rec {
		if k == "attributes" {
			// Salesforce's per-record type/url envelope is noise in the picker.
			continue
		}
		data[k] = v
	}
	data["record"] = rec
	data["id"] = id
	data["object"] = cfg.Object
	data["event"] = cfg.cursorField()
	data["cursor"] = stamp.UTC().Format(soqlTimeLayout)
	data["triggered_at"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{"error": err, "trigger_id": tr.ID, "record_id": id}).
			Error("unable to fire salesforce-poll trigger")
	}
}

// ── state helpers ───────────────────────────────────────────────────────────

func (s *Service) storeCursor(triggerID string, stamp time.Time) {
	s.setState(triggerID, cursorKey, stamp.UTC().Format(soqlTimeLayout))
}

func (s *Service) storeFired(triggerID string, ids map[string]bool) {
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	// Sorted so the stored value is stable and diffable rather than map-ordered.
	sort.Strings(list)
	s.setState(triggerID, firedKey, list)
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

func readCursor(state map[string]json.RawMessage) (time.Time, bool) {
	raw, ok := state[cursorKey]
	if !ok {
		return time.Time{}, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(soqlTimeLayout, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func readFired(state map[string]json.RawMessage) map[string]bool {
	out := map[string]bool{}
	raw, ok := state[firedKey]
	if !ok {
		return out
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return out
	}
	for _, id := range list {
		out[id] = true
	}
	return out
}

// ── query construction ──────────────────────────────────────────────────────

// buildQuery assembles the poll query. The comparison is deliberately INCLUSIVE
// (>=): Salesforce datetimes are second-granular and ties are real, so an
// exclusive comparison drops every record that shares the boundary second with
// one already dispatched. The caller suppresses the ones it has seen by id.
func buildQuery(object string, fields []string, cursorField, where string, watermark time.Time, haveWatermark bool) string {
	cond := ""
	if haveWatermark {
		cond = fmt.Sprintf("%s >= %s", cursorField, watermark.UTC().Format(soqlTimeLayout))
	}
	return fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s ASC, Id ASC LIMIT %d",
		strings.Join(fields, ", "), object, whereClause(where, cond), cursorField, batchLimit)
}

// whereClause joins the operator's filter and our watermark condition, emitting
// nothing at all when both are empty.
func whereClause(userWhere, cond string) string {
	var parts []string
	if c := strings.TrimSpace(cond); c != "" {
		parts = append(parts, c)
	}
	if w := strings.TrimSpace(userWhere); w != "" {
		// Parenthesised so an OR inside the operator's filter cannot escape the
		// watermark condition and widen the query to the whole object.
		parts = append(parts, "("+w+")")
	}
	if len(parts) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(parts, " AND ")
}

// andClause renders the operator's filter as a trailing AND, for queries that
// already have a condition.
func andClause(userWhere string) string {
	if w := strings.TrimSpace(userWhere); w != "" {
		return " AND (" + w + ")"
	}
	return ""
}

// defaultFields is what an operator who names no fields gets. Id and the cursor
// field are always added, so this is only the human-useful remainder — and every
// name here exists on every standard and custom object.
var defaultFields = []string{"Name", "CreatedDate", "LastModifiedDate"}

// fieldList validates the operator's comma-separated field list and guarantees
// Id and the cursor field are present, since the poller cannot work without them.
func fieldList(raw, cursorField string) ([]string, error) {
	var requested []string
	if strings.TrimSpace(raw) == "" {
		requested = defaultFields
	} else {
		for _, f := range strings.Split(raw, ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if !validFieldName(f) {
				return nil, fmt.Errorf("invalid field name %q", f)
			}
			requested = append(requested, f)
		}
	}

	out := []string{"Id"}
	seen := map[string]bool{"id": true}
	for _, f := range append(requested, cursorField) {
		k := strings.ToLower(f)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out, nil
}

// objectNamePattern matches a Salesforce object API name: standard (Account) or
// custom (My_Object__c). No dots, no spaces, no quotes — nothing that could
// close the FROM clause and start another.
var objectNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,78}$`)

func validObjectName(s string) bool { return objectNamePattern.MatchString(s) }

// fieldNamePattern matches a field API name, permitting ONE dot so a
// relationship field (Account.Name, Owner.Email) can be selected — Salesforce
// allows those in a SELECT and operators reasonably want them.
var fieldNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,78}(\.[A-Za-z][A-Za-z0-9_]{0,78})?$`)

func validFieldName(s string) bool { return fieldNamePattern.MatchString(s) }

// whereForbidden lists what may not appear in the operator's filter. The filter
// is a SOQL fragment by design — an operator writing "Status = 'Open'" is the
// whole point — so it cannot be validated as an identifier. What it must not do
// is terminate the statement or smuggle in a second clause, so the characters and
// keywords that would allow that are refused outright.
var whereForbidden = []string{
	// A comment could hide the rest of our own clause, including the watermark.
	"--", "/*", "*/",
	// SOQL has no statement separator, but a subquery or a stacked clause does
	// real damage: a LIMIT or ORDER BY of the operator's own would override ours
	// and break the cursor.
	" limit ", " order by ", " group by ", " offset ", " for update",
	// Sub-selects can read other objects entirely.
	"select ",
}

func validateWhere(where string) error {
	w := strings.TrimSpace(where)
	if w == "" {
		return nil
	}
	// Balanced quoting matters more than any keyword list: an unbalanced quote is
	// how a fragment escapes its literal and reaches our clause.
	if strings.Count(w, "'")%2 != 0 {
		return fmt.Errorf("filter has an unclosed quote")
	}
	lower := " " + strings.ToLower(w) + " "
	for _, bad := range whereForbidden {
		if strings.Contains(lower, bad) {
			return fmt.Errorf("filter may not contain %q", strings.TrimSpace(bad))
		}
	}
	return nil
}

// normaliseInstanceURL pins the poll target to a Salesforce-owned host. Launch
// polls on a schedule with a live token, so an instance URL that has been
// tampered with would quietly forward that token to somebody else's server —
// which is exactly why this is checked here and not only in the editor.
func normaliseInstanceURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("instance URL is required")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("instance URL must be https")
	}
	if u.User != nil {
		return "", fmt.Errorf("instance URL must not contain credentials")
	}
	host := strings.ToLower(u.Hostname())
	allowed := false
	for _, suffix := range []string{".salesforce.com", ".force.com", ".salesforce.mil", ".cloudforce.com"} {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("instance URL must be a Salesforce host")
	}
	return "https://" + host, nil
}

// ── small helpers ───────────────────────────────────────────────────────────

func stringField(rec map[string]interface{}, key string) string {
	if v, ok := rec[key].(string); ok {
		return v
	}
	return ""
}

// recordStamp reads the cursor field off a record. Salesforce renders datetimes
// as 2026-07-26T13:50:38.000+0000, which is not RFC3339 (no colon in the
// offset), so both forms are attempted.
func recordStamp(rec map[string]interface{}, field string) (time.Time, bool) {
	raw := stringField(rec, field)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339, soqlTimeLayout} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func firstErrorMessage(body []byte) string {
	var arr []struct {
		Message   string `json:"message"`
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		if arr[0].ErrorCode != "" {
			return fmt.Sprintf("%s [%s]", arr[0].Message, arr[0].ErrorCode)
		}
		return arr[0].Message
	}
	// Not the array shape — an auth failure can arrive as {"error":"..."}.
	var obj struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &obj); err == nil && obj.Error != "" {
		if obj.Description != "" {
			return obj.Error + ": " + obj.Description
		}
		return obj.Error
	}
	return "unrecognised error response"
}

func parseInterval(raw string) time.Duration {
	if strings.TrimSpace(raw) == "" {
		return TriggerDefaultInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return TriggerDefaultInterval
	}
	if d < MinPollInterval {
		return MinPollInterval
	}
	return d
}

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
