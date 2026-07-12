package mqtt

import (
	"strings"
	"testing"
	"time"
)

func TestParseTopicList(t *testing.T) {
	topics, err := parseTopicList("sensors/#:1, alerts:2 ,plain", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]byte{"sensors/#": 1, "alerts": 2, "plain": 0}
	if len(topics) != len(want) {
		t.Fatalf("got %v, want %v", topics, want)
	}
	for topic, qos := range want {
		if topics[topic] != qos {
			t.Errorf("%s: qos = %d, want %d", topic, topics[topic], qos)
		}
	}
}

func TestParseTopicListAppliesDefaultQoS(t *testing.T) {
	topics, err := parseTopicList("a,b:2", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topics["a"] != 1 {
		t.Errorf("a: qos = %d, want the default 1", topics["a"])
	}
	if topics["b"] != 2 {
		t.Errorf("b: qos = %d, want its own 2", topics["b"])
	}
}

// A colon is legal inside a topic name, so only a trailing :0-2 is a QoS suffix.
func TestParseTopicListOnlyTreatsTrailingDigitAsQoS(t *testing.T) {
	topics, err := parseTopicList("ns:sensors/temp", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := topics["ns:sensors/temp"]; !ok {
		t.Errorf("a colon inside the topic name was mistaken for a QoS suffix: %v", topics)
	}
}

func TestParseTopicListRejectsEmpty(t *testing.T) {
	if _, err := parseTopicList(" , ,", 0); err == nil {
		t.Error("expected an error for a list with no topics")
	}
}

// The client ID is the identity the broker keys the durable session off, so it
// must be stable for a given trigger and short enough for brokers that still
// enforce the 23-byte MQTT 3.1 limit.
func TestClientIDIsStableAndShort(t *testing.T) {
	id := "0f8fad5b-d9cb-469f-a165-70867728950e"

	first := clientID(id)
	if first != clientID(id) {
		t.Error("client ID is not stable across calls — the durable session would be orphaned on every reconnect")
	}
	if len(first) > 23 {
		t.Errorf("client ID %q is %d bytes, over the 23-byte limit some brokers enforce", first, len(first))
	}
	if !strings.HasPrefix(first, "flo-") {
		t.Errorf("client ID %q is not identifiable in broker logs", first)
	}
	if clientID(id) == clientID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Error("two triggers produced the same client ID — they would evict each other from the broker")
	}
}

func TestBrokerURLCarriesPathOnlyForWebSockets(t *testing.T) {
	tcp := Config{Protocol: "mqtt", Host: "h", Port: 1883, WSPath: "/mqtt"}
	if got := tcp.brokerURL(); got != "mqtt://h:1883" {
		t.Errorf("tcp brokerURL() = %q, want no path", got)
	}

	ws := Config{Protocol: "ws", Host: "h", Port: 8083, WSPath: "/mqtt"}
	if got := ws.brokerURL(); got != "ws://h:8083/mqtt" {
		t.Errorf("ws brokerURL() = %q, want the endpoint path", got)
	}
}

// An edit to the node has to force a re-dial, including a rotated password.
func TestFingerprintChangesWithEveryField(t *testing.T) {
	base := Config{
		Protocol: "mqtt", Host: "h", Port: 1883, Username: "u", Password: "p",
		Topics: "a", DefaultQoS: 0, ParseJSON: false, Durable: true,
	}

	mutations := map[string]Config{}

	m := base
	m.Password = "rotated"
	mutations["password"] = m

	m = base
	m.Topics = "a,b"
	mutations["topics"] = m

	m = base
	m.Host = "other"
	mutations["host"] = m

	m = base
	m.DefaultQoS = 1
	mutations["default_qos"] = m

	m = base
	m.Durable = false
	mutations["durable"] = m

	m = base
	m.ParseJSON = true
	mutations["parse_json"] = m

	for name, mutated := range mutations {
		if mutated.fingerprint() == base.fingerprint() {
			t.Errorf("changing %s did not change the fingerprint — the connection would not be rebuilt", name)
		}
	}

	// Determinism matters as much as sensitivity: an unchanged config has to produce
	// the same fingerprint every time, or the reconcile loop would tear the
	// connection down and re-dial it on every single tick.
	//
	// Compared via a copy rather than calling base.fingerprint() twice inline —
	// staticcheck (SA4000) rightly objects to identical expressions either side of a
	// comparison, and this states what is actually being asserted anyway.
	unchanged := base
	if base.fingerprint() != unchanged.fingerprint() {
		t.Error("fingerprint is not stable for an unchanged config — the connection would be rebuilt every tick")
	}
}

// Values reach the trigger as whatever the editor stored: a port may be a JSON
// number or a string, a checkbox a bool or "true".
func TestValueCoercion(t *testing.T) {
	if v, ok := asInt(float64(8883)); !ok || v != 8883 {
		t.Errorf("JSON number: %d %v", v, ok)
	}
	if v, ok := asInt("8883"); !ok || v != 8883 {
		t.Errorf("string: %d %v", v, ok)
	}
	if _, ok := asInt(""); ok {
		t.Error("empty string reported as a set integer")
	}
	if _, ok := asInt(nil); ok {
		t.Error("nil reported as a set integer")
	}
	if _, ok := asInt("not a number"); ok {
		t.Error("junk reported as a set integer")
	}

	if !asBool(true, false) {
		t.Error("native bool not read")
	}
	if !asBool("true", false) {
		t.Error(`"true" not read`)
	}
	if asBool("false", true) {
		t.Error(`"false" not read`)
	}
	// The default matters: "durable" defaults ON, and an untouched checkbox must
	// not silently turn the durable session off.
	if !asBool(nil, true) {
		t.Error("nil did not fall back to the default")
	}
	if !asBool("", true) {
		t.Error("empty string did not fall back to the default")
	}
}

func TestStripSchemeToleratesAPastedURL(t *testing.T) {
	for _, in := range []string{
		"mqtt://broker.example.com/",
		"broker.example.com",
		"ws://broker.example.com/mqtt",
		"ssl://broker.example.com",
	} {
		if got := stripScheme(in); got != "broker.example.com" {
			t.Errorf("stripScheme(%q) = %q", in, got)
		}
	}
}

func TestRedactHidesThePassword(t *testing.T) {
	c := Config{Password: "hunter2"}
	if got := redact(c, "bad user name or password hunter2"); strings.Contains(got, "hunter2") {
		t.Errorf("password survived redaction: %q", got)
	}
	if got := redact(Config{}, "connection refused"); got != "connection refused" {
		t.Errorf("empty password mangled the message: %q", got)
	}
}

func TestClientOptionsDurabilityAndTLS(t *testing.T) {
	durable := Config{Protocol: "mqtt", Host: "h", Port: 1883, Durable: true}
	opts, err := durable.clientOptions("t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.CleanSession {
		t.Error("durable subscription left clean session ON — the broker would drop queued messages on reconnect")
	}
	if !opts.AutoReconnect {
		t.Error("auto-reconnect is off — a dropped connection would never come back")
	}

	ephemeral := Config{Protocol: "mqtt", Host: "h", Port: 1883, Durable: false}
	opts, err = ephemeral.clientOptions("t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.CleanSession {
		t.Error("non-durable subscription left clean session OFF")
	}

	// TLS must verify unless the operator explicitly opted out.
	secure := Config{Protocol: "mqtts", Host: "broker.example.com", Port: 8883}
	opts, err = secure.clientOptions("t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.TLSConfig == nil {
		t.Fatal("mqtts produced no TLS config")
	}
	if opts.TLSConfig.InsecureSkipVerify {
		t.Error("TLS verification was off without allow_insecure being set")
	}

	insecure := Config{Protocol: "mqtts", Host: "h", Port: 8883, Insecure: true}
	opts, _ = insecure.clientOptions("t1")
	if !opts.TLSConfig.InsecureSkipVerify {
		t.Error("allow_insecure did not disable verification")
	}

	// A plain-TCP broker must never get a TLS config.
	plain := Config{Protocol: "mqtt", Host: "h", Port: 1883}
	opts, _ = plain.clientOptions("t1")
	if opts.TLSConfig != nil && opts.TLSConfig.ServerName != "" {
		t.Error("a TLS config was attached to a plain-TCP connection")
	}
}

func TestClientOptionsRejectsJunkCA(t *testing.T) {
	c := Config{Protocol: "mqtts", Host: "h", Port: 8883, CACert: "clearly not a certificate"}
	if _, err := c.clientOptions("t1"); err == nil {
		t.Error("a junk CA certificate was accepted")
	}
}

// A typo in the QoS suffix must clamp, not silently create a topic literally
// named "sensors/temp:3" that subscribes fine and never matches anything.
func TestParseTopicListClampsOutOfRangeQoS(t *testing.T) {
	topics, err := parseTopicList("sensors/temp:3,alerts:-1,ok:2", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, dead := topics["sensors/temp:3"]; dead {
		t.Error(`"sensors/temp:3" was taken as a topic NAME — the subscription would never fire`)
	}
	if q, ok := topics["sensors/temp"]; !ok || q != 0 {
		t.Errorf("sensors/temp: qos = %d, present = %v; want it clamped to 0", q, ok)
	}
	if q, ok := topics["alerts"]; !ok || q != 0 {
		t.Errorf("alerts: qos = %d, present = %v; want a negative QoS clamped to 0", q, ok)
	}
	if topics["ok"] != 2 {
		t.Errorf("a valid QoS was disturbed: %d", topics["ok"])
	}
}

// Brokers like AWS IoT Core refuse a connection whose client ID isn't the one
// their policy names, so a pinned ID has to win over the derived one.
func TestClientOptionsHonoursAPinnedClientID(t *testing.T) {
	c := Config{Protocol: "mqtt", Host: "h", Port: 1883, ClientID: "my-thing-name"}
	opts, err := c.clientOptions("some-trigger-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.ClientID != "my-thing-name" {
		t.Errorf("ClientID = %q, want the pinned my-thing-name", opts.ClientID)
	}

	derived := Config{Protocol: "mqtt", Host: "h", Port: 1883}
	opts, _ = derived.clientOptions("0f8fad5b-d9cb-469f-a165-70867728950e")
	if opts.ClientID != clientID("0f8fad5b-d9cb-469f-a165-70867728950e") {
		t.Errorf("ClientID = %q, want the derived one", opts.ClientID)
	}

	// Changing it must force a re-dial, or the operator's edit would never apply.
	a := Config{Protocol: "mqtt", Host: "h", Port: 1883, ClientID: "one"}
	b := a
	b.ClientID = "two"
	if a.fingerprint() == b.fingerprint() {
		t.Error("a changed client ID did not change the fingerprint")
	}
}

// A certificate pasted through a single-line field loses its newlines, which
// every PEM parser rejects with an error that says nothing about newlines.
func TestFormatPEMRepairsAFlattenedCertificate(t *testing.T) {
	flat := "-----BEGIN CERTIFICATE----- QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5ejAxMjM0NTY3ODk= -----END CERTIFICATE-----"

	got := formatPEM(flat)
	if !strings.HasPrefix(got, "-----BEGIN CERTIFICATE-----\n") {
		t.Errorf("no newline after the header:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n-----END CERTIFICATE-----") {
		t.Errorf("no newline before the footer:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 64 && !strings.HasPrefix(line, "-----") {
			t.Errorf("body line longer than 64 chars: %q", line)
		}
	}

	// An already-valid PEM must pass through untouched.
	valid := "-----BEGIN CERTIFICATE-----\nQUJDRA==\n-----END CERTIFICATE-----"
	if formatPEM(valid) != valid {
		t.Error("a valid PEM was mangled")
	}
	if formatPEM("") != "" {
		t.Error("empty input was not left alone")
	}
}

// The handover between instances must be strictly ordered: we have to be GONE
// before another instance can arrive, or the broker fans each message out to both
// of us and every message runs the flow twice. That means releasing on a safety
// margin, not on the true expiry.
func TestLeaseSafetyMarginCoversAReconcileInterval(t *testing.T) {
	if leaseSafetyMargin <= reconcileInterval {
		t.Errorf("leaseSafetyMargin (%s) must exceed one reconcile interval (%s), or a single missed tick could leave us subscribed past the point another instance can claim the lease",
			leaseSafetyMargin, reconcileInterval)
	}
	if leaseSafetyMargin >= leaseDuration {
		t.Errorf("leaseSafetyMargin (%s) must be well under leaseDuration (%s), or a healthy subscription would release itself immediately",
			leaseSafetyMargin, leaseDuration)
	}
}

func TestReleaseExpiringLeasesDropsOnlyTheAtRiskSubscriptions(t *testing.T) {
	s := &Service{subs: make(map[string]*subscription)}

	// Renewed a moment ago — safe.
	healthy := &subscription{triggerID: "healthy", leaseExpiry: time.Now().Add(leaseDuration)}
	// Renewal has been failing; the lease lapses inside the safety margin. Another
	// instance could take it imminently, so we must let go NOW rather than wait for
	// the row to actually expire.
	atRisk := &subscription{triggerID: "at-risk", leaseExpiry: time.Now().Add(leaseSafetyMargin / 2)}
	// Already lapsed.
	lapsed := &subscription{triggerID: "lapsed", leaseExpiry: time.Now().Add(-time.Minute)}

	s.subs["healthy"] = healthy
	s.subs["at-risk"] = atRisk
	s.subs["lapsed"] = lapsed

	s.releaseExpiringLeases()

	if _, held := s.subs["healthy"]; !held {
		t.Error("a freshly renewed lease was released")
	}
	if _, held := s.subs["at-risk"]; held {
		t.Error("a lease expiring inside the safety margin was kept — another instance could take it while we are still subscribed, double-firing the flow")
	}
	if _, held := s.subs["lapsed"]; held {
		t.Error("an already-lapsed lease was kept")
	}
}
