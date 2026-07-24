package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	launch "flomation.app/automate/launch"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// embedSchemaVersion is the version of the public form-definition projection
// contract the SDK consumes. Bump it when the projection's shape changes so an
// old SDK can detect an incompatible payload.
const embedSchemaVersion = 1

// publishableKeyHeader carries the developer's publishable key on every embed
// request. It is safe in client JS — the origin allowlist + per-resource opt-in
// + server-side re-validation are what actually gate access.
const publishableKeyHeader = "X-Flomation-Publishable-Key"

// Embeddable resource types (mirrors api.EmbedResource*). Forms are gated by
// their owning FLOW (see embedFormGate), so "flow" is the type checked today;
// direct flow-invocation and agent-chat routes (which add "agent") arrive in a
// later phase.
const embedResourceFlow = "flow"

// embedPreflight answers a CORS preflight (OPTIONS) for an embed route: it
// reflects the Origin and advertises the allowed methods/headers so the browser
// proceeds to the real, key-validated request. No key is checked here — browsers
// don't send custom headers on preflight, and a preflight exposes no data.
func (s *Service) embedPreflight(c *gin.Context) {
	s.setEmbedCORS(c, c.GetHeader("Origin"))
	c.AbortWithStatus(http.StatusNoContent)
}

// embedResolution mirrors api.EmbedResolution — the verdict the API returns for
// a (publishable key, origin, resource) tuple.
type embedResolution struct {
	EmbedAppID      string  `json:"embed_app_id"`
	OrganisationID  *string `json:"organisation_id"`
	OwnerID         string  `json:"owner_id"`
	OriginAllowed   bool    `json:"origin_allowed"`
	ResourceAllowed bool    `json:"resource_allowed"`
}

// resolveEmbed asks the API (internal, mTLS) whether a publishable key is valid
// and whether the given origin + resource are permitted. A nil result with a nil
// error means the key is unknown (the caller returns 401).
func (s *Service) resolveEmbed(pk, origin, resourceType, resourceID string) (*embedResolution, error) {
	body, _ := json.Marshal(map[string]string{
		"publishable_key": pk,
		"origin":          origin,
		"resource_type":   resourceType,
		"resource_id":     resourceID,
	})
	url := fmt.Sprintf("%v/api/v1/internal/embed/resolve", s.config.InternalAPIURL())
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // unknown key
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed resolve returned status %d", resp.StatusCode)
	}
	var res embedResolution
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

// applyEmbedGate runs the shared gate: validate the publishable key, resolve the
// (key, origin, resource) tuple via the API, enforce the origin allowlist and the
// per-resource opt-in, and — only on success — reflect the caller's Origin so the
// browser accepts the response and stash the org for downstream handlers. Returns
// false (and writes the appropriate status) when the request is denied. The
// underlying handler still re-validates the payload, so this controls access, not
// trust.
func (s *Service) applyEmbedGate(c *gin.Context, resourceType, resourceID string) bool {
	origin := c.GetHeader("Origin")
	pk := c.GetHeader(publishableKeyHeader)
	if pk == "" {
		c.AbortWithStatus(http.StatusUnauthorized)
		return false
	}
	res, err := s.resolveEmbed(pk, origin, resourceType, resourceID)
	if err != nil {
		log.WithError(err).Error("embed gate: unable to resolve publishable key")
		c.AbortWithStatus(http.StatusBadGateway)
		return false
	}
	if res == nil {
		c.AbortWithStatus(http.StatusUnauthorized) // unknown key
		return false
	}
	if !res.OriginAllowed || !res.ResourceAllowed {
		c.AbortWithStatus(http.StatusForbidden)
		return false
	}
	s.setEmbedCORS(c, origin)
	if res.OrganisationID != nil {
		c.Set("embed_organisation_id", *res.OrganisationID)
	}
	c.Set("embed_app_id", res.EmbedAppID)
	return true
}

// embedFormGate guards a form embed by its owning FLOW. A form is embeddable when
// its flow is opted in for the app — matching the editor, where a developer opts
// flows (and agents) in, not individual form triggers. It resolves the form
// trigger's flow id and checks the "flow" resource.
func (s *Service) embedFormGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if uuid.Validate(id) != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		tr, err := s.trigger.GetTriggerByID(id)
		if err != nil || tr == nil || tr.Type != launch.TriggerTypeForm || tr.FlowID == "" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if s.applyEmbedGate(c, embedResourceFlow, tr.FlowID) {
			c.Next()
		}
	}
}

// setEmbedCORS reflects the specific request Origin (never "*") and advertises
// the methods/headers the SDK needs, including credentials so a login-gated form
// can forward the end-user's session cookie/JWT.
func (s *Service) setEmbedCORS(c *gin.Context, origin string) {
	if origin != "" {
		c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
		c.Writer.Header().Set("Vary", "Origin")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, "+publishableKeyHeader)
}

// ── Public form-definition projection (the read-path security boundary) ──

// publicComponentKeys is the ALLOWLIST of form-component JSON keys exposed to the
// SDK. This is default-deny: a field not listed here is never sent, so adding a
// new sensitive field to formComponent cannot accidentally leak. Notably absent:
// payment_secret, value_source, value_output — internal ids/secrets the client
// must never see. The client learns a field is flow-computed via the derived
// "computed" flag and calls /compute (where the server holds the real flow id).
var publicComponentKeys = map[string]struct{}{
	"name": {}, "label": {}, "type": {}, "placeholder": {}, "required": {},
	"order": {}, "read_only": {}, "default_value": {},
	"options": {}, "options_source": {}, "multiple": {},
	"matrix_rows": {}, "matrix_columns": {}, "cell_type": {},
	"min": {}, "max": {}, "step": {}, "integer_only": {}, "scale": {},
	"min_date": {}, "max_date": {}, "display_text": {}, "precision": {},
	"scale_label_low": {}, "scale_label_high": {}, "contact_fields": {},
	"accept_mime": {}, "max_size_bytes": {}, "allow_gallery": {},
	"capture_mode": {}, "auto_submit": {}, "confidence_threshold": {},
	"privacy_notice": {}, "show_privacy_notice": {},
	"amount": {}, "currency": {}, "allow_copy": {}, "visible_if": {},
	// Table (data-grid) field. rows_source names a data-source OUTPUT key (safe,
	// like options_source — not an internal flow id), so it is exposed. Manual
	// table_rows are the display data. No internal ids here.
	"table_columns": {}, "table_rows": {}, "rows_source": {}, "selection_mode": {},
	"value_column": {}, "page_size": {}, "filterable": {},
}

// projectComponent reduces a component to its allowlisted keys plus derived
// safe flags. Marshalling then filtering keeps us in lock-step with the JSON
// tags without hand-copying 40 fields.
func projectComponent(comp formComponent) map[string]interface{} {
	raw, _ := json.Marshal(comp)
	var all map[string]interface{}
	_ = json.Unmarshal(raw, &all)

	out := make(map[string]interface{}, len(all))
	for k, v := range all {
		if _, ok := publicComponentKeys[k]; ok {
			out[k] = v
		}
	}
	// Derived flag: this field's value is produced server-side by a flow, so the
	// SDK must fetch it via /compute rather than trusting a client value.
	if comp.ValueSource != "" {
		out["computed"] = true
	}
	return out
}

// projectDefinition builds the public projection of a form definition: safe
// component keys, page visibility, the submit config, and two safe flags telling
// the client when to call /data (dynamic values/options) — never the underlying
// flow id. Includes the schema version so an SDK can detect an incompatible
// payload.
func projectDefinition(def formDefinition) map[string]interface{} {
	pages := make([]map[string]interface{}, 0, len(def.Pages))
	for _, p := range def.Pages {
		comps := make([]map[string]interface{}, 0, len(p.Components))
		for _, comp := range p.Components {
			comps = append(comps, projectComponent(comp))
		}
		page := map[string]interface{}{"components": comps}
		if p.VisibleIf != nil {
			page["visible_if"] = p.VisibleIf
		}
		pages = append(pages, page)
	}

	out := map[string]interface{}{
		"schema_version": embedSchemaVersion,
		"title":          def.Title,
		"description":    def.Description,
		"pages":          pages,
		"require_login":  def.RequireLogin,
		// has_data_source tells the client to GET /data for ${data.X} values and
		// dynamic options — without ever exposing the data-source flow id.
		"has_data_source": def.DataSource != nil && def.DataSource.FlowID != "",
	}
	if def.Submit != nil {
		out["submit"] = def.Submit
	}
	return out
}

// handleEmbedFormDefinition serves the public projection of a form definition to
// the SDK. The embed gate has already validated the key/origin/resource, so this
// just loads the trigger, resolves render-time substitutions, and returns JSON.
//
// Render-time substitution mirrors the hosted form path (handleForm): a
// logged-in user (Bearer token or flomation-token cookie — the SDK forwards the
// former via getAuthToken) gets ${user.X} resolved, and ${query.X} comes from the
// request URL regardless of auth state. ${data.X} is deliberately left intact for
// the client to resolve lazily per page (DataVariables stays nil). Without this,
// a login-gated embedded form projected ${user.email} etc. verbatim.
func (s *Service) handleEmbedFormDefinition(c *gin.Context) {
	id := c.Param("id")
	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil || tr == nil || tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	def, perr := parseFormDefinition(tr.Data)
	if perr != nil {
		log.WithError(perr).Warn("embed: could not parse form definition")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	cookie, _ := c.Cookie("flomation-token")
	token := extractSessionToken(c.GetHeader("Authorization"), cookie)
	userID := s.resolveSessionUser(token)
	ctx := substitutionContext{QueryParams: queryParamsMap(c)}
	if userID != "" {
		if vars, verr := s.loadUserVariables(userID); verr == nil {
			ctx.UserVariables = vars
		} else {
			log.WithError(verr).Warn("embed: failed to load user variables; projecting without ${user.X}")
		}
	}
	c.JSON(http.StatusOK, projectDefinition(resolveFormForRender(def, ctx)))
}

// handleEmbedFormSession mints a server-side draft for an embedded form and
// returns its submission id. The SDK calls this once on load so that autosave,
// payments and the stateful submit gate — all of which live on the draft — work
// over the embed path. The Launch-hosted page gets its draft from handleForm,
// but the SDK never loads that HTML, so it needs an explicit session.
func (s *Service) handleEmbedFormSession(c *gin.Context) {
	id := c.Param("id")
	tr, err := s.trigger.GetTriggerByID(id)
	if err != nil || tr == nil || tr.Type != launch.TriggerTypeForm {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	submissionID := uuid.NewString()
	if cerr := s.db.CreateFormDraft(submissionID, id, tr.FlowID, formDraftTTL); cerr != nil {
		log.WithError(cerr).Error("embed: unable to create form session draft")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.JSON(http.StatusOK, gin.H{"submission_id": submissionID})
}
