package mqtt

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// These tests exercise the connection layer against a real broker — the parts
// that don't need a database. They are skipped unless MQTT_LIVE_HOST is set:
//
//	MQTT_LIVE_HOST=192.168.80.28 MQTT_LIVE_USER=flomation MQTT_LIVE_PASS=... \
//	  go test ./internal/mqtt/ -run Live -v
//
// The claim under test in TestLiveDurableSessionSurvivesRestart is the one this
// whole design rests on: a durable session makes the broker queue QoS 1 messages
// published while Launch is disconnected and deliver them on reconnect. If that
// is not true, a Launch restart silently drops messages, and the trigger cannot
// be trusted with anything that matters.

func liveConfig(t *testing.T, topics string) Config {
	t.Helper()

	host := os.Getenv("MQTT_LIVE_HOST")
	if host == "" {
		t.Skip("MQTT_LIVE_HOST not set — skipping the live broker tests")
	}

	return Config{
		Protocol:   "mqtt",
		Host:       host,
		Port:       1883,
		Username:   os.Getenv("MQTT_LIVE_USER"),
		Password:   os.Getenv("MQTT_LIVE_PASS"),
		Topics:     topics,
		DefaultQoS: 1,
		Durable:    true,
	}
}

// collector records what a subscription receives.
type collector struct {
	mu   sync.Mutex
	msgs []string
}

func (c *collector) add(m paho.Message) {
	c.mu.Lock()
	c.msgs = append(c.msgs, string(m.Payload()))
	c.mu.Unlock()
}

func (c *collector) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.msgs))
	copy(out, c.msgs)
	return out
}

// subscribeLike builds a client exactly the way the service does — including
// subscribing from the OnConnect handler, which is what makes the subscription
// survive a reconnect.
//
// It waits for the SUBACK before returning. paho runs OnConnect on its own
// goroutine, so a caller that disconnects the moment Connect() acks can race the
// SUBSCRIBE and tear the client down before the broker has registered the
// subscription — leaving the durable session with nothing to queue against.
func subscribeLike(t *testing.T, cfg Config, triggerID, filter string, c *collector) paho.Client {
	t.Helper()

	opts, err := cfg.clientOptions(triggerID)
	if err != nil {
		t.Fatalf("clientOptions: %v", err)
	}

	subscribed := make(chan error, 1)
	opts.SetOnConnectHandler(func(cl paho.Client) {
		tok := cl.SubscribeMultiple(map[string]byte{filter: 1}, func(_ paho.Client, m paho.Message) {
			c.add(m)
		})
		if !tok.WaitTimeout(10 * time.Second) {
			subscribed <- fmt.Errorf("timed out subscribing to %s", filter)
			return
		}
		subscribed <- tok.Error()
	})

	client := paho.NewClient(opts)
	tok := client.Connect()
	if !tok.WaitTimeout(15*time.Second) || tok.Error() != nil {
		t.Fatalf("connect to %s: %v", cfg.brokerURL(), tok.Error())
	}

	select {
	case err := <-subscribed:
		if err != nil {
			t.Fatalf("subscribe to %s: %v", filter, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the broker never acknowledged the subscription to %s", filter)
	}

	return client
}

func publisher(t *testing.T, cfg Config, id string) paho.Client {
	t.Helper()

	opts := paho.NewClientOptions().
		AddBroker(cfg.brokerURL()).
		SetClientID(id).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetCleanSession(true)

	client := paho.NewClient(opts)
	tok := client.Connect()
	if !tok.WaitTimeout(15*time.Second) || tok.Error() != nil {
		t.Fatalf("publisher connect: %v", tok.Error())
	}
	return client
}

func TestLiveSubscribeAndReceive(t *testing.T) {
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	topic := "flomation/launchtest/" + stamp
	cfg := liveConfig(t, topic+"/#:1")

	c := &collector{}
	sub := subscribeLike(t, cfg, "11111111-2222-3333-4444-"+stamp[:12], topic+"/#", c)
	defer sub.Disconnect(250)

	pub := publisher(t, cfg, "flo-pub-"+stamp[:12])
	defer pub.Disconnect(250)

	pub.Publish(topic+"/live", 1, false, `{"temp":21.5}`).WaitTimeout(10 * time.Second)

	waitFor(t, c, 1, 5*time.Second)
	if got := c.seen(); got[0] != `{"temp":21.5}` {
		t.Errorf("payload = %q", got[0])
	}
}

// The restart claim. Disconnect the subscriber, publish while it is away, then
// reconnect under the same client ID and check the broker replays the backlog.
func TestLiveDurableSessionSurvivesRestart(t *testing.T) {
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	topic := "flomation/launchtest/" + stamp
	cfg := liveConfig(t, topic+"/#:1")
	triggerID := "22222222-3333-4444-5555-" + stamp[:12]

	// Establish the durable session, then take it down — a Launch restart.
	first := &collector{}
	sub := subscribeLike(t, cfg, triggerID, topic+"/#", first)
	sub.Disconnect(250)
	time.Sleep(time.Second)

	pub := publisher(t, cfg, "flo-pub-"+stamp[:12])
	defer pub.Disconnect(250)

	for i := 1; i <= 3; i++ {
		tok := pub.Publish(topic+"/offline", 1, false, fmt.Sprintf("while-away-%d", i))
		if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
			t.Fatalf("publish %d: %v", i, tok.Error())
		}
	}

	// Come back up under the same client ID.
	second := &collector{}
	sub2 := subscribeLike(t, cfg, triggerID, topic+"/#", second)
	defer sub2.Disconnect(250)

	waitFor(t, second, 3, 10*time.Second)

	got := second.seen()
	if len(got) != 3 {
		t.Fatalf("the broker replayed %d of 3 messages queued during the restart (%v) — a Launch restart WOULD lose messages", len(got), got)
	}
	t.Logf("broker replayed the full backlog on reconnect: %v", got)
}

// The control: without a durable session the same sequence must LOSE the
// message. Without this, a broker that retains regardless would make the test
// above pass for the wrong reason.
func TestLiveEphemeralSessionDropsMessagesSentWhileAway(t *testing.T) {
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	topic := "flomation/launchtest/" + stamp
	cfg := liveConfig(t, topic+"/#:1")
	cfg.Durable = false
	triggerID := "33333333-4444-5555-6666-" + stamp[:12]

	c := &collector{}
	sub := subscribeLike(t, cfg, triggerID, topic+"/#", c)
	sub.Disconnect(250)
	time.Sleep(time.Second)

	pub := publisher(t, cfg, "flo-pub-"+stamp[:12])
	defer pub.Disconnect(250)
	pub.Publish(topic+"/offline", 1, false, "lost").WaitTimeout(10 * time.Second)

	after := &collector{}
	sub2 := subscribeLike(t, cfg, triggerID, topic+"/#", after)
	defer sub2.Disconnect(250)

	time.Sleep(3 * time.Second)
	if got := after.seen(); len(got) != 0 {
		t.Errorf("a non-durable session was replayed %v — the durability in the test above is not coming from our clean-session setting", got)
	}
}

func TestLiveWildcardSubscription(t *testing.T) {
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	topic := "flomation/launchtest/" + stamp
	cfg := liveConfig(t, topic+"/#:1")

	c := &collector{}
	sub := subscribeLike(t, cfg, "44444444-5555-6666-7777-"+stamp[:12], topic+"/#", c)
	defer sub.Disconnect(250)

	pub := publisher(t, cfg, "flo-pub-"+stamp[:12])
	defer pub.Disconnect(250)
	pub.Publish(topic+"/deep/nested/leaf", 1, false, "wildcard-match").WaitTimeout(10 * time.Second)

	waitFor(t, c, 1, 5*time.Second)
}

// A bad password must fail the connection rather than hanging or connecting
// anonymously.
func TestLiveRejectsBadCredentials(t *testing.T) {
	cfg := liveConfig(t, "x/#:1")
	if cfg.Username == "" {
		t.Skip("broker is anonymous — nothing to reject")
	}
	cfg.Password = "definitely-the-wrong-password"

	opts, err := cfg.clientOptions("55555555-6666-7777-8888-999999999999")
	if err != nil {
		t.Fatalf("clientOptions: %v", err)
	}
	// ConnectRetry would mask the rejection by retrying forever.
	opts.SetConnectRetry(false)
	opts.SetAutoReconnect(false)

	client := paho.NewClient(opts)
	tok := client.Connect()
	defer client.Disconnect(0)

	if !tok.WaitTimeout(15 * time.Second) {
		t.Fatal("the broker never answered a connection with a bad password")
	}
	if tok.Error() == nil {
		t.Fatal("the broker ACCEPTED a bad password")
	}
	t.Logf("bad password rejected: %v", tok.Error())
}

func waitFor(t *testing.T, c *collector, n int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(c.seen()) >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d message(s); got %v", n, c.seen())
}
