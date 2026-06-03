package metrics

import (
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"
)

// ── Counters (incremented inline by handlers) ────────────────────────

// InboundMessagesTotal is incremented when a message arrives from an
// external channel (Slack, Telegram, webhook, etc.).
var InboundMessagesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "flomation_inbound_messages_total",
	Help: "Total inbound messages by channel type.",
}, []string{"channel_type"})

// TriggerFiresTotal is incremented each time a trigger fires.
var TriggerFiresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "flomation_trigger_fires_total",
	Help: "Total trigger fires by type.",
}, []string{"type"})

// FlowDispatchesTotal is incremented when a flow execution is dispatched.
var FlowDispatchesTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "flomation_flow_dispatches_total",
	Help: "Total flow executions dispatched since service start.",
})

// ── Gauges (updated by the periodic collector) ──────────────────────

// triggersActive tracks the number of active triggers.
var triggersActive = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "flomation_triggers_active",
	Help: "Number of active triggers.",
})

// AgentsManaged tracks the number of agents this instance is managing.
var AgentsManaged = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "flomation_agents_managed",
	Help: "Number of agents managed by this instance.",
})

// SocketConnectionsActive tracks active Slack Socket Mode connections.
var SocketConnectionsActive = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "flomation_socket_connections_active",
	Help: "Active Slack Socket Mode connections.",
})

// StartCollector launches a background goroutine that periodically
// queries the database to update gauge metrics.
func StartCollector(db *sqlx.DB, interval time.Duration) {
	go func() {
		time.Sleep(5 * time.Second)
		collect(db)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			collect(db)
		}
	}()
	log.WithField("interval", interval).Info("metrics collector started")
}

func collect(db *sqlx.DB) {
	var count int64

	// Active triggers (not disabled)
	if err := db.Get(&count, `SELECT COUNT(*) FROM trigger WHERE disabled_at IS NULL`); err == nil {
		triggersActive.Set(float64(count))
	}
}
