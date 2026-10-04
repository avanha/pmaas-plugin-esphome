package broker

import (
	"fmt"
	"log/slog"
	"net"
	"reflect"
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
