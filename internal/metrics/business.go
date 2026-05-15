package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

// ── Gauges (set inline — no periodic collector needed) ──────────────

// TriggersActive tracks the number of active triggers.
var TriggersActive = promauto.NewGauge(prometheus.GaugeOpts{
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
