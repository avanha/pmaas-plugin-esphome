package esphome

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

// fakeNativeConn stands in for a connection to a device.
type fakeNativeConn struct {
	mu      sync.Mutex
	sent    []proto.Message
	done    chan struct{}
	err     error
	closed  bool
	sendErr error
	handler nativeapi.Handler
	cfg     nativeapi.Config
}

func newFakeNativeConn() *fakeNativeConn { return &fakeNativeConn{done: make(chan struct{})} }

func (c *fakeNativeConn) Send(m proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sendErr != nil {
		return c.sendErr
	}

	c.sent = append(c.sent, m)

	return nil
}

func (c *fakeNativeConn) Done() <-chan struct{} { return c.done }

func (c *fakeNativeConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

// end ends the connection the way the device dropping it would.
func (c *fakeNativeConn) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.closed {
		c.closed = true
		c.err = err
		close(c.done)
	}
}

func (c *fakeNativeConn) Close() error {
	c.end(nativeapi.ErrClosed)

	return nil
}

func (c *fakeNativeConn) sentTypes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var types []string

	for _, m := range c.sent {
		types = append(types, fmt.Sprintf("%T", m))
	}

	return types
}

// recorder collects what the manager reports, in order.
type recorder struct {
	mu     sync.Mutex
	events []string
	cond   chan struct{}
}

func newRecorder() *recorder { return &recorder{cond: make(chan struct{}, 64)} }

func (r *recorder) add(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()

	select {
	case r.cond <- struct{}{}:
	default:
	}
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.events...)
}

func (r *recorder) callbacks() nativeCallbacks {
	return nativeCallbacks{
		onConnected:    func(name, address string) { r.add("connected " + name + " " + address) },
		onMessage:      func(name string, m proto.Message) { r.add(fmt.Sprintf("message %s %T", name, m)) },
		onDisconnected: func(name string) { r.add("disconnected " + name) },
	}
}

func (r *recorder) waitFor(t *testing.T, what string, condition func([]string) bool) []string {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		if events := r.all(); condition(events) {
			return events
		}

		select {
		case <-r.cond:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s; got %v", what, r.all())
		}
	}
}

func count(events []string, prefix string) int {
	n := 0

	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}

	return n
}

// scriptedDial returns a dial function that hands out the connections in order, or the errors, and remembers the
// configuration each was asked for.
type scriptedDial struct {
	mu      sync.Mutex
	results []any // *fakeNativeConn or error
	calls   []nativeapi.Config
	times   []time.Time
	conns   []*fakeNativeConn
}

func (s *scriptedDial) dial(ctx context.Context, cfg nativeapi.Config, handler nativeapi.Handler) (nativeConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, cfg)
	s.times = append(s.times, time.Now())

	if len(s.results) == 0 {
		// Nothing left in the script: wait to be stopped, as a device that can't be reached would.
		s.mu.Unlock()
		<-ctx.Done()
		s.mu.Lock()

		return nil, ctx.Err()
	}

	next := s.results[0]
	s.results = s.results[1:]

	if err, isErr := next.(error); isErr {
		return nil, err
	}

	conn := next.(*fakeNativeConn)
	conn.handler = handler
	conn.cfg = cfg
	s.conns = append(s.conns, conn)

	return conn, nil
}

func (s *scriptedDial) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.calls)
}

func newTestManager(devices []NativeDeviceConfig, rec *recorder, script *scriptedDial) *nativeManager {
	m := newNativeManager(devices, rec.callbacks(), slog.New(slog.DiscardHandler))
	m.dial = script.dial
	m.minBackoff = 5 * time.Millisecond
	m.maxBackoff = 80 * time.Millisecond
	m.stableAfter = time.Hour
	m.jitter = func() float64 { return 0 }

	return m
}

var kaufDevice = NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7", Key: "", DisplayName: "Heater"}

func TestNativeManager_ConnectsAsksForEverythingAndDeliversWhatComesBack(t *testing.T) {
	conn := newFakeNativeConn()
	script := &scriptedDial{results: []any{conn}}
	rec := newRecorder()
	m := newTestManager([]NativeDeviceConfig{kaufDevice}, rec, script)
	m.Start()

	defer m.Stop()

	rec.waitFor(t, "the connection", func(e []string) bool { return count(e, "connected") == 1 })

	// Who it is, what it has, then what each is doing, in that order.
	want := []string{"*pb.DeviceInfoRequest", "*pb.ListEntitiesRequest", "*pb.SubscribeStatesRequest"}
	for i := 0; i < 100 && len(conn.sentTypes()) < 3; i++ {
		time.Sleep(10 * time.Millisecond)
	}

	if got := conn.sentTypes(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sent %v, want %v", got, want)
	}

	conn.handler(&pb.ListEntitiesDoneResponse{})
	conn.handler(&pb.SwitchStateResponse{Key: 1})

	events := rec.waitFor(t, "the messages", func(e []string) bool { return count(e, "message") == 2 })
	if events[0] != "connected plug1 10.0.0.7:6053" || events[1] != "message plug1 *pb.ListEntitiesDoneResponse" ||
		events[2] != "message plug1 *pb.SwitchStateResponse" {
		t.Fatalf("unexpected events %v", events)
	}

	if cfg := conn.cfg; cfg.Address != "10.0.0.7:6053" || cfg.Name != "plug1" || cfg.Key != "" || cfg.ClientInfo != "PMAAS" {
		t.Fatalf("unexpected connection config %+v", cfg)
	}

	if m.conn("plug1") != conn || m.problem("plug1") != "" {
		t.Fatal("the connection should be available, with no problem")
	}
}

func TestNativeManager_ReconnectsWhenTheConnectionEnds(t *testing.T) {
	first, second := newFakeNativeConn(), newFakeNativeConn()
	script := &scriptedDial{results: []any{first, second}}
	rec := newRecorder()
	m := newTestManager([]NativeDeviceConfig{kaufDevice}, rec, script)
	m.Start()

	defer m.Stop()

	rec.waitFor(t, "the first connection", func(e []string) bool { return count(e, "connected") == 1 })
	first.end(&nativeapi.DeviceDisconnectedError{})

	events := rec.waitFor(t, "the second connection", func(e []string) bool { return count(e, "connected") == 2 })

	// It was reported disconnected in between, so the registry saw the gap.
	order := []string{}

	for _, e := range events {
		if strings.HasPrefix(e, "connected") || strings.HasPrefix(e, "disconnected") {
			order = append(order, strings.Fields(e)[0])
		}
	}

	if fmt.Sprint(order) != "[connected disconnected connected]" {
		t.Fatalf("unexpected order %v", order)
	}

	// And the new connection was asked for everything again: a device that rebooted has told us nothing yet.
	for i := 0; i < 100 && len(second.sentTypes()) < 3; i++ {
		time.Sleep(10 * time.Millisecond)
	}

	if len(second.sentTypes()) != 3 {
		t.Fatalf("sent %v", second.sentTypes())
	}
}

func TestNativeManager_AFailedConnectionIsRetriedWithAGrowingDelayAndTheProblemIsReported(t *testing.T) {
	conn := newFakeNativeConn()
	script := &scriptedDial{results: []any{
		errors.New("connection refused"), errors.New("connection refused"), errors.New("connection refused"),
		errors.New("no route to host"), conn,
	}}
	rec := newRecorder()
	m := newTestManager([]NativeDeviceConfig{kaufDevice}, rec, script)
	m.Start()

	defer m.Stop()

	for i := 0; i < 200 && script.callCount() < 2; i++ {
		time.Sleep(5 * time.Millisecond)
	}

	if got := m.problem("plug1"); !strings.Contains(got, "connection refused") && !strings.Contains(got, "no route") {
		t.Fatalf("the problem wasn't reported: %q", got)
	}

	rec.waitFor(t, "the connection", func(e []string) bool { return count(e, "connected") == 1 })

	script.mu.Lock()
	times := append([]time.Time(nil), script.times...)
	script.mu.Unlock()

	// 5ms, 10ms, 20ms, 40ms between the five attempts: each at least as long as the one before.
	var gaps []time.Duration
	for i := 1; i < len(times); i++ {
		gaps = append(gaps, times[i].Sub(times[i-1]))
	}

	for i := 1; i < len(gaps); i++ {
		if gaps[i] < gaps[i-1] {
			t.Fatalf("the delay shrank: %v", gaps)
		}
	}

	if gaps[len(gaps)-1] < 30*time.Millisecond {
		t.Fatalf("the delay didn't grow: %v", gaps)
	}

	if m.problem("plug1") != "" {
		t.Fatalf("the problem outlived the connection: %q", m.problem("plug1"))
	}
}

func TestNativeManager_TheDelayIsCapped(t *testing.T) {
	m := newTestManager(nil, newRecorder(), &scriptedDial{})
	backoff := m.minBackoff

	var delays []time.Duration

	for i := 0; i < 8; i++ {
		delays = append(delays, m.delayAfterFailure(errors.New("down"), &backoff))
	}

	want := []time.Duration{5, 10, 20, 40, 80, 80, 80, 80}
	for i, d := range delays {
		if d != want[i]*time.Millisecond {
			t.Fatalf("delays %v", delays)
		}
	}
}

func TestNativeManager_AFailureThatWaitingWontFixIsWaitedOutSlowlyAndExplained(t *testing.T) {
	m := newTestManager(nil, newRecorder(), &scriptedDial{})
	backoff := m.minBackoff

	for _, err := range []error{
		nativeapi.ErrInvalidKey, nativeapi.ErrEncryptionRequired, nativeapi.ErrDeviceNotEncrypted,
		nativeapi.ErrIncompatibleVersion, nativeapi.ErrPasswordRequired, &nativeapi.WrongDeviceError{Expected: "a", Got: "b"},
		fmt.Errorf("wrapped: %w", nativeapi.ErrInvalidKey),
	} {
		if got := m.delayAfterFailure(err, &backoff); got != m.maxBackoff {
			t.Errorf("%v: waited %v, want %v", err, got, m.maxBackoff)
		}

		if !isPersistentNativeError(err) {
			t.Errorf("%v should be persistent", err)
		}
	}

	if backoff != m.minBackoff {
		t.Errorf("a persistent failure moved the ordinary backoff to %v", backoff)
	}

	for _, err := range []error{errors.New("connection refused"), nativeapi.ErrKeepAliveTimeout, &net.OpError{Op: "dial"}} {
		if isPersistentNativeError(err) {
			t.Errorf("%v shouldn't be persistent", err)
		}
	}
}

func TestDescribeNativeError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nativeapi.ErrInvalidKey, "doesn't accept the configured encryption key"},
		{nativeapi.ErrEncryptionRequired, "set the device's Key"},
		{nativeapi.ErrDeviceNotEncrypted, "remove the Key"},
		{nativeapi.ErrPasswordRequired, "api: encryption: key"},
		{&nativeapi.WrongDeviceError{Expected: "plug1", Got: "plug2"}, `calls itself "plug2", not "plug1"`},
		{&nativeapi.WrongDeviceError{Expected: "plug1", Got: "plug2"}, "use DisplayName"},
		{errors.New("connection refused"), "connection refused"},
	} {
		if got := describeNativeError(c.err); !strings.Contains(got, c.want) {
			t.Errorf("%v: got %q, want one containing %q", c.err, got, c.want)
		}
	}
}

func TestNativeManager_JitterLengthensTheDelayByAtMostAFifth(t *testing.T) {
	m := newTestManager(nil, newRecorder(), &scriptedDial{})
	m.jitter = func() float64 { return 0.999 }
	backoff := 100 * time.Millisecond

	got := m.delayAfterFailure(errors.New("down"), &backoff)
	if got <= 100*time.Millisecond || got > 120*time.Millisecond {
		t.Fatalf("got %v", got)
	}
}

func TestNativeManager_AStableConnectionStartsTheDelaySequenceOver(t *testing.T) {
	a, b := newFakeNativeConn(), newFakeNativeConn()
	script := &scriptedDial{results: []any{errors.New("down"), errors.New("down"), errors.New("down"), a, b}}
	rec := newRecorder()
	m := newTestManager([]NativeDeviceConfig{kaufDevice}, rec, script)
	m.stableAfter = 0 // any connection counts as stable
	m.minBackoff, m.maxBackoff = 20*time.Millisecond, 400*time.Millisecond
	m.Start()

	defer m.Stop()

	rec.waitFor(t, "the connection", func(e []string) bool { return count(e, "connected") == 1 })

	start := time.Now()

	a.end(errors.New("dropped"))
	rec.waitFor(t, "the reconnection", func(e []string) bool { return count(e, "connected") == 2 })

	// Three failures had grown the delay to 160ms; being stable put it back to the first, 20ms.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("reconnecting took %v: the delay wasn't reset", elapsed)
	}
}

func TestNativeManager_EachDeviceHasItsOwnConnection(t *testing.T) {
	one, two := newFakeNativeConn(), newFakeNativeConn()

	var mu sync.Mutex

	byAddress := map[string]*fakeNativeConn{"10.0.0.7:6053": one, "plug2.local:6053": two}

	rec := newRecorder()
	m := newNativeManager(
		[]NativeDeviceConfig{kaufDevice, {Name: "plug2", Address: "plug2.local"}}, rec.callbacks(), slog.New(slog.DiscardHandler))
	m.dial = func(ctx context.Context, cfg nativeapi.Config, handler nativeapi.Handler) (nativeConn, error) {
		mu.Lock()
		defer mu.Unlock()

		conn := byAddress[cfg.Address]
		conn.handler = handler

		return conn, nil
	}
	m.Start()

	defer m.Stop()

	rec.waitFor(t, "both", func(e []string) bool { return count(e, "connected") == 2 })

	one.handler(&pb.SwitchStateResponse{Key: 1})
	two.handler(&pb.SensorStateResponse{Key: 2})

	events := rec.waitFor(t, "both messages", func(e []string) bool { return count(e, "message") == 2 })

	joined := strings.Join(events, "|")
	if !strings.Contains(joined, "message plug1 *pb.SwitchStateResponse") || !strings.Contains(joined, "message plug2 *pb.SensorStateResponse") {
		t.Fatalf("unexpected events %v", events)
	}

	if m.conn("plug1") != one || m.conn("plug2") != two || m.conn("nobody") != nil {
		t.Fatal("unexpected connections")
	}
}

func TestNativeManager_StopClosesTheConnectionsAndNothingIsReportedAfterIt(t *testing.T) {
	conn := newFakeNativeConn()
	script := &scriptedDial{results: []any{conn}}
	rec := newRecorder()
	m := newTestManager([]NativeDeviceConfig{kaufDevice}, rec, script)
	m.Start()

	rec.waitFor(t, "the connection", func(e []string) bool { return count(e, "connected") == 1 })

	m.Stop()

	select {
	case <-conn.Done():
	default:
		t.Fatal("the connection wasn't closed")
	}

	if m.conn("plug1") != nil {
		t.Fatal("a stopped manager still hands out a connection")
	}

	after := len(rec.all())

	// A dial still in flight or a reconnect due must not start up again.
	time.Sleep(60 * time.Millisecond)

	if len(rec.all()) != after || script.callCount() != 1 {
		t.Fatalf("something happened after Stop: %v, %d dials", rec.all(), script.callCount())
	}
}

func TestNativeManager_StopInterruptsAConnectionAttempt(t *testing.T) {
	script := &scriptedDial{} // nothing to hand out: the dial waits for the context
	m := newTestManager([]NativeDeviceConfig{kaufDevice}, newRecorder(), script)
	m.Start()

	for i := 0; i < 100 && script.callCount() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}

	stopped := make(chan struct{})

	go func() {
		m.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop didn't return while a connection was being attempted")
	}
}

func TestNativeManager_StopInterruptsTheWaitBetweenAttempts(t *testing.T) {
	script := &scriptedDial{results: []any{errors.New("down")}}
	m := newTestManager([]NativeDeviceConfig{kaufDevice}, newRecorder(), script)
	m.minBackoff, m.maxBackoff = time.Hour, time.Hour
	m.Start()

	for i := 0; i < 100 && script.callCount() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(20 * time.Millisecond)

	stopped := make(chan struct{})

	go func() {
		m.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop didn't return while waiting to try again")
	}
}

func TestNativeManager_AConnectionThatCannotBeSentToIsClosedAndRetried(t *testing.T) {
	broken := newFakeNativeConn()
	broken.sendErr = errors.New("write failed")
	good := newFakeNativeConn()
	script := &scriptedDial{results: []any{broken, good}}
	rec := newRecorder()
	m := newTestManager([]NativeDeviceConfig{kaufDevice}, rec, script)
	m.Start()

	defer m.Stop()

	rec.waitFor(t, "the second connection", func(e []string) bool { return count(e, "connected") == 2 })

	select {
	case <-broken.Done():
	default:
		t.Fatal("the connection that couldn't be written to wasn't closed")
	}
}

func TestNativeManager_ACallbackThatWaitsDoesNotLoseOrReorderMessages(t *testing.T) {
	conn := newFakeNativeConn()
	script := &scriptedDial{results: []any{conn}}
	rec := newRecorder()

	callbacks := rec.callbacks()
	release := make(chan struct{})
	first := true
	callbacks.onMessage = func(name string, m proto.Message) {
		if first {
			first = false
			<-release
		}

		rec.add(fmt.Sprintf("message %s %v", name, m.(*pb.SwitchStateResponse).Key))
	}

	m := newNativeManager([]NativeDeviceConfig{kaufDevice}, callbacks, slog.New(slog.DiscardHandler))
	m.dial = script.dial
	m.Start()

	defer m.Stop()

	rec.waitFor(t, "the connection", func(e []string) bool { return count(e, "connected") == 1 })

	go func() {
		for i := uint32(1); i <= 5; i++ {
			conn.handler(&pb.SwitchStateResponse{Key: i})
		}
	}()

	time.Sleep(30 * time.Millisecond)
	close(release)

	events := rec.waitFor(t, "all five", func(e []string) bool { return count(e, "message") == 5 })
	if fmt.Sprint(events[1:]) != "[message plug1 1 message plug1 2 message plug1 3 message plug1 4 message plug1 5]" {
		t.Fatalf("unexpected events %v", events)
	}
}
