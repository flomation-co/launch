// Package mqtt holds an MQTT subscription open on behalf of every flow with an
// MQTT trigger, and fires the flow whenever a message arrives.
//
// It is the first trigger in Launch that owns a persistent outbound connection.
// The webhook triggers are pushed to; the poll triggers (schedule, git, S3) wake
// up on a ticker and go looking. MQTT can do neither: a subscription only exists
// for as long as a client is connected to the broker, so something has to stay
// connected.
//
// The design is a poller supervising persistent connections, which keeps the
// real-time delivery MQTT is chosen for while reusing the machinery the pollers
// already rely on:
//
//   - A reconcile loop re-reads the trigger table every 30s and makes the live
//     connections match it — connecting new triggers, dropping deleted ones,
//     and re-dialling any whose config changed. That also makes a restart
//     self-healing: the process comes up holding nothing, and the first tick
//     rebuilds every subscription from the database.
//
//   - Each trigger is leased (the same trigger_lease table the schedule trigger
//     uses) so exactly one Launch instance subscribes to it. This matters more
//     for MQTT than it does for a poller: a broker fans a message out to *every*
//     connected subscriber, so two instances holding the same subscription would
//     run the flow twice per message.
//
//   - Registration is also hooked into trigger create/delete, so saving a flow
//     activates it immediately rather than up to a tick later. The loop is the
//     safety net, not the activation path.
//
// Between reconnects, the broker is what keeps messages from being lost: the
// subscription is durable (clean session off, a stable client ID) so QoS 1 and 2
// messages published while Launch is away are queued and delivered on reconnect.
package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	launch "flomation.app/automate/launch"
	"flomation.app/automate/launch/internal/config"
	"flomation.app/automate/launch/internal/persistence"
	"flomation.app/automate/launch/internal/trigger"
	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	// reconcileInterval is how often the live connections are re-checked against
	// the trigger table. It also renews the leases, so it must stay comfortably
	// below leaseDuration.
	reconcileInterval = 30 * time.Second

	// leaseDuration is how long an instance owns a trigger. On a crash the lease
	// expires and another instance picks the subscription up within a tick of it
	// lapsing. Matches the schedule trigger.
	leaseDuration = 2 * time.Minute

	// leaseSafetyMargin is how long BEFORE the lease actually lapses in the
	// database that we give the subscription up.
	//
	// The margin is the whole point. Another instance may claim the trigger the
	// instant the row expires, and it will connect and subscribe immediately. If
	// we only disconnected once our own clock said the lease had expired, we would
	// still be holding the subscription while that instance arrived — and a broker
	// fans every message out to both subscribers, so the flow would run twice per
	// message. Releasing early makes the handover strictly ordered: we are always
	// gone before anyone else can arrive. The margin covers a full reconcile
	// interval plus slack, so a single missed tick can't strand us.
	leaseSafetyMargin = 45 * time.Second

	// startupDelay lets the process finish booting before the first reconcile.
	startupDelay = 5 * time.Second

	// connectTimeout bounds a dial. Failing is not fatal: paho keeps retrying in
	// the background and the OnConnect handler re-subscribes when it succeeds.
	connectTimeout = 15 * time.Second

	// subscribeTimeout bounds the wait for a SUBACK.
	subscribeTimeout = 15 * time.Second

	// maxPayloadBytes caps what is carried into a flow run. A device streaming
	// megabyte payloads should not be able to push them through the execution
	// pipeline; the message is still delivered, with the payload truncated.
	maxPayloadBytes = 256 * 1024

	// dispatchWorkers bounds how many flow starts MQTT can have in flight at once
	// across every trigger. Each one costs a database read and an internal HTTP
	// call, so this is really a cap on how much of the database pool a chatty
	// broker can take from the rest of the process.
	dispatchWorkers = 8

	// dispatchQueueSize absorbs bursts without dropping messages. Once it is full
	// the subscribe handler blocks, which is what pushes back on the broker.
	dispatchQueueSize = 512
)

// Config is an MQTT trigger's stored node configuration.
type Config struct {
	Protocol   string
	Host       string
	Port       int64
	Username   string
	Password   string
	ClientID   string
	WSPath     string
	CACert     string
	ClientCert string
	ClientKey  string
	Insecure   bool

	Topics     string
	DefaultQoS byte
	ParseJSON  bool
	Durable    bool
}

// defaultPorts mirrors the executor's, so a trigger that leaves Port empty dials
// the same place the actions would.
var defaultPorts = map[string]int64{
	"mqtt":  1883,
	"mqtts": 8883,
	"ws":    8083,
	"wss":   8084,
}

// subscription is one live broker connection, held on behalf of one trigger.
type subscription struct {
	triggerID string
	client    paho.Client
	// fingerprint of the config the connection was built from; a change means the
	// operator edited the node and the connection has to be rebuilt.
	fingerprint string

	// alive is true only once the broker has ACKNOWLEDGED the subscription — not
	// merely once the TCP connection is up. The difference matters: a broker that
	// accepts the connection but denies the topic (an ACL rule) leaves a client
	// that is connected, will never receive a message, and will never reconnect,
	// because as far as the network is concerned nothing is wrong.
	alive atomic.Bool

	// failed records that a SUBSCRIBE was rejected, so the reconcile loop rebuilds
	// the connection instead of trusting it. Without this the zombie above is
	// invisible forever.
	failed atomic.Bool

	// leaseExpiry is when this instance's claim on the trigger lapses. Tracked
	// locally so a database outage can't leave us subscribed on a lease we can no
	// longer prove we hold. Guarded by Service.mu.
	leaseExpiry time.Time
}

// dispatchJob is one inbound message queued for delivery to a flow.
type dispatchJob struct {
	triggerID string
	cfg       Config
	msg       paho.Message
}

// Service supervises the MQTT subscriptions.
type Service struct {
	config  *config.Config
	trigger *trigger.Service
	db      *persistence.Service

	// instanceID identifies this process when leasing triggers. A restart
	// deliberately gets a fresh ID so the old lease lapses and is re-taken.
	instanceID string

	mu   sync.Mutex
	subs map[string]*subscription // triggerID → live subscription

	// dispatch is a bounded queue drained by a fixed worker pool. Starting a flow
	// costs a database read and an HTTP call, and paho will happily hand us
	// messages faster than that: a chatty device on one topic must not be able to
	// exhaust the database pool for every other flow in the process. The queue
	// absorbs bursts; when it fills, the send blocks, which stops us acknowledging
	// packets and lets the broker's in-flight window throttle the publisher. That
	// is real backpressure rather than a silent drop.
	dispatch chan dispatchJob
}

// NewService starts the worker pool and the reconcile loop.
func NewService(cfg *config.Config, db *persistence.Service, triggerSvc *trigger.Service) *Service {
	s := &Service{
		config:     cfg,
		trigger:    triggerSvc,
		db:         db,
		instanceID: uuid.NewString(),
		subs:       make(map[string]*subscription),
		dispatch:   make(chan dispatchJob, dispatchQueueSize),
	}

	for i := 0; i < dispatchWorkers; i++ {
		go s.dispatchWorker()
	}

	go s.watch()

	// Log the pool sizing at startup: a saturated pool shows up as flow-start
	// latency, and an operator debugging that needs to know what the ceiling is
	// without going to the source.
	log.WithFields(log.Fields{
		"dispatch_workers": dispatchWorkers,
		"queue_size":       dispatchQueueSize,
		"reconcile_every":  reconcileInterval.String(),
		"lease_duration":   leaseDuration.String(),
	}).Info("mqtt trigger service started")

	return s
}

// dispatchWorker drains the queue. The pool size is what bounds how many flow
// starts (and therefore database connections and internal HTTP calls) MQTT can
// have in flight at once.
func (s *Service) dispatchWorker() {
	for job := range s.dispatch {
		s.deliver(job.triggerID, job.cfg, job.msg)
	}
}

// watch drives the reconcile loop.
func (s *Service) watch() {
	time.Sleep(startupDelay)

	// The first pass rebuilds every subscription from the database — this is what
	// makes a restart recover without any explicit boot-time replay.
	s.reconcile()

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for range ticker.C {
		s.reconcile()
	}
}

// reconcile makes the live connections match the trigger table.
func (s *Service) reconcile() {
	triggers, err := s.db.GetTriggersByType(launch.TriggerTypeMQTT)
	if err != nil {
		// The broker connection is independent of the database, so it stays up
		// through an outage — and that is a problem, not a feature. If this
		// instance can't reach the database for longer than the lease, a healthy
		// instance will see the lease lapse, take it, and subscribe. Both would
		// then hold the subscription and every message would run the flow twice.
		// Dropping anything whose lease we can no longer prove we hold keeps the
		// guarantee the lease exists to provide.
		log.WithError(err).
			Warn("mqtt trigger: unable to load triggers; releasing any subscription whose lease is lapsing")
		s.releaseExpiringLeases()
		return
	}

	wanted := make(map[string]bool, len(triggers))

	for _, tr := range triggers {
		// GetTriggersByType already filters disabled triggers, but a disabled one
		// slipping through would subscribe a flow the operator turned off.
		if tr.DisabledAt != nil {
			continue
		}

		// The lease is what stops two instances both subscribing and running the
		// flow twice for every message.
		acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, leaseDuration)
		if err != nil {
			// Deliberately NOT disconnecting here, and deliberately not marking the
			// trigger wanted either. The lease is simply not renewed this tick, and
			// releaseExpiringLeases below is what decides whether we have now gone
			// too long without renewing to still be sure we own it. A single failed
			// renewal is survivable; a sustained one must hand the subscription back
			// before the row lapses.
			log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).
				Warn("mqtt trigger: unable to renew the lease this tick")
			wanted[tr.ID] = true
			continue
		}
		if !acquired {
			// Another instance owns it. If we were holding the subscription (it
			// changed hands), let it go so the message isn't delivered twice.
			s.disconnect(tr.ID, "lease taken by another instance")
			continue
		}

		wanted[tr.ID] = true
		s.ensure(tr)
	}

	// Anything we hold that is no longer a live, leased trigger gets dropped.
	s.mu.Lock()
	stale := make([]string, 0)
	for id := range s.subs {
		if !wanted[id] {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()

	for _, id := range stale {
		s.disconnect(id, "trigger removed or disabled")
	}

	// Finally, give up anything whose lease we can no longer prove we hold —
	// including the triggers above whose renewal failed.
	s.releaseExpiringLeases()
}

// ensure brings up the subscription for one trigger, or leaves the existing one
// alone when it is already healthy on the same config.
//
// The check and the reservation happen under a SINGLE lock hold. They must: this
// is called both from the reconcile loop and, on a flow save, straight from the
// HTTP handler, so two goroutines can be here for the same trigger at once. If
// each could observe "nothing connected" and go on to dial, both would connect
// under the same MQTT client ID — and a broker responds to that by kicking the
// older client off. With auto-reconnect on, the evicted client immediately
// reconnects and evicts the other, forever, re-delivering the durable session's
// queued messages on every bounce. Reserving the map slot before releasing the
// lock is what makes that impossible.
func (s *Service) ensure(tr *launch.Trigger) {
	cfg, err := s.parseConfig(tr)
	if err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).
			Warn("mqtt trigger: unusable configuration, not subscribing")
		return
	}

	fingerprint := cfg.fingerprint()

	s.mu.Lock()
	existing, held := s.subs[tr.ID]

	if held && existing.fingerprint == fingerprint && !existing.failed.Load() {
		// Already ours and built from the same config. A connection that is merely
		// DOWN is not something to act on — paho is retrying it, and rebuilding
		// here would just fight with that. Only a rejected subscription (failed)
		// needs intervention, because nothing else will ever retry it.
		existing.leaseExpiry = time.Now().Add(leaseDuration)
		s.mu.Unlock()
		return
	}

	// Take ownership of whatever was there and replace it in the same critical
	// section, so no concurrent caller can dial the same client ID.
	var previous *subscription
	if held {
		previous = existing
	}

	sub := &subscription{
		triggerID:   tr.ID,
		fingerprint: fingerprint,
		leaseExpiry: time.Now().Add(leaseDuration),
	}
	s.subs[tr.ID] = sub
	s.mu.Unlock()

	if previous != nil {
		reason := "configuration changed"
		if previous.failed.Load() {
			reason = "the broker rejected the subscription, rebuilding"
		}
		closeClient(previous, reason)
	}

	if err := s.dial(tr, cfg, sub); err != nil {
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).
			Warn("mqtt trigger: unusable connection settings, not subscribing")

		// Give the slot back, or the loop would think it is connected.
		s.mu.Lock()
		if s.subs[tr.ID] == sub {
			delete(s.subs, tr.ID)
		}
		s.mu.Unlock()
	}
}

// dial builds the client for a subscription the caller has already reserved, and
// starts connecting. It does not wait: paho retries in the background and the
// OnConnect handler subscribes each time it succeeds, so a broker that is down at
// boot simply joins late rather than stalling the reconcile loop.
func (s *Service) dial(tr *launch.Trigger, cfg Config, sub *subscription) error {
	topics, err := parseTopicList(cfg.Topics, cfg.DefaultQoS)
	if err != nil {
		return err
	}

	opts, err := cfg.clientOptions(tr.ID)
	if err != nil {
		return err
	}

	// Subscribing from OnConnect (rather than once, after Connect) is what makes
	// the reconnect path work: paho hands us a fresh session on every reconnect
	// and the subscriptions have to be re-established on it.
	opts.SetOnConnectHandler(func(client paho.Client) {
		filters := make(map[string]byte, len(topics))
		for topic, qos := range topics {
			filters[topic] = qos
		}

		token := client.SubscribeMultiple(filters, func(_ paho.Client, m paho.Message) {
			job := dispatchJob{triggerID: tr.ID, cfg: cfg, msg: m}

			// Blocking when the queue is full is deliberate — it is what pushes back
			// on the broker instead of piling up goroutines. But a full queue means
			// flows are now starting later than their messages arrived, and that must
			// not be invisible: without this the only symptom is unexplained latency.
			select {
			case s.dispatch <- job:
			default:
				log.WithFields(log.Fields{
					"trigger_id": tr.ID,
					"topic":      m.Topic(),
					"queue_size": dispatchQueueSize,
					"workers":    dispatchWorkers,
				}).Warn("mqtt trigger: dispatch queue is full — flow starts are being delayed by a faster broker than the workers can drain")

				s.dispatch <- job
			}
		})

		if !token.WaitTimeout(subscribeTimeout) {
			// Mark it failed, NOT merely not-alive: the TCP connection is fine, so
			// nothing will ever reconnect and retry this on its own. The reconcile
			// loop rebuilds a failed subscription; it leaves a merely-down one to
			// paho.
			sub.failed.Store(true)
			sub.alive.Store(false)
			log.WithField("trigger_id", tr.ID).
				Error("mqtt trigger: timed out subscribing; will rebuild the connection")
			return
		}
		if err := token.Error(); err != nil {
			sub.failed.Store(true)
			sub.alive.Store(false)
			log.WithFields(log.Fields{"trigger_id": tr.ID, "error": redact(cfg, err.Error())}).
				Error("mqtt trigger: the broker refused the subscription — check the topic ACLs for this user")
			return
		}

		// Only now is this connection genuinely able to receive anything.
		sub.failed.Store(false)
		sub.alive.Store(true)

		log.WithFields(log.Fields{
			"trigger_id": tr.ID,
			"broker":     cfg.brokerURL(),
			"topics":     cfg.Topics,
		}).Info("mqtt trigger: subscribed")
	})

	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		sub.alive.Store(false)
		log.WithFields(log.Fields{"trigger_id": tr.ID, "error": err}).
			Warn("mqtt trigger: connection lost, retrying")
	})

	client := paho.NewClient(opts)
	sub.client = client

	// ConnectRetry is on, so this token only reports the FIRST attempt. A failure
	// here is worth logging but is not terminal — paho keeps trying.
	go func() {
		token := client.Connect()
		if !token.WaitTimeout(connectTimeout) {
			log.WithFields(log.Fields{"trigger_id": tr.ID, "broker": cfg.brokerURL()}).
				Warn("mqtt trigger: broker did not answer, still retrying in the background")
			return
		}
		if err := token.Error(); err != nil {
			log.WithFields(log.Fields{
				"trigger_id": tr.ID,
				"broker":     cfg.brokerURL(),
				"error":      redact(cfg, err.Error()),
			}).Error("mqtt trigger: could not connect, still retrying in the background")
		}
	}()

	return nil
}

// deliver turns a broker message into a flow run. It runs on a pool worker, so
// the number of these in flight at once is bounded.
func (s *Service) deliver(triggerID string, cfg Config, m paho.Message) {
	tr, err := s.db.GetTriggerByID(triggerID)
	if err != nil || tr == nil {
		log.WithField("trigger_id", triggerID).
			Warn("mqtt trigger: a message arrived for a trigger that no longer exists")
		return
	}
	// The flow may have been paused between the message being published and this
	// running; firing it anyway would resurrect a disabled trigger.
	if tr.DisabledAt != nil {
		return
	}

	payload := string(m.Payload())
	truncated := false
	if len(payload) > maxPayloadBytes {
		payload = payload[:maxPayloadBytes]
		truncated = true
		log.WithFields(log.Fields{"trigger_id": triggerID, "topic": m.Topic()}).
			Warn("mqtt trigger: payload truncated")
	}

	// These keys are the trigger node's declared Outputs. They have to match by
	// name — the executor materialises them onto the node's inputs and the node
	// echoes them straight out.
	data := map[string]interface{}{
		"topic":       m.Topic(),
		"payload":     payload,
		"qos":         int64(m.Qos()),
		"retained":    m.Retained(),
		"received_at": time.Now().UTC().Format(time.RFC3339),
		"broker":      cfg.brokerURL(),
	}
	if truncated {
		data["truncated"] = true
	}

	if cfg.ParseJSON {
		var decoded interface{}
		if err := json.Unmarshal([]byte(payload), &decoded); err == nil {
			data["payload_json"] = decoded
		}
	}

	if err := s.trigger.Trigger(tr, data); err != nil {
		log.WithFields(log.Fields{
			"trigger_id": triggerID,
			"topic":      m.Topic(),
			"error":      err,
		}).Error("mqtt trigger: could not start the flow")
	}
}

// Register brings a trigger up immediately, rather than at the next tick. Called
// when a flow with an MQTT trigger is saved.
func (s *Service) Register(triggerID string) error {
	tr, err := s.db.GetTriggerByID(triggerID)
	if err != nil {
		return err
	}
	if tr == nil {
		return fmt.Errorf("trigger %s not found", triggerID)
	}
	if tr.Type != launch.TriggerTypeMQTT {
		return nil
	}
	if tr.DisabledAt != nil {
		return nil
	}

	acquired, err := s.db.TryAcquireLease(tr.ID, s.instanceID, leaseDuration)
	if err != nil {
		return err
	}
	if !acquired {
		// Another instance owns it and will subscribe. Saving still succeeded.
		log.WithField("trigger_id", triggerID).
			Info("mqtt trigger: another instance holds the lease, it will subscribe")
		return nil
	}

	s.ensure(tr)
	return nil
}

// Deregister tears the subscription down. Called when the trigger is deleted or
// the flow stops using it.
func (s *Service) Deregister(triggerID string) {
	s.disconnect(triggerID, "trigger deleted")
}

// disconnect drops a subscription if we hold one.
func (s *Service) disconnect(triggerID, reason string) {
	s.mu.Lock()
	sub, held := s.subs[triggerID]
	if held {
		delete(s.subs, triggerID)
	}
	s.mu.Unlock()

	if !held {
		return
	}

	closeClient(sub, reason)
}

// releaseExpiringLeases gives up any subscription whose lease is close enough to
// lapsing that we can no longer be sure we still own it.
//
// It releases on a safety margin rather than on the true expiry, and the
// difference is the whole correctness argument: the moment the row lapses,
// another instance may claim the trigger and subscribe. Waiting for our own clock
// to agree the lease had expired would leave both of us subscribed at once, and
// the broker would deliver every message to both — running the flow twice. By
// letting go early we guarantee we are gone before anyone else can arrive.
func (s *Service) releaseExpiringLeases() {
	// Anything whose lease expires within the margin is treated as already lost.
	cutoff := time.Now().Add(leaseSafetyMargin)

	s.mu.Lock()
	expiring := make([]string, 0)
	for id, sub := range s.subs {
		if cutoff.After(sub.leaseExpiry) {
			expiring = append(expiring, id)
		}
	}
	s.mu.Unlock()

	for _, id := range expiring {
		s.disconnect(id, "the lease is about to lapse and could not be renewed — releasing it before another instance can take it")
	}
}

// closeClient tears one connection down. Kept separate from disconnect so a
// subscription that has already been removed from the map (replaced by a rebuild)
// can still be closed cleanly rather than orphaned.
func closeClient(sub *subscription, reason string) {
	sub.alive.Store(false)
	if sub.client != nil {
		sub.client.Disconnect(250)
	}

	log.WithFields(log.Fields{"trigger_id": sub.triggerID, "reason": reason}).
		Info("mqtt trigger: unsubscribed")
}

// ActiveCount reports how many subscriptions this instance holds. Used by the
// health endpoint and the tests.
func (s *Service) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// -- configuration -----------------------------------------------------------

// parseConfig reads the trigger's stored node config and resolves its secrets.
// Values arrive as whatever the editor stored — a port may be a number or a
// string, a checkbox a bool or "true" — so every read is lenient.
func (s *Service) parseConfig(tr *launch.Trigger) (Config, error) {
	raw := map[string]interface{}{}
	if len(tr.Data) > 0 {
		if err := json.Unmarshal(tr.Data, &raw); err != nil {
			return Config{}, fmt.Errorf("could not read the trigger configuration: %w", err)
		}
	}

	// Secrets are stored as ${secrets.X} references and resolved against the
	// flow's environment at use time, never persisted in the clear.
	str := func(key string) string {
		return strings.TrimSpace(s.trigger.ResolveString(tr.ID, asString(raw[key])))
	}

	cfg := Config{
		Protocol:   strings.ToLower(str("protocol")),
		Host:       stripScheme(str("host")),
		Username:   str("username"),
		Password:   str("password"),
		ClientID:   str("client_id"),
		WSPath:     str("ws_path"),
		CACert:     str("ca_certificate"),
		ClientCert: str("client_certificate"),
		ClientKey:  str("client_key"),
		Insecure:   asBool(raw["allow_insecure"], false),
		Topics:     str("topics"),
		ParseJSON:  asBool(raw["parse_json"], false),
		// Durable defaults ON: without it the broker forgets the subscription the
		// moment the connection drops, and messages published during a restart are
		// lost with no trace.
		Durable: asBool(raw["durable"], true),
	}

	if cfg.Protocol == "" {
		cfg.Protocol = "mqtt"
	}
	if _, ok := defaultPorts[cfg.Protocol]; !ok {
		return Config{}, fmt.Errorf("unsupported protocol %q", cfg.Protocol)
	}
	if cfg.Host == "" {
		return Config{}, fmt.Errorf("no broker host configured")
	}

	if port, ok := asInt(raw["port"]); ok {
		if port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("port %d is out of range", port)
		}
		cfg.Port = port
	} else {
		cfg.Port = defaultPorts[cfg.Protocol]
	}

	if cfg.WSPath == "" {
		cfg.WSPath = "/mqtt"
	}
	if !strings.HasPrefix(cfg.WSPath, "/") {
		cfg.WSPath = "/" + cfg.WSPath
	}

	if cfg.Topics == "" {
		return Config{}, fmt.Errorf("no topics configured")
	}

	if q, ok := asInt(raw["default_qos"]); ok && q >= 0 && q <= 2 {
		cfg.DefaultQoS = byte(q)
	}

	if (cfg.ClientCert == "") != (cfg.ClientKey == "") {
		return Config{}, fmt.Errorf("mutual TLS needs both a client certificate and a client key")
	}

	return cfg, nil
}

// brokerURL is the address paho dials.
func (c Config) brokerURL() string {
	base := fmt.Sprintf("%s://%s:%d", c.Protocol, c.Host, c.Port)
	if c.Protocol == "ws" || c.Protocol == "wss" {
		return base + c.WSPath
	}
	return base
}

// clientID is stable per trigger, and deliberately so: it is the identity the
// broker keys the durable session off, so the queued messages waiting for this
// flow are only delivered back to a client that reconnects under the same name.
// The "flo-" prefix costs 4 of the 23 bytes MQTT 3.1 allows, leaving 19 — still
// 76 bits of a UUID, so a collision between two triggers is not a real risk.
func clientID(triggerID string) string {
	id := strings.ReplaceAll(triggerID, "-", "")
	if len(id) > 19 {
		id = id[:19]
	}
	return "flo-" + id
}

// fingerprint identifies the config a connection was built from, so an edit to
// the node is detected and the connection rebuilt. It deliberately covers the
// credentials too — a rotated password has to force a re-dial.
func (c Config) fingerprint() string {
	parts := []string{
		c.Protocol, c.Host, strconv.FormatInt(c.Port, 10), c.Username, c.Password,
		c.ClientID, c.WSPath, c.CACert, c.ClientCert, c.ClientKey,
		strconv.FormatBool(c.Insecure), c.Topics, strconv.Itoa(int(c.DefaultQoS)),
		strconv.FormatBool(c.ParseJSON), strconv.FormatBool(c.Durable),
	}
	return strings.Join(parts, "\x00")
}

// clientOptions builds the paho options for a durable, self-reconnecting
// subscriber.
func (c Config) clientOptions(triggerID string) (*paho.ClientOptions, error) {
	opts := paho.NewClientOptions()
	opts.AddBroker(c.brokerURL())

	// Some brokers pin the client ID in their access rules — AWS IoT Core wants the
	// thing name, Azure IoT Hub the device ID — and refuse the connection outright
	// otherwise. When the operator has set one, it IS the identity the durable
	// session hangs off; otherwise derive a stable one from the trigger.
	if c.ClientID != "" {
		opts.SetClientID(c.ClientID)
	} else {
		opts.SetClientID(clientID(triggerID))
	}

	if c.Username != "" {
		opts.SetUsername(c.Username)
	}
	if c.Password != "" {
		opts.SetPassword(c.Password)
	}

	if c.Protocol == "mqtts" || c.Protocol == "wss" {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: c.Host,
		}

		if c.CACert != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(formatPEM(c.CACert))) {
				return nil, fmt.Errorf("the CA certificate could not be parsed")
			}
			tlsCfg.RootCAs = pool
		}
		if c.ClientCert != "" && c.ClientKey != "" {
			pair, err := tls.X509KeyPair([]byte(formatPEM(c.ClientCert)), []byte(formatPEM(c.ClientKey)))
			if err != nil {
				return nil, fmt.Errorf("the client certificate and key could not be loaded: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{pair}
		}
		if c.Insecure {
			// #nosec G402 — opt-in only, gated behind the allow_insecure setting on
			// the trigger node so a self-signed broker can be used deliberately.
			tlsCfg.InsecureSkipVerify = true
		}

		opts.SetTLSConfig(tlsCfg)
	}

	// Clean session OFF is what makes the subscription durable: the broker keeps
	// this client's subscriptions and queues its QoS 1/2 messages while it is
	// away, then delivers the backlog on reconnect. That is the whole reason a
	// Launch restart doesn't drop messages.
	opts.SetCleanSession(!c.Durable)

	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(10 * time.Second)
	opts.SetMaxReconnectInterval(2 * time.Minute)
	opts.SetConnectTimeout(connectTimeout)
	opts.SetKeepAlive(30 * time.Second)

	// OrderMatters keeps paho dispatching on a single goroutine rather than
	// spawning a fresh one per message. That is what makes the bounded queue mean
	// anything: with paho spawning goroutines, a burst would pile up thousands of
	// them regardless of our worker pool. Serial dispatch into a bounded queue
	// gives real backpressure — and, as a bonus, messages reach the queue in the
	// order the broker sent them.
	opts.SetOrderMatters(true)

	return opts, nil
}

// redact keeps the broker password out of logs.
func redact(c Config, msg string) string {
	if c.Password == "" {
		return msg
	}
	return strings.ReplaceAll(msg, c.Password, "********")
}

// stripScheme tolerates a host stored as a full broker URL.
func stripScheme(host string) string {
	for _, scheme := range []string{"mqtts://", "mqtt://", "wss://", "ws://", "tcp://", "ssl://"} {
		if strings.HasPrefix(strings.ToLower(host), scheme) {
			host = host[len(scheme):]
			break
		}
	}
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	return strings.TrimSpace(host)
}

// parseTopicList reads "sensors/#:1,alerts:2,plain" — the comma-separated form
// MQTT tooling has settled on, where a trailing :0-2 overrides the default QoS.
func parseTopicList(raw string, defaultQoS byte) (map[string]byte, error) {
	topics := map[string]byte{}

	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		topic := entry
		qos := defaultQoS

		// A colon is legal inside a topic, so only split on the LAST one. A numeric
		// suffix is always taken as a QoS and clamped: otherwise a typo such as
		// "sensors/temp:3" is read as a topic literally NAMED "sensors/temp:3",
		// which subscribes happily, never matches a message, and reports nothing
		// wrong.
		if i := strings.LastIndex(entry, ":"); i > 0 {
			if suffix := strings.TrimSpace(entry[i+1:]); isNumeric(suffix) {
				topic = strings.TrimSpace(entry[:i])
				qos = clampQoS(suffix)
			}
		}

		if topic != "" {
			topics[topic] = qos
		}
	}

	if len(topics) == 0 {
		return nil, fmt.Errorf("no topics to subscribe to")
	}

	return topics, nil
}

// isNumeric reports whether s is a bare integer (a leading "-" allowed, so an
// out-of-range "-1" is still recognised as an attempted QoS and clamped).
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// clampQoS reads a QoS suffix, pinning anything outside 0-2 to 0.
func clampQoS(s string) byte {
	q, err := strconv.Atoi(s)
	if err != nil || q < 0 || q > 2 {
		return 0
	}
	return byte(q)
}

// pemBlock matches a PEM whose newlines have been flattened away.
var pemBlock = regexp.MustCompile(`(-----BEGIN [A-Z0-9 ]+-----)\s*(.*?)\s*(-----END [A-Z0-9 ]+-----)`)

// formatPEM repairs a certificate whose line breaks were lost. Pasting a cert
// through a single-line field or carrying it in an environment variable flattens
// it to one line, which every PEM parser rejects — with an error that says
// nothing about newlines.
func formatPEM(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "\n") {
		return s
	}

	return pemBlock.ReplaceAllStringFunc(s, func(block string) string {
		parts := pemBlock.FindStringSubmatch(block)
		if len(parts) != 4 {
			return block
		}

		header, body, footer := parts[1], strings.ReplaceAll(parts[2], " ", ""), parts[3]

		var b strings.Builder
		b.WriteString(header)
		b.WriteString("\n")
		for i := 0; i < len(body); i += 64 {
			end := i + 64
			if end > len(body) {
				end = len(body)
			}
			b.WriteString(body[i:end])
			b.WriteString("\n")
		}
		b.WriteString(footer)

		return b.String()
	})
}

// -- lenient value coercion --------------------------------------------------

func asString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asInt(v interface{}) (int64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func asBool(v interface{}, fallback bool) bool {
	switch t := v.(type) {
	case nil:
		return fallback
	case bool:
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return fallback
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fallback
		}
		return b
	}
	return fallback
}
