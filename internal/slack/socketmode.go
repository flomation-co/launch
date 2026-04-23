package slack

import (
	"context"
	"fmt"
	stdlog "log"
	"os"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	log "github.com/sirupsen/logrus"
)

// MessageHandler is called when a Socket Mode connection receives a message event.
type MessageHandler func(msg *ParsedMessage)

// InteractionHandler is called when a Socket Mode connection receives an interaction event.
type InteractionHandler func(payload *InteractionPayload)

// SocketClient manages a Socket Mode WebSocket connection for a single agent.
type SocketClient struct {
	agentID        string
	api            *slack.Client
	sm             *socketmode.Client
	cancel         context.CancelFunc
	onMessage      MessageHandler
	onInteract     InteractionHandler
	presenceOnce   sync.Once
}

// SocketManager tracks active Socket Mode connections across agents.
type SocketManager struct {
	mu      sync.RWMutex
	clients map[string]*SocketClient // agentID → client
}

// NewSocketManager creates a manager for Socket Mode connections.
func NewSocketManager() *SocketManager {
	return &SocketManager{
		clients: make(map[string]*SocketClient),
	}
}

// Connect starts a Socket Mode connection for an agent. Requires an app-level
// token (xapp-...) for the WebSocket and a bot token (xoxb-...) for API calls.
// The connection sets the bot's presence to "auto" so it shows as online.
func (m *SocketManager) Connect(agentID, appToken, botToken string, onMessage MessageHandler, onInteract InteractionHandler) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Disconnect existing connection if re-registering
	if existing, ok := m.clients[agentID]; ok {
		existing.cancel()
		delete(m.clients, agentID)
	}

	api := slack.New(botToken,
		slack.OptionAppLevelToken(appToken),
	)

	sm := socketmode.New(api,
		socketmode.OptionLog(stdlog.New(os.Stderr, "slack-sm: ", stdlog.Lshortfile|stdlog.LstdFlags)),
	)

	ctx, cancel := context.WithCancel(context.Background())

	client := &SocketClient{
		agentID:    agentID,
		api:        api,
		sm:         sm,
		cancel:     cancel,
		onMessage:  onMessage,
		onInteract: onInteract,
	}

	m.clients[agentID] = client

	// Start event handler in background (also manages presence)
	go client.handleEvents(ctx)

	// Start Socket Mode connection in background
	go func() {
		if err := sm.RunContext(ctx); err != nil && ctx.Err() == nil {
			log.WithFields(log.Fields{
				"agent_id": agentID,
				"error":    err,
			}).Error("slack socket mode connection closed unexpectedly")
		}
	}()

	log.WithField("agent_id", agentID).Info("slack socket mode connected")
	return nil
}

// Disconnect closes the Socket Mode connection for an agent.
func (m *SocketManager) Disconnect(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	client, ok := m.clients[agentID]
	if !ok {
		return
	}

	// Set presence to away before disconnecting
	if err := client.api.SetUserPresence("away"); err != nil {
		log.WithFields(log.Fields{
			"agent_id": agentID,
			"error":    err,
		}).Warn("failed to set bot presence to away on disconnect")
	}

	client.cancel()
	delete(m.clients, agentID)
	log.WithField("agent_id", agentID).Info("slack socket mode disconnected")
}

// DisconnectAll closes all active Socket Mode connections.
func (m *SocketManager) DisconnectAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, client := range m.clients {
		client.cancel()
		delete(m.clients, id)
	}
}

// IsConnected returns whether an agent has an active Socket Mode connection.
func (m *SocketManager) IsConnected(agentID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.clients[agentID]
	return ok
}

// handleEvents processes incoming Socket Mode events and dispatches them
// to the appropriate handler.
func (c *SocketClient) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-c.sm.Events:
			if !ok {
				return
			}
			c.processEvent(ctx, evt)
		}
	}
}

func (c *SocketClient) processEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		c.handleEventsAPI(evt)
	case socketmode.EventTypeInteractive:
		c.handleInteractive(evt)
	case socketmode.EventTypeConnecting:
		log.WithField("agent_id", c.agentID).Debug("slack socket mode connecting...")
	case socketmode.EventTypeConnected:
		log.WithField("agent_id", c.agentID).Info("slack socket mode connected")
		c.presenceOnce.Do(func() { go c.presenceLoop(ctx) })
	case socketmode.EventTypeConnectionError:
		log.WithField("agent_id", c.agentID).Warn("slack socket mode connection error")
	case socketmode.EventTypeHello:
		log.WithField("agent_id", c.agentID).Debug("slack socket mode hello received")
	default:
		log.WithFields(log.Fields{
			"agent_id":   c.agentID,
			"event_type": evt.Type,
		}).Debug("socket mode: unhandled event type")
	}
}

// presenceLoop sets the bot to active and refreshes every 15 minutes.
func (c *SocketClient) presenceLoop(ctx context.Context) {
	setPresence := func() bool {
		if err := c.api.SetUserPresence("active"); err != nil {
			return false
		}
		return true
	}

	// Retry until presence is accepted (connection needs time to stabilise)
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if setPresence() {
			log.WithField("agent_id", c.agentID).Info("slack bot presence set to active")
			break
		}
	}

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			setPresence()
		}
	}
}

func (c *SocketClient) handleEventsAPI(evt socketmode.Event) {
	eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		log.WithField("agent_id", c.agentID).Debug("socket mode: event data is not EventsAPIEvent")
		return
	}

	// Always acknowledge the event
	c.sm.Ack(*evt.Request)

	log.WithFields(log.Fields{
		"agent_id":   c.agentID,
		"event_type": eventsAPI.Type,
		"inner_type": eventsAPI.InnerEvent.Type,
	}).Debug("socket mode: received events API event")

	if eventsAPI.Type != slackevents.CallbackEvent {
		return
	}

	switch ev := eventsAPI.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		// Skip bot messages and subtypes
		if ev.SubType != "" || ev.BotID != "" {
			log.WithFields(log.Fields{
				"agent_id": c.agentID,
				"subtype":  ev.SubType,
				"bot_id":   ev.BotID,
			}).Debug("socket mode: skipping bot/subtype message")
			return
		}

		msg := &ParsedMessage{
			Text:      ev.Text,
			UserID:    ev.User,
			ChannelID: ev.Channel,
			Timestamp: ev.TimeStamp,
			ThreadTS:  ev.ThreadTimeStamp,
			TeamID:    eventsAPI.TeamID,
			EventID:   fmt.Sprintf("sm_%s", ev.TimeStamp),
			EventType: "message",
		}

		if c.onMessage != nil {
			c.onMessage(msg)
		}

	case *slackevents.AppMentionEvent:
		msg := &ParsedMessage{
			Text:      ev.Text,
			UserID:    ev.User,
			ChannelID: ev.Channel,
			Timestamp: ev.TimeStamp,
			ThreadTS:  ev.ThreadTimeStamp,
			TeamID:    eventsAPI.TeamID,
			EventID:   fmt.Sprintf("sm_%s", ev.TimeStamp),
			EventType: "app_mention",
		}

		if c.onMessage != nil {
			c.onMessage(msg)
		}
	}
}

func (c *SocketClient) handleInteractive(evt socketmode.Event) {
	callback, ok := evt.Data.(slack.InteractionCallback)
	if !ok {
		return
	}

	// Acknowledge the interaction
	c.sm.Ack(*evt.Request)

	payload := &InteractionPayload{
		Type:        string(callback.Type),
		ResponseURL: callback.ResponseURL,
		TriggerID:   callback.TriggerID,
	}
	payload.User.ID = callback.User.ID
	payload.User.Username = callback.User.Name
	payload.User.Name = callback.User.RealName
	payload.Channel.ID = callback.Channel.ID
	payload.Channel.Name = callback.Channel.Name
	if callback.Message.Msg.Timestamp != "" {
		payload.Message.TS = callback.Message.Msg.Timestamp
		payload.Message.Text = callback.Message.Msg.Text
	}

	for _, action := range callback.ActionCallback.BlockActions {
		ia := InteractionAction{
			Type:     string(action.Type),
			ActionID: action.ActionID,
			BlockID:  action.BlockID,
			Value:    action.Value,
		}
		ia.Text.Text = action.Text.Text
		ia.Text.Type = action.Text.Type
		if action.SelectedOption.Value != "" {
			ia.SelectedOption = &struct {
				Text  struct{ Text string } `json:"text"`
				Value string                `json:"value"`
			}{
				Text:  struct{ Text string }{Text: action.SelectedOption.Text.Text},
				Value: action.SelectedOption.Value,
			}
		}
		payload.Actions = append(payload.Actions, ia)
	}

	if c.onInteract != nil {
		c.onInteract(payload)
	}
}
