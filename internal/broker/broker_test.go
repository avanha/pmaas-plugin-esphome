package broker

import (
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

type message struct {
	device  string
	topic   string
	payload string
	retain  bool
}

// recorder collects everything the broker reports.
type recorder struct {
	messages     chan message
	connected    chan string
	disconnected chan string
	// lifecycle has "connected" and "disconnected" in the order they were reported.
	lifecycle chan string
}

func newRecorder() *recorder {
	return &recorder{
		messages:     make(chan message, 100),
		connected:    make(chan string, 100),
		disconnected: make(chan string, 100),
		lifecycle:    make(chan string, 1000),
	}
}

func (r *recorder) callbacks() Callbacks {
	return Callbacks{
		OnMessage: func(device string, topic string, payload []byte, retain bool) {
			r.messages <- message{device, topic, string(payload), retain}
		},
		OnDeviceConnected: func(device string) {
			r.connected <- device
			r.lifecycle <- "connected"
		},
		OnDeviceDisconnected: func(device string) {
			r.disconnected <- device
			r.lifecycle <- "disconnected"
		},
	}
}

func startBroker(t *testing.T) (*Broker, string, *recorder) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	rec := newRecorder()
	b, err := New(Config{
		Listener:        listener,
		Devices:         map[string]string{"garage1": "secret1", "garage2": "secret2"},
		DiscoveryPrefix: "homeassistant",
	}, rec.callbacks())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = b.Close() })

	return b, listener.Addr().String(), rec
}

func connect(addr string, clientID string, username string, password string) (paho.Client, error) {
	opts := paho.NewClientOptions().
		AddBroker("tcp://" + addr).
		SetClientID(clientID).
		SetUsername(username).
		SetPassword(password).
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetConnectTimeout(3 * time.Second)

	client := paho.NewClient(opts)
	token := client.Connect()

	if !token.WaitTimeout(5 * time.Second) {
		return nil, fmt.Errorf("connect timed out")
	}

	if err := token.Error(); err != nil {
		return nil, err
	}

	return client, nil
}

func mustConnect(t *testing.T, addr string, clientID string, username string, password string) paho.Client {
	t.Helper()

	client, err := connect(addr, clientID, username, password)
	if err != nil {
		t.Fatalf("connect as %s: %v", username, err)
	}

	t.Cleanup(func() { client.Disconnect(0) })

	return client
}

func publish(t *testing.T, client paho.Client, topic string, payload string, retain bool) {
	t.Helper()

	token := client.Publish(topic, 0, retain, payload)
	token.WaitTimeout(2 * time.Second)

	if err := token.Error(); err != nil {
		t.Fatal(err)
	}
}

func expectString(t *testing.T, ch chan string, want string) {
	t.Helper()

	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

func expectNoString(t *testing.T, ch chan string) {
	t.Helper()

	select {
	case got := <-ch:
		t.Fatalf("unexpected %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBroker_RejectsBadCredentials(t *testing.T) {
	_, addr, _ := startBroker(t)

	for _, c := range []struct{ user, password string }{
		{"garage1", "wrong"},
		{"garage1", ""},
		{"nobody", "secret1"},
		{"", ""},
	} {
		if client, err := connect(addr, "c", c.user, c.password); err == nil {
			client.Disconnect(0)
			t.Errorf("expected connect as %q/%q to be rejected", c.user, c.password)
		}
	}
}

func TestBroker_ReportsConnectAndDisconnect(t *testing.T) {
	_, addr, rec := startBroker(t)

	client := mustConnect(t, addr, "c1", "garage1", "secret1")
	expectString(t, rec.connected, "garage1")

	client.Disconnect(100)
	expectString(t, rec.disconnected, "garage1")
}

// A device that reconnects before its old connection is noticed to be dead has its old connection
// taken over, and the broker may wind that up in either order. Whatever the order, a connected
// device must never be left looking offline, which means the old connection's "disconnected" must
// be delivered before the new connection's "connected" if it's delivered at all. This forces the
// bad interleaving: the disconnect callback is held up mid-delivery while the new session starts.
func TestHook_DisconnectOfReplacedSessionIsDeliveredBeforeNewConnect(t *testing.T) {
	var (
		mu       sync.Mutex
		sequence []string
	)

	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()

		sequence = append(sequence, event)
	}

	inDisconnectCallback := make(chan struct{})
	releaseDisconnectCallback := make(chan struct{})

	h := &hook{
		devices:  map[string]string{"garage1": "secret1"},
		sessions: map[string]*session{"garage1": {}},
		logger:   slog.New(slog.DiscardHandler),
		callbacks: Callbacks{
			OnDeviceConnected: func(string) { record("connected") },
			OnDeviceDisconnected: func(string) {
				close(inDisconnectCallback)
				<-releaseDisconnectCallback
				record("disconnected")
			},
		},
	}

	server := mqtt.New(&mqtt.Options{})
	newClient := func(id string) *mqtt.Client {
		cl := server.NewClient(nil, "test", id, false)
		cl.Properties.Username = []byte("garage1")

		return cl
	}

	oldClient, newerClient := newClient("old"), newClient("new")
	h.OnSessionEstablished(oldClient, packets.Packet{})
	sequence = nil // forget the initial connect

	var wg sync.WaitGroup

	wg.Go(func() { h.OnDisconnect(oldClient, nil, false) })
	<-inDisconnectCallback

	wg.Go(func() { h.OnSessionEstablished(newerClient, packets.Packet{}) })

	// Give the new session every chance to overtake the held-up disconnect.
	time.Sleep(200 * time.Millisecond)
	close(releaseDisconnectCallback)
	wg.Wait()

	if want := []string{"disconnected", "connected"}; !reflect.DeepEqual(sequence, want) {
		t.Fatalf("delivered %v, want %v", sequence, want)
	}
}

func TestHook_DisconnectOfAReplacedSessionIsNotReported(t *testing.T) {
	var disconnects int

	h := &hook{
		devices:   map[string]string{"garage1": "secret1"},
		sessions:  map[string]*session{"garage1": {}},
		logger:    slog.New(slog.DiscardHandler),
		callbacks: Callbacks{OnDeviceDisconnected: func(string) { disconnects++ }},
	}

	server := mqtt.New(&mqtt.Options{})
	newClient := func(id string) *mqtt.Client {
		cl := server.NewClient(nil, "test", id, false)
		cl.Properties.Username = []byte("garage1")

		return cl
	}

	oldClient, newerClient := newClient("old"), newClient("new")
	h.OnSessionEstablished(oldClient, packets.Packet{})
	h.OnSessionEstablished(newerClient, packets.Packet{})

	h.OnDisconnect(oldClient, nil, false)
	if disconnects != 0 {
		t.Fatal("the replaced connection ending was reported")
	}

	h.OnDisconnect(newerClient, nil, false)
	if disconnects != 1 {
		t.Fatalf("expected the current connection ending to be reported once, got %d", disconnects)
	}
}

func TestBroker_DeliversAllowedMessagesAndDropsDeniedOnes(t *testing.T) {
	_, addr, rec := startBroker(t)
	client := mustConnect(t, addr, "c1", "garage1", "secret1")

	publish(t, client, "garage2/sensor/temp/state", "stolen", false)
	publish(t, client, "homeassistant/sensor/garage2/temp/config", "{}", true)
	publish(t, client, "garage1/sensor/temp/state", "21.5", true)
	publish(t, client, "homeassistant/sensor/garage1/temp/config", "{}", true)

	var got []message

	for len(got) < 2 {
		select {
		case m := <-rec.messages:
			got = append(got, m)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out; received %+v", got)
		}
	}

	want := []message{
		{"garage1", "garage1/sensor/temp/state", "21.5", true},
		{"garage1", "homeassistant/sensor/garage1/temp/config", "{}", true},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d: got %+v, want %+v", i, got[i], want[i])
		}
	}

	select {
	case m := <-rec.messages:
		t.Fatalf("unexpected extra message %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestBroker_PublishReachesSubscribedDevice(t *testing.T) {
	b, addr, _ := startBroker(t)
	client := mustConnect(t, addr, "c1", "garage1", "secret1")

	received := make(chan string, 1)
	token := client.Subscribe("garage1/switch/fan/command", 0, func(_ paho.Client, m paho.Message) {
		received <- string(m.Payload())
	})
	token.WaitTimeout(2 * time.Second)

	if err := token.Error(); err != nil {
		t.Fatal(err)
	}

	if err := b.Publish("garage1/switch/fan/command", []byte("ON"), false); err != nil {
		t.Fatal(err)
	}

	expectString(t, received, "ON")
}

func TestBroker_DeviceCannotSubscribeOutsideItsOwnTree(t *testing.T) {
	_, addr, _ := startBroker(t)
	client := mustConnect(t, addr, "c1", "garage1", "secret1")

	for filter, wantAllowed := range map[string]bool{
		"garage1/+/+/command": true,
		"garage2/#":           false,
		"#":                   false,
		"homeassistant/#":     false,
	} {
		token := client.Subscribe(filter, 0, nil)
		token.WaitTimeout(2 * time.Second)

		granted, ok := token.(*paho.SubscribeToken)
		if !ok {
			t.Fatalf("unexpected token type %T", token)
		}

		code, found := granted.Result()[filter]
		if !found {
			t.Fatalf("no result for %q: %v", filter, granted.Result())
		}

		if allowed := code != 0x80; allowed != wantAllowed {
			t.Errorf("subscribe %q: allowed=%v (code 0x%x), want %v", filter, allowed, code, wantAllowed)
		}
	}
}

func TestNew_ValidatesConfig(t *testing.T) {
	newListener := func() net.Listener {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = l.Close() })

		return l
	}

	for name, cfg := range map[string]Config{
		"no listener":     {Devices: map[string]string{"a": "b"}},
		"empty password":  {Listener: newListener(), Devices: map[string]string{"a": ""}},
		"bad device name": {Listener: newListener(), Devices: map[string]string{"a/b": "x"}},
	} {
		if b, err := New(cfg, Callbacks{}); err == nil {
			_ = b.Close()
			t.Errorf("%s: expected an error", name)
		}
	}
}

// syncBuffer is a log destination the broker's goroutines and the test can both use.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func startLoggingBroker(t *testing.T) (string, *syncBuffer) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	b, err := New(Config{
		Listener:        listener,
		Devices:         map[string]string{"garage1": "secret1", "garage2": "secret2"},
		DiscoveryPrefix: "homeassistant",
		Logger:          slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}, Callbacks{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = b.Close() })

	return listener.Addr().String(), logs
}

func waitForLog(t *testing.T, logs *syncBuffer, want ...string) string {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)

	for {
		out := logs.String()
		missing := ""

		for _, w := range want {
			if !strings.Contains(out, w) {
				missing = w
				break
			}
		}

		if missing == "" {
			return out
		}

		if time.Now().After(deadline) {
			t.Fatalf("log is missing %q:\n%s", missing, out)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// A device that's refused something just keeps reconnecting, and says nothing useful about why, so what
// the broker refuses has to be logged where the operator can see it.
func TestBroker_LogsADeniedPublishWithItsTopic(t *testing.T) {
	addr, logs := startLoggingBroker(t)
	client := mustConnect(t, addr, "c1", "garage1", "secret1")

	publish(t, client, "homeassistant/sensor/somebody-else/temp/config", "{}", false) // QoS 0: dropped quietly

	waitForLog(t, logs, "denying publish", `device=garage1`, `topic=homeassistant/sensor/somebody-else/temp/config`)

	if !client.IsConnected() {
		t.Error("a denied QoS 0 publish must not disconnect the device")
	}
}

// This is the failure that looks like a flapping connection: the device's QoS 1 publish is refused, so
// it's disconnected, reconnects, sends it again, and so on.
func TestBroker_ADeniedQoS1PublishDisconnectsTheDeviceAndIsLogged(t *testing.T) {
	addr, logs := startLoggingBroker(t)

	lost := make(chan struct{}, 1)
	opts := paho.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("c1").
		SetUsername("garage1").SetPassword("secret1").SetAutoReconnect(false).
		SetConnectionLostHandler(func(paho.Client, error) { lost <- struct{}{} })

	client := paho.NewClient(opts)
	if token := client.Connect(); !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("connect: %v", token.Error())
	}

	client.Publish("garage2/status", 1, false, "online").WaitTimeout(time.Second)

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the device to be disconnected")
	}

	waitForLog(t, logs, "denying publish", "MQTT 3.1.1 client that sent it at QoS 1 or 2 is disconnected", `topic=garage2/status`)
}

func TestBroker_LogsADeniedSubscription(t *testing.T) {
	addr, logs := startLoggingBroker(t)
	client := mustConnect(t, addr, "c1", "garage1", "secret1")

	client.Subscribe("homeassistant/status", 0, nil).WaitTimeout(2 * time.Second)

	waitForLog(t, logs, "denying subscription", `device=garage1`, `filter=homeassistant/status`)
}

func TestBroker_LogsAFailedLogin(t *testing.T) {
	addr, logs := startLoggingBroker(t)

	if client, err := connect(addr, "c1", "garage1", "wrong"); err == nil {
		client.Disconnect(0)
		t.Fatal("expected the connection to be refused")
	}

	waitForLog(t, logs, "rejecting device connection", `device=garage1`)

	if strings.Contains(logs.String(), "wrong") || strings.Contains(logs.String(), "secret1") {
		t.Errorf("a password reached the log:\n%s", logs.String())
	}
}

// ESPHome's node discovery is on by default and is what a freshly flashed device does on every connect. The
// broker takes it in and throws it away (see below), so what's refused is anything else in ESPHome's namespace,
// and that refusal says what the broker tolerates.
func TestBroker_ExplainsWhatItToleratesInESPHomesNamespace(t *testing.T) {
	addr, logs := startLoggingBroker(t)
	client := mustConnect(t, addr, "c1", "garage1", "secret1")

	client.Subscribe("esphome/#", 0, nil).WaitTimeout(2 * time.Second)
	publish(t, client, "esphome/other", "{}", false)

	out := waitForLog(t, logs, "denying subscription", "filter=esphome/#", "denying publish", "topic=esphome/other")

	if got := strings.Count(out, "discover_ip: false"); got != 2 {
		t.Errorf("expected the hint on both refusals, found it %d times in:\n%s", got, out)
	}
}

func TestBroker_DoesNotGiveTheESPHomeHintForOtherTopics(t *testing.T) {
	addr, logs := startLoggingBroker(t)
	client := mustConnect(t, addr, "c1", "garage1", "secret1")

	publish(t, client, "garage2/status", "online", false)

	if out := waitForLog(t, logs, "denying publish", "topic=garage2/status"); strings.Contains(out, "discover_ip") {
		t.Errorf("an unrelated refusal got the ESPHome hint:\n%s", out)
	}
}

// A device left at ESPHome's defaults does its node discovery on every connect, including a QoS 1 publish
// that, if refused, gets it disconnected. So it's accepted, and thrown away.
func TestBroker_AcknowledgesAndThrowsAwayESPHomeNodeDiscovery(t *testing.T) {
	_, addr, rec := startBroker(t)

	lost := make(chan struct{}, 1)
	opts := paho.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("c1").
		SetUsername("garage1").SetPassword("secret1").SetAutoReconnect(false).
		SetConnectionLostHandler(func(paho.Client, error) { lost <- struct{}{} })

	device := paho.NewClient(opts)
	if token := device.Connect(); !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("connect: %v", token.Error())
	}

	t.Cleanup(func() { device.Disconnect(0) })

	// Another device, listening for anything the first one's discovery might leak to it.
	other := mustConnect(t, addr, "c2", "garage2", "secret2")
	leaked := make(chan string, 10)

	for _, filter := range []string{"esphome/discover", "esphome/discover/#"} {
		token := other.Subscribe(filter, 1, func(_ paho.Client, m paho.Message) { leaked <- m.Topic() })
		token.WaitTimeout(2 * time.Second)

		if err := token.Error(); err != nil {
			t.Fatal(err)
		}
	}

	// What the device does on connect: subscribe, and publish at QoS 1, once retained.
	for _, filter := range []string{"esphome/discover", "esphome/discover/#", "esphome/ping/garage1"} {
		token := device.Subscribe(filter, 0, nil)
		token.WaitTimeout(2 * time.Second)

		if code := token.(*paho.SubscribeToken).Result()[filter]; token.Error() != nil || code == 0x80 {
			t.Errorf("subscribe %q was refused (code 0x%x, err %v)", filter, code, token.Error())
		}
	}

	for _, c := range []struct {
		topic  string
		retain bool
	}{
		{"esphome/discover", false},
		{"esphome/discover/garage1", true},
	} {
		token := device.Publish(c.topic, 1, c.retain, `{"name":"garage1"}`)

		// The acknowledgement is what the device waits for before it considers the message sent.
		if !token.WaitTimeout(3*time.Second) || token.Error() != nil {
			t.Fatalf("publish to %q wasn't acknowledged: %v", c.topic, token.Error())
		}
	}

	select {
	case <-lost:
		t.Fatal("the device was disconnected")
	case <-time.After(300 * time.Millisecond):
	}

	select {
	case topic := <-leaked:
		t.Fatalf("a message published to the node discovery topics reached another device: %q", topic)
	default:
	}

	// Nothing was retained either: a device subscribing now isn't handed it.
	late := mustConnect(t, addr, "c3", "garage2", "secret2")
	late.Subscribe("esphome/discover/#", 0, func(_ paho.Client, m paho.Message) { leaked <- m.Topic() }).WaitTimeout(2 * time.Second)

	select {
	case topic := <-leaked:
		t.Fatalf("a retained node discovery message was kept and delivered: %q", topic)
	case <-time.After(300 * time.Millisecond):
	}

	// The messages are still reported, so that capturing messages shows what a device sends.
	var reported []string

	for len(reported) < 2 {
		select {
		case m := <-rec.messages:
			reported = append(reported, m.topic)
		case <-time.After(2 * time.Second):
			t.Fatalf("expected both messages to be reported, got %v", reported)
		}
	}
}

func TestBroker_StillRefusesOtherDevicesNodeDiscoveryTopics(t *testing.T) {
	addr, logs := startLoggingBroker(t)

	lost := make(chan struct{}, 1)
	opts := paho.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("c1").
		SetUsername("garage1").SetPassword("secret1").SetAutoReconnect(false).
		SetConnectionLostHandler(func(paho.Client, error) { lost <- struct{}{} })

	device := paho.NewClient(opts)
	if token := device.Connect(); !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("connect: %v", token.Error())
	}

	// Pretending to be another device's answer is exactly what the ACL is there to stop.
	device.Publish("esphome/discover/garage2", 1, false, "{}").WaitTimeout(time.Second)

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the device to be disconnected")
	}

	waitForLog(t, logs, "denying publish", "topic=esphome/discover/garage2")
}

func TestBroker_SubscriptionsToNodeDiscoveryAreGrantedOnlyForTheDevicesOwnTopics(t *testing.T) {
	addr, _ := startLoggingBroker(t)
	client := mustConnect(t, addr, "c1", "garage1", "secret1")

	for filter, wantGranted := range map[string]bool{
		"esphome/discover":     true,
		"esphome/discover/#":   true,
		"esphome/ping/garage1": true,
		"esphome/ping/garage2": false,
		"esphome/discover/+":   false,
		"esphome/#":            false,
	} {
		token := client.Subscribe(filter, 0, nil)
		token.WaitTimeout(2 * time.Second)

		granted := token.(*paho.SubscribeToken).Result()[filter] != 0x80

		if granted != wantGranted {
			t.Errorf("subscribe %q: granted=%v, want %v", filter, granted, wantGranted)
		}
	}
}
