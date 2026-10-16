package nativeapi

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

// collector is a handler that keeps what it's given.
type collector struct {
	mu       sync.Mutex
	messages []proto.Message
	changed  chan struct{}
}

func newCollector() *collector { return &collector{changed: make(chan struct{}, 1)} }

func (c *collector) handle(m proto.Message) {
	c.mu.Lock()
	c.messages = append(c.messages, m)
	c.mu.Unlock()

	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *collector) all() []proto.Message {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]proto.Message(nil), c.messages...)
}

func (c *collector) waitFor(t *testing.T, what string, match func(proto.Message) bool) proto.Message {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		for _, m := range c.all() {
			if match(m) {
				return m
			}
		}

		select {
		case <-c.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s; got %v", what, c.all())
		}
	}
}

func isType[T proto.Message](m proto.Message) bool {
	_, ok := m.(T)
	return ok
}

// forBothFramings runs a test once against a plaintext device and once against an encrypted one.
func forBothFramings(t *testing.T, test func(t *testing.T, key string)) {
	t.Helper()

	t.Run("plaintext", func(t *testing.T) { test(t, "") })
	t.Run("encrypted", func(t *testing.T) { test(t, testKey) })
}

func dial(t *testing.T, cfg Config, handler Handler) *Conn {
	t.Helper()

	c, err := Dial(context.Background(), cfg, handler)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestDial_SaysHelloAndReportsWhatTheDeviceSaid(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		c := dial(t, device.config(), newCollector().handle)
		session := device.nextSession(t)

		hello := c.Hello()
		if hello.Name != "plug1" || hello.ServerInfo != "ESPHome 2026.9.1" || hello.ApiVersionMajor != 1 {
			t.Fatalf("unexpected hello %+v", hello)
		}

		sent := session.waitFor(t, "the client's hello", isType[*pb.HelloRequest]).(*pb.HelloRequest)
		if sent.ClientInfo != "PMAAS" || sent.ApiVersionMajor != clientAPIMajor || sent.ApiVersionMinor != clientAPIMinor {
			t.Fatalf("unexpected hello request %+v", sent)
		}
	})
}

func TestConn_EntitiesAndStatesAreDeliveredInOrder(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		got := newCollector()
		c := dial(t, device.config(), got.handle)

		if err := c.Send(&pb.ListEntitiesRequest{}); err != nil {
			t.Fatal(err)
		}

		got.waitFor(t, "the end of the entity list", isType[*pb.ListEntitiesDoneResponse])

		if err := c.Send(&pb.SubscribeStatesRequest{}); err != nil {
			t.Fatal(err)
		}

		got.waitFor(t, "the power reading", isType[*pb.SensorStateResponse])

		var order []string

		for _, m := range got.all() {
			switch m.(type) {
			case *pb.ListEntitiesSwitchResponse:
				order = append(order, "switch")
			case *pb.ListEntitiesSensorResponse:
				order = append(order, "sensor")
			case *pb.ListEntitiesDoneResponse:
				order = append(order, "done")
			case *pb.SwitchStateResponse:
				order = append(order, "switch state")
			case *pb.SensorStateResponse:
				order = append(order, "sensor state")
			default:
				t.Errorf("an unexpected message reached the handler: %T", m)
			}
		}

		want := []string{"switch", "sensor", "done", "switch state", "sensor state"}
		if len(order) != len(want) {
			t.Fatalf("got %v, want %v", order, want)
		}

		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("got %v, want %v", order, want)
			}
		}

		sensor := got.all()[1].(*pb.ListEntitiesSensorResponse)
		if sensor.ObjectId != "power" || sensor.DeviceClass != "power" || sensor.UnitOfMeasurement != "W" || sensor.Key != 2 {
			t.Fatalf("unexpected sensor %+v", sensor)
		}
	})
}

func TestConn_ACommandReachesTheDeviceAndItsAnswerComesBack(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		got := newCollector()
		c := dial(t, device.config(), got.handle)
		session := device.nextSession(t)

		if err := c.Send(&pb.SwitchCommandRequest{Key: 1, State: true}); err != nil {
			t.Fatal(err)
		}

		command := session.waitFor(t, "the command", isType[*pb.SwitchCommandRequest]).(*pb.SwitchCommandRequest)
		if command.Key != 1 || !command.State {
			t.Fatalf("unexpected command %+v", command)
		}

		state := got.waitFor(t, "the switch's state", isType[*pb.SwitchStateResponse]).(*pb.SwitchStateResponse)
		if state.Key != 1 || !state.State {
			t.Fatalf("unexpected state %+v", state)
		}
	})
}

// Newer firmware sends messages this client's proto doesn't have. They must be skipped, and everything after them
// still has to arrive.
func TestConn_AMessageItDoesNotKnowIsSkippedAndTheNextOneStillArrives(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		got := newCollector()

		var logged atomic.Int32

		cfg := device.config()
		cfg.Logf = func(string, ...any) { logged.Add(1) }

		_ = dial(t, cfg, got.handle)
		session := device.nextSession(t)

		session.sendRaw(60000, []byte{0x08, 0x01, 0x10, 0x02})
		session.sendRaw(59999, nil)
		session.send(&pb.SensorStateResponse{Key: 9, State: 3})

		state := got.waitFor(t, "the reading after the unknown messages", isType[*pb.SensorStateResponse]).(*pb.SensorStateResponse)
		if state.Key != 9 || len(got.all()) != 1 {
			t.Fatalf("got %+v, and %d messages", state, len(got.all()))
		}

		if logged.Load() != 2 {
			t.Errorf("expected the two skipped messages to be logged, got %d", logged.Load())
		}
	})
}

func TestConn_ThePingsAreAnsweredAndNotPassedOn(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		got := newCollector()
		_ = dial(t, device.config(), got.handle)
		session := device.nextSession(t)

		session.send(&pb.PingRequest{})
		session.waitFor(t, "the answer to the ping", isType[*pb.PingResponse])
		session.send(&pb.SensorStateResponse{Key: 1})

		got.waitFor(t, "a message after the ping", isType[*pb.SensorStateResponse])

		if len(got.all()) != 1 {
			t.Fatalf("housekeeping reached the handler: %v", got.all())
		}
	})
}

func TestConn_ThePingsItSendsKeepItAlive(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		cfg := device.config()
		cfg.KeepAlive = 20 * time.Millisecond // so it gives up after about 90ms of silence

		c := dial(t, cfg, newCollector().handle)
		session := device.nextSession(t)

		session.waitFor(t, "a ping from the client", isType[*pb.PingRequest])

		// Well past the time it would have given up, if the answers hadn't kept coming.
		time.Sleep(300 * time.Millisecond)

		select {
		case <-c.Done():
			t.Fatalf("the connection ended: %v", c.Err())
		default:
		}
	})
}

func TestConn_ADeviceThatStopsAnsweringIsGivenUpOn(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		device.silentAfterHello = true

		cfg := device.config()
		cfg.KeepAlive = 20 * time.Millisecond

		c := dial(t, cfg, newCollector().handle)

		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("a silent device was never given up on")
		}

		if !errors.Is(c.Err(), ErrKeepAliveTimeout) {
			t.Fatalf("got %v", c.Err())
		}
	})
}

func TestConn_ADeviceThatDisconnectsItselfIsReportedWithWhy(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		c := dial(t, device.config(), newCollector().handle)
		session := device.nextSession(t)

		session.send(&pb.DisconnectRequest{Reason: pb.DisconnectReason_DISCONNECT_REASON_PROVISIONING_CLOSED})

		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the connection didn't end")
		}

		var disconnected *DeviceDisconnectedError
		if !errors.As(c.Err(), &disconnected) || disconnected.Reason != pb.DisconnectReason_DISCONNECT_REASON_PROVISIONING_CLOSED {
			t.Fatalf("got %v", c.Err())
		}

		session.waitFor(t, "the answer to the disconnect", isType[*pb.DisconnectResponse])
	})
}

func TestConn_CloseTellsTheDeviceAndStopsTheHandler(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		got := newCollector()
		c, err := Dial(context.Background(), device.config(), got.handle)
		if err != nil {
			t.Fatal(err)
		}

		session := device.nextSession(t)

		if err := c.Close(); err != nil {
			t.Fatal(err)
		}

		session.waitFor(t, "the disconnect request", isType[*pb.DisconnectRequest])

		select {
		case <-c.Done():
		default:
			t.Fatal("Close returned before the connection was done")
		}

		if !errors.Is(c.Err(), ErrClosed) {
			t.Fatalf("got %v", c.Err())
		}

		// Nothing is delivered once it's closed, and closing again is harmless.
		before := len(got.all())
		session.send(&pb.SensorStateResponse{Key: 1})
		time.Sleep(50 * time.Millisecond)

		if len(got.all()) != before {
			t.Fatal("the handler was called after Close")
		}

		if err := c.Close(); err != nil {
			t.Fatal(err)
		}

		if err := c.Send(&pb.PingRequest{}); err == nil {
			t.Error("expected sending on a closed connection to fail")
		}
	})
}

func TestConn_ErrIsNilWhileTheConnectionIsUp(t *testing.T) {
	device := newFakeDevice(t, "")
	c := dial(t, device.config(), newCollector().handle)

	if c.Err() != nil {
		t.Fatalf("got %v", c.Err())
	}
}

func TestConn_TheDeviceIsAskedTheTimeAndAnswers(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		cfg := device.config()
		cfg.Timezone = "MST7MDT,M3.2.0,M11.1.0"

		got := newCollector()
		_ = dial(t, cfg, got.handle)
		session := device.nextSession(t)

		before := uint32(time.Now().Unix())
		session.send(&pb.GetTimeRequest{})

		answer := session.waitFor(t, "the time", isType[*pb.GetTimeResponse]).(*pb.GetTimeResponse)
		if answer.EpochSeconds < before || answer.EpochSeconds > before+5 || answer.Timezone != cfg.Timezone {
			t.Fatalf("unexpected answer %+v", answer)
		}

		if len(got.all()) != 0 {
			t.Fatalf("housekeeping reached the handler: %v", got.all())
		}
	})
}

func TestConn_ManyGoroutinesCanSendAtOnce(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		c := dial(t, device.config(), newCollector().handle)
		session := device.nextSession(t)

		const senders = 40

		var wg sync.WaitGroup

		for i := 0; i < senders; i++ {
			wg.Add(1)

			go func(key uint32) {
				defer wg.Done()

				if err := c.Send(&pb.SwitchCommandRequest{Key: key, State: true}); err != nil {
					t.Errorf("send %d: %v", key, err)
				}
			}(uint32(i))
		}

		wg.Wait()

		seen := map[uint32]bool{}

		session.waitFor(t, "every command", func(proto.Message) bool {
			for _, m := range session.messages() {
				if command, ok := m.(*pb.SwitchCommandRequest); ok {
					seen[command.Key] = true
				}
			}

			return len(seen) == senders
		})
	})
}

func TestDial_RefusesADeviceThatIsNotTheOneExpected(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		cfg := device.config()
		cfg.Name = "someone-else"

		_, err := Dial(context.Background(), cfg, newCollector().handle)

		var wrong *WrongDeviceError
		if !errors.As(err, &wrong) || wrong.Expected != "someone-else" || wrong.Got != "plug1" {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDial_NoExpectedNameAcceptsAnyDevice(t *testing.T) {
	device := newFakeDevice(t, "")
	cfg := device.config()
	cfg.Name = ""

	dial(t, cfg, newCollector().handle)
}

func TestDial_RefusesADeviceWithAnAPIThisIsNotWrittenFor(t *testing.T) {
	device := newFakeDevice(t, "")
	device.apiMajor = 3

	_, err := Dial(context.Background(), device.config(), newCollector().handle)
	if !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestDial_TheWrongKeyIsSaidToBeTheWrongKey(t *testing.T) {
	device := newFakeDevice(t, testKey)
	cfg := device.config()
	cfg.Key = otherKey

	_, err := Dial(context.Background(), cfg, newCollector().handle)
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v", err)
	}
}

func TestDial_AHandshakeTheDeviceRefusesForAnotherReasonSaysWhy(t *testing.T) {
	device := newFakeDevice(t, testKey)
	device.refuseHandshake = "Handshake failure\x00\x1b[31m: out of memory"

	_, err := Dial(context.Background(), device.config(), newCollector().handle)
	if !errors.Is(err, ErrHandshake) || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v", err)
	}

	// What the device says is text from the network: whatever it puts in it mustn't reach a log as it is.
	if got := err.Error(); containsControl(got) {
		t.Fatalf("control characters in %q", got)
	}
}

func containsControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}

	return false
}

func TestDial_AKeyForADeviceThatIsNotEncryptedSaysSo(t *testing.T) {
	device := newFakeDevice(t, "")
	cfg := device.config()
	cfg.Key = testKey

	_, err := Dial(context.Background(), cfg, newCollector().handle)
	if !errors.Is(err, ErrDeviceNotEncrypted) {
		t.Fatalf("got %v", err)
	}
}

func TestDial_NoKeyForAnEncryptedDeviceSaysSo(t *testing.T) {
	device := newFakeDevice(t, testKey)
	cfg := device.config()
	cfg.Key = ""

	_, err := Dial(context.Background(), cfg, newCollector().handle)
	if !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("got %v", err)
	}
}

func TestDial_AMalformedKeyIsRefusedBeforeConnectingToAnything(t *testing.T) {
	for name, key := range map[string]string{
		"not base64":     "this is not base64!",
		"too short":      "AAAA",
		"31 bytes":       "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTI=",
		"a password-ish": "hunter2",
	} {
		var dialed atomic.Bool

		cfg := Config{Address: "127.0.0.1:1", Key: key, dial: func(context.Context, string) (net.Conn, error) {
			dialed.Store(true)

			c1, c2 := net.Pipe()
			t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

			return c1, nil
		}}

		if _, err := Dial(context.Background(), cfg, newCollector().handle); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDial_ADeviceThatNeverAnswersIsGivenUpOnAtTheSetupTimeout(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		device.neverAnswerHello = true

		cfg := device.config()
		cfg.SetupTimeout = 200 * time.Millisecond

		start := time.Now()

		_, err := Dial(context.Background(), cfg, newCollector().handle)
		if err == nil {
			t.Fatal("expected an error")
		}

		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("took %v", elapsed)
		}
	})
}

func TestDial_CancellingAbandonsASetupThatIsWaiting(t *testing.T) {
	device := newFakeDevice(t, "")
	device.neverAnswerHello = true

	cfg := device.config()
	cfg.SetupTimeout = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)

	go func() {
		_, err := Dial(ctx, cfg, newCollector().handle)
		result <- err
	}()

	device.nextSession(t)
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Dial didn't return after its context was cancelled")
	}
}

func TestDial_NothingListeningIsAnError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().String()
	_ = listener.Close()

	if _, err := Dial(context.Background(), Config{Address: address}, newCollector().handle); err == nil {
		t.Fatal("expected an error")
	}
}

func TestConn_AFrameThatClaimsToBeHugeEndsTheConnectionRatherThanAllocating(t *testing.T) {
	device := newFakeDevice(t, "")
	c := dial(t, device.config(), newCollector().handle)
	session := device.nextSession(t)

	// 0x00, a length of about 4GB as a varint, then a type.
	_, _ = session.conn.Write([]byte{0x00, 0xff, 0xff, 0xff, 0xff, 0x0f, 0x01})

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection didn't end")
	}

	if !errors.Is(c.Err(), ErrProtocol) {
		t.Fatalf("got %v", c.Err())
	}
}

func TestConn_AFrameThatIsNotAFrameEndsTheConnection(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		c := dial(t, device.config(), newCollector().handle)
		session := device.nextSession(t)

		// Neither framing starts a frame with this.
		_, _ = session.conn.Write([]byte{0x7f, 0x00, 0x00})

		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the connection didn't end")
		}

		if !errors.Is(c.Err(), ErrProtocol) {
			t.Fatalf("got %v", c.Err())
		}
	})
}

func TestConn_AMessageThatDoesNotParseEndsTheConnection(t *testing.T) {
	device := newFakeDevice(t, "")
	c := dial(t, device.config(), newCollector().handle)
	session := device.nextSession(t)

	// A sensor state whose payload is cut off in the middle of a field.
	id, _ := messageID(&pb.SensorStateResponse{})
	session.sendRaw(id, []byte{0x0d, 0x01})

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection didn't end")
	}

	if !errors.Is(c.Err(), ErrProtocol) {
		t.Fatalf("got %v", c.Err())
	}
}

func TestConn_ConnectionLossIsReported(t *testing.T) {
	device := newFakeDevice(t, "")
	c := dial(t, device.config(), newCollector().handle)
	session := device.nextSession(t)

	_ = session.conn.Close()

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection didn't end")
	}

	if c.Err() == nil || errors.Is(c.Err(), ErrClosed) {
		t.Fatalf("a connection lost is not a connection closed: %v", c.Err())
	}
}

// Close is what lets a caller know the handler is finished with whatever it shares with the caller, so it has to
// wait for a handler that's running.
func TestConn_CloseWaitsForAHandlerThatIsRunning(t *testing.T) {
	device := newFakeDevice(t, "")

	entered := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	c, err := Dial(context.Background(), device.config(), func(proto.Message) {
		once.Do(func() { close(entered) })
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}

	session := device.nextSession(t)
	session.send(&pb.SensorStateResponse{Key: 1})
	<-entered

	closed := make(chan struct{})

	go func() {
		_ = c.Close()
		close(closed)
	}()

	select {
	case <-closed:
		t.Fatal("Close returned while the handler was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned")
	}
}

// Found on a real device: a Kauf plug asks for the time before it says hello, so what arrives before the hello
// response can't be treated as a protocol error.
func TestDial_ADeviceThatAsksForTheTimeBeforeItSaysHelloIsAnswered(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		device.askTimeFirst = true

		cfg := device.config()
		cfg.Timezone = "MST7MDT,M3.2.0,M11.1.0"

		got := newCollector()
		c := dial(t, cfg, got.handle)
		session := device.nextSession(t)

		if c.Hello().Name != "plug1" {
			t.Fatalf("unexpected hello %+v", c.Hello())
		}

		answer := session.waitFor(t, "the time", isType[*pb.GetTimeResponse]).(*pb.GetTimeResponse)
		if answer.Timezone != cfg.Timezone || answer.EpochSeconds == 0 {
			t.Fatalf("unexpected answer %+v", answer)
		}

		if len(got.all()) != 0 {
			t.Fatalf("housekeeping reached the handler: %v", got.all())
		}
	})
}

func TestDial_AnythingElseBeforeTheHelloIsSkippedNotFatal(t *testing.T) {
	forBothFramings(t, func(t *testing.T, key string) {
		device := newFakeDevice(t, key)
		device.sendOtherFirst = true

		var logged atomic.Int32

		cfg := device.config()
		cfg.Logf = func(string, ...any) { logged.Add(1) }

		got := newCollector()
		c := dial(t, cfg, got.handle)

		if c.Hello().Name != "plug1" || logged.Load() != 1 || len(got.all()) != 0 {
			t.Fatalf("hello %+v, %d logged, %d delivered", c.Hello(), logged.Load(), len(got.all()))
		}
	})
}
