package esphome

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/avanha/pmaas-common/mailbox"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
	"github.com/avanha/pmaas-spi/device"
	"google.golang.org/protobuf/proto"
)

// startNativePlugin starts the plugin with connections to devices made by dial rather than over the network.
func startNativePlugin(t *testing.T, config PluginConfig, dial nativeDialFunc) (*plugin, *pluginContainer) {
	t.Helper()

	c := &pluginContainer{mailbox: mailbox.NewMailbox()}
	t.Cleanup(c.mailbox.Stop)

	p := NewPlugin(config).(*plugin)
	p.nativeDial = dial
	p.Init(c)
	p.registry.log = slog.New(slog.DiscardHandler)

	c.onPluginGoroutine(p.Start)

	return p, c
}

func stopPlugin(t *testing.T, p *plugin, c *pluginContainer) {
	t.Helper()

	var done chan func()

	c.onPluginGoroutine(func() { done = p.Stop() })

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete")
	}
}

// nextConn waits for the plugin to connect to a device, and fails the test rather than waiting forever if it
// never does.
func nextConn(t *testing.T, conns <-chan *fakeNativeConn) *fakeNativeConn {
	t.Helper()

	select {
	case conn := <-conns:
		return conn
	case <-time.After(5 * time.Second):
		t.Fatal("the plugin never connected to the device")
		return nil
	}
}

// aKaufPlugDial is a dial function that connects to a device that behaves like a stock Kauf plug: it answers the
// three requests with what a plug would say, and does what a command says.
func aKaufPlugDial(conns chan<- *fakeNativeConn) nativeDialFunc {
	var mu sync.Mutex

	return func(ctx context.Context, cfg nativeapi.Config, handler nativeapi.Handler) (nativeConn, error) {
		conn := &answeringConn{fakeNativeConn: newFakeNativeConn()}
		conn.handler = handler

		mu.Lock()
		conns <- conn.fakeNativeConn
		mu.Unlock()

		return conn, nil
	}
}

// answeringConn is a fakeNativeConn that answers like a plug does.
type answeringConn struct {
	*fakeNativeConn
}

func (c *answeringConn) Send(m proto.Message) error {
	if err := c.fakeNativeConn.Send(m); err != nil {
		return err
	}

	// Answers are delivered here, in order, as a connection's single read loop would.
	switch m := m.(type) {
	case *pb.DeviceInfoRequest:
		c.handler(&pb.DeviceInfoResponse{
			Name: "plug1", FriendlyName: "Kauf Plug", MacAddress: "AA:BB:CC:DD:EE:FF", Model: "PLF10", Manufacturer: "Kauf",
			EsphomeVersion: "2026.9.1",
		})
	case *pb.ListEntitiesRequest:
		for _, e := range kaufEntities() {
			c.handler(e)
		}
	case *pb.SubscribeStatesRequest:
		c.handler(&pb.SwitchStateResponse{Key: 11, State: false})
		c.handler(&pb.SensorStateResponse{Key: 12, State: 1200})
	case *pb.SwitchCommandRequest:
		c.handler(&pb.SwitchStateResponse{Key: m.Key, State: m.State})
	}

	return nil
}

func TestPlugin_NativeOnlyStartsNoBrokerAndSaysSo(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7", DisplayName: "Heater"})

	conns := make(chan *fakeNativeConn, 4)
	p, c := startNativePlugin(t, config, aKaufPlugDial(conns))

	defer stopPlugin(t, p, c)

	if conn, err := net.DialTimeout("tcp", config.ListenAddress, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("a broker is listening, though no MQTT device is configured")
	}

	var status struct {
		notUsed, running bool
		problem          string
	}

	c.onPluginGoroutine(func() {
		s := p.getStatusAndEntities().Status
		status.notUsed, status.running, status.problem = s.BrokerNotUsed, s.BrokerRunning, s.BrokerProblem
	})

	if !status.notUsed || status.running || status.problem != "" {
		t.Fatalf("unexpected broker status %+v", status)
	}
}

func TestPlugin_ANativeDeviceIsRegisteredAndControlledAndStopsCleanly(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7", DisplayName: "Heater"})

	conns := make(chan *fakeNativeConn, 4)
	p, c := startNativePlugin(t, config, aKaufPlugDial(conns))

	eventually(t, c, "the switch and its power", func() bool {
		return len(c.registrations) == 3 && p.registry.switchViews()[0].HasData && p.registry.powerViews()[0].HasPower
	})

	var views struct {
		switches int
		power    float64
		label    string
		devices  int
		counts   [2]int
	}

	c.onPluginGoroutine(func() {
		status := p.getStatusAndEntities()
		cards := deviceViews(t, status.Entities)
		views.switches = len(p.registry.switchViews())
		views.power = p.registry.powerViews()[0].Power
		views.label = cards[0].Label
		views.devices = len(cards)
		views.counts = [2]int{status.Status.SwitchCount, status.Status.PowerMonitorCount}

		// The switch, then the power, in the card of the device they're on.
		if got := kinds(cards[0]); got != "switch,power" {
			t.Errorf("unexpected children %q", got)
		}

		if d := cards[0]; d.Transport != "Native API" || !d.Connected || d.Address != "10.0.0.7:6053" || d.Problem != "" {
			t.Errorf("unexpected device status %+v", d)
		}
	})

	if views.switches != 1 || views.power != 1200 || views.label != "Heater" || views.devices != 1 || views.counts != [2]int{1, 1} {
		t.Fatalf("unexpected views %+v", views)
	}

	// Turn it on, through the stub as anything else in PMAAS would: the command reaches the device, which
	// answers with its new state.
	conn := nextConn(t, conns)
	relay := c.stubFor(t, device.ISwitchType).(device.ISwitch)

	if err := relay.SetState(device.SwitchStateOn); err != nil {
		t.Fatal(err)
	}

	eventually(t, c, "the relay to report on", func() bool {
		return p.registry.switchViews()[0].State == "On"
	})

	found := false

	for _, m := range conn.sentMessages() {
		if command, ok := m.(*pb.SwitchCommandRequest); ok && command.Key == 11 && command.State {
			found = true
		}
	}

	if !found {
		t.Fatalf("the command never reached the device: %v", conn.sentTypes())
	}

	stopPlugin(t, p, c)

	select {
	case <-conn.Done():
	default:
		t.Fatal("the connection wasn't closed")
	}

	c.onPluginGoroutine(func() {
		if len(c.deregistered) != 3 { // the switch, its power, and the device
			t.Errorf("expected the device and both entities deregistered, got %v", c.deregistered)
		}
	})

	// With the plugin stopped nothing can be commanded: the stub is closed, and a call through it panics, as
	// the other plugins' stubs do.
	defer func() {
		if recover() == nil {
			t.Fatal("expected a call through a closed stub to panic")
		}
	}()

	_ = relay.SetState(device.SwitchStateOff)
}

func (c *fakeNativeConn) sentMessages() []proto.Message {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]proto.Message(nil), c.sent...)
}

func TestPlugin_ANativeDeviceThatCannotBeReachedShowsWhy(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7", Key: testNativeKey})

	dial := func(ctx context.Context, cfg nativeapi.Config, h nativeapi.Handler) (nativeConn, error) {
		return nil, nativeapi.ErrInvalidKey
	}

	p, c := startNativePlugin(t, config, dial)

	defer stopPlugin(t, p, c)

	var device0 struct {
		problem   string
		connected bool
	}

	eventually(t, c, "the problem", func() bool {
		d := deviceViews(t, p.getStatusAndEntities().Entities)[0]
		device0.problem, device0.connected = d.Problem, d.Connected

		return d.Problem != ""
	})

	if device0.connected || device0.problem != "the device doesn't accept the configured encryption key" {
		t.Fatalf("unexpected device %+v", device0)
	}
}

func TestPlugin_ABadNativeEntryIsSkippedAndTheRestRun(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "bad", Address: "10.0.0.9", Key: "nonsense"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7"})

	conns := make(chan *fakeNativeConn, 4)
	p, c := startNativePlugin(t, config, aKaufPlugDial(conns))

	defer stopPlugin(t, p, c)

	nextConn(t, conns)

	eventually(t, c, "the good device", func() bool { return len(c.registrations) == 3 }) // its device, switch and power

	c.onPluginGoroutine(func() {
		if devices := deviceViews(t, p.getStatusAndEntities().Entities); len(devices) != 1 || devices[0].Name != "plug1" {
			t.Errorf("unexpected devices %+v", devices)
		}
	})
}

func TestPlugin_NativeAndMQTTDevicesRunTogether(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "garagedoor1", Password: "secret1"})
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7"})

	conns := make(chan *fakeNativeConn, 4)
	p, c := startNativePlugin(t, config, aKaufPlugDial(conns))

	opts := paho.NewClientOptions().AddBroker("tcp://" + config.ListenAddress).SetClientID("g").
		SetUsername("garagedoor1").SetPassword("secret1").SetAutoReconnect(false)
	client := paho.NewClient(opts)

	if token := client.Connect(); !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("connect: %v", token.Error())
	}

	token := client.Publish("homeassistant/binary_sensor/garagedoor1/door/config", 0, true, espDoorConfig)
	token.WaitTimeout(2 * time.Second)
	client.Publish("garagedoor1/binary_sensor/garage_door_position/state", 0, true, "OFF").WaitTimeout(2 * time.Second)

	eventually(t, c, "devices of both kinds", func() bool {
		return len(c.registrations) == 5 // the door, the plug's switch and power, and the two devices
	})

	c.onPluginGoroutine(func() {
		s := p.getStatusAndEntities().Status

		cards := deviceViews(t, p.getStatusAndEntities().Entities)

		if !s.BrokerRunning || s.BrokerNotUsed || len(cards) != 2 || cards[0].Transport != "MQTT" ||
			cards[1].Transport != "Native API" || s.ConnectedDevices != 2 || s.DeviceCount != 2 {
			t.Errorf("unexpected status %+v", s)
		}
	})

	client.Disconnect(0)
	stopPlugin(t, p, c)
}

func TestPlugin_SendNativeFailsWhenThereIsNothingToSendTo(t *testing.T) {
	p := &plugin{log: slog.New(slog.DiscardHandler)}

	if err := p.sendNative("plug1", &pb.PingRequest{}); !errors.Is(err, errNativeNotRunning) {
		t.Fatalf("got %v", err)
	}
}

func TestPlugin_SendNativeFailsForADeviceThatIsNotConnected(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "plug1", Address: "10.0.0.7"})

	dial := func(ctx context.Context, cfg nativeapi.Config, h nativeapi.Handler) (nativeConn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	p, c := startNativePlugin(t, config, dial)

	defer stopPlugin(t, p, c)

	var err error

	c.onPluginGoroutine(func() { err = p.sendNative("plug1", &pb.PingRequest{}) })

	if !errors.Is(err, errNativeNotConnected) {
		t.Fatalf("got %v", err)
	}
}

const testNativeKey = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="

// A plugin with only MQTT devices has an outbox and no native manager, and mustn't panic if something asks it to
// send to a native device.
func TestPlugin_SendNativeOnAnMQTTOnlyPluginFailsRatherThanPanicking(t *testing.T) {
	outbox := mailbox.NewOutbox()
	defer outbox.Stop()

	p := &plugin{log: slog.New(slog.DiscardHandler), outbox: outbox}

	if err := p.sendNative("plug1", &pb.PingRequest{}); !errors.Is(err, errNativeNotRunning) {
		t.Fatalf("got %v", err)
	}
}
