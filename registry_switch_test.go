package esphome

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
)

func switchConfig(deviceName string, object string, name string) string {
	return fmt.Sprintf(
		`{"name":%q,"state_topic":"%s/switch/%s/state","command_topic":"%s/switch/%s/command"}`,
		name, deviceName, object, deviceName, object)
}

// published records what the registry asked the plugin to publish.
type published struct {
	topic   string
	payload string
}

func (f *fixture) recordPublishes(err error) *[]published {
	var sent []published

	f.registry.publish = func(topic string, payload []byte) error {
		if err != nil {
			return err
		}

		sent = append(sent, published{topic, string(payload)})

		return nil
	}

	return &sent
}

// switchStub returns the stub handed out for the first registration, which has to be a switch.
func (f *fixture) switchStub() device.ISwitch {
	f.t.Helper()

	if len(f.container.registrations) == 0 || f.container.registrations[0].entityType != device.ISwitchType {
		f.t.Fatalf("expected a switch registration, got %+v", f.container.registrations)
	}

	stub, err := f.container.registrations[0].stubFactory()
	if err != nil {
		f.t.Fatal(err)
	}

	return stub.(device.ISwitch)
}

func lastSwitchState(t *testing.T, c *fakeContainer) device.Switch {
	t.Helper()

	return c.lastBroadcast(t).event.(events.EntityStateChangedEvent).NewState.(device.Switch)
}

func TestSwitch_RegisteredOnDiscovery(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	if len(f.container.registrations) != 1 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	reg := f.container.registrations[0]
	if reg.entityType != device.ISwitchType || reg.name != "Garage 1 Plug" || reg.uniqueData != "garage1/plug" {
		t.Fatalf("unexpected registration %+v", reg)
	}

	if got := f.switchStub().GetSwitchData(); got.HasData || got.State != device.SwitchStateUnknown {
		t.Fatalf("a switch that hasn't reported must be Unknown, not Off: %+v", got)
	}
}

func TestSwitch_FirstReportEstablishesTheStateAndLaterOnesChangeIt(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	f.advance(time.Second)
	f.state("garage1", "garage1/switch/plug/state", "OFF")

	first := lastSwitchState(t, f.container)
	if !first.HasData || first.State != device.SwitchStateOff || !first.LastChangeTime.IsZero() ||
		!first.LastUpdateTime.Equal(f.clock) || first.Connectivity != environment.ConnectivityOnline {
		t.Fatalf("unexpected first state %+v", first)
	}

	for _, b := range f.container.broadcasts {
		if _, isChange := b.event.(device.SwitchStateChangedEvent); isChange {
			t.Fatalf("the first report isn't a change: %+v", b)
		}
	}

	// The same state again: the update time moves, but it's neither a change nor worth broadcasting.
	before := len(f.container.broadcasts)
	f.advance(time.Second)
	f.state("garage1", "garage1/switch/plug/state", "OFF")

	if len(f.container.broadcasts) != before {
		t.Fatalf("an unchanged state was broadcast: %+v", f.container.broadcasts[before:])
	}

	f.advance(time.Second)
	f.state("garage1", "garage1/switch/plug/state", "ON")

	changed := f.container.lastBroadcast(t).event.(device.SwitchStateChangedEvent)
	if changed.NewState != device.SwitchStateOn || changed.OldState != device.SwitchStateOff ||
		changed.EntityType != device.ISwitchType || changed.Name != "Garage 1 Plug" {
		t.Fatalf("unexpected change %+v", changed)
	}

	if got := f.switchStub().GetSwitchData(); got.State != device.SwitchStateOn || !got.LastChangeTime.Equal(f.clock) {
		t.Fatalf("unexpected state %+v", got)
	}
}

func TestSwitch_UnrecognizedStatePayloadIgnored(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	f.state("garage1", "garage1/switch/plug/state", "ON")

	before := len(f.container.broadcasts)
	f.state("garage1", "garage1/switch/plug/state", "banana")

	if len(f.container.broadcasts) != before || f.switchStub().GetSwitchData().State != device.SwitchStateOn {
		t.Fatal("an unrecognized payload changed something")
	}

	if len(f.logs) == 0 || !strings.Contains(f.logs[len(f.logs)-1], "unrecognized switch state") {
		t.Fatalf("expected it to be logged, got %v", f.logs)
	}
}

func TestSwitch_CustomPayloads(t *testing.T) {
	f := newFixture(t)
	sent := f.recordPublishes(nil)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", `{"name":"Plug","state_topic":"garage1/switch/plug/state",`+
		`"command_topic":"garage1/switch/plug/command","payload_on":"1","payload_off":"0"}`)

	f.state("garage1", "garage1/switch/plug/state", "1")

	if got := f.switchStub().GetSwitchData().State; got != device.SwitchStateOn {
		t.Fatalf("got %v", got)
	}

	if err := f.switchStub().SetState(device.SwitchStateOff); err != nil {
		t.Fatal(err)
	}

	if want := []published{{"garage1/switch/plug/command", "0"}}; !reflect.DeepEqual(*sent, want) {
		t.Fatalf("sent %+v, want %+v", *sent, want)
	}
}

func TestSwitch_SetStatePublishesTheCommandButDoesNotChangeTheState(t *testing.T) {
	f := newFixture(t)
	sent := f.recordPublishes(nil)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	f.state("garage1", "garage1/switch/plug/state", "OFF")
	broadcasts := len(f.container.broadcasts)

	if err := f.switchStub().SetState(device.SwitchStateOn); err != nil {
		t.Fatal(err)
	}

	if want := []published{{"garage1/switch/plug/command", "ON"}}; !reflect.DeepEqual(*sent, want) {
		t.Fatalf("sent %+v, want %+v", *sent, want)
	}

	// It's the device that says what state it's in, not the request.
	if got := f.switchStub().GetSwitchData(); got.State != device.SwitchStateOff || len(f.container.broadcasts) != broadcasts {
		t.Fatalf("the request changed the state: %+v", got)
	}

	f.state("garage1", "garage1/switch/plug/state", "ON")

	if got := f.switchStub().GetSwitchData(); got.State != device.SwitchStateOn {
		t.Fatalf("the device's report didn't: %+v", got)
	}

	if err := f.switchStub().SetState(device.SwitchStateOff); err != nil {
		t.Fatal(err)
	}

	if len(*sent) != 2 || (*sent)[1].payload != "OFF" {
		t.Fatalf("sent %+v", *sent)
	}
}

func TestSwitch_SetStateRefusesWhatCantBeDelivered(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(f *fixture) *[]published
		state   device.SwitchState
		wantErr string
		is      error
	}{
		{
			name: "unknown isn't a state to ask for",
			setup: func(f *fixture) *[]published {
				f.registry.HandleConnected("garage1", "10.0.0.1")
				return f.recordPublishes(nil)
			},
			state: device.SwitchStateUnknown, wantErr: "a switch can only be set to on or off", is: device.ErrInvalidSwitchState,
		},
		{
			name: "nor is a value that isn't one at all",
			setup: func(f *fixture) *[]published {
				f.registry.HandleConnected("garage1", "10.0.0.1")
				return f.recordPublishes(nil)
			},
			state: device.SwitchState(42), wantErr: "a switch can only be set to on or off", is: device.ErrInvalidSwitchState,
		},
		{
			name: "the device is offline",
			setup: func(f *fixture) *[]published {
				return f.recordPublishes(nil)
			},
			state: device.SwitchStateOn, wantErr: "the device is offline",
		},
		{
			name: "the device went offline",
			setup: func(f *fixture) *[]published {
				f.registry.HandleConnected("garage1", "10.0.0.1")
				f.registry.HandleDisconnected("garage1")
				return f.recordPublishes(nil)
			},
			state: device.SwitchStateOn, wantErr: "the device is offline",
		},
		{
			name: "there is no broker to publish with",
			setup: func(f *fixture) *[]published {
				f.registry.HandleConnected("garage1", "10.0.0.1")
				f.registry.publish = nil
				return &[]published{}
			},
			state: device.SwitchStateOn, wantErr: "the MQTT broker isn't running",
		},
		{
			name: "publishing fails",
			setup: func(f *fixture) *[]published {
				f.registry.HandleConnected("garage1", "10.0.0.1")
				return f.recordPublishes(errTest)
			},
			state: device.SwitchStateOn, wantErr: "nope", is: errTest,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
			sent := c.setup(f)

			err := f.switchStub().SetState(c.state)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %v, want one containing %q", err, c.wantErr)
			}

			if c.is != nil && !errors.Is(err, c.is) {
				t.Fatalf("%v should wrap %v", err, c.is)
			}

			if len(*sent) != 0 {
				t.Fatalf("something was published: %+v", *sent)
			}
		})
	}
}

func TestSwitch_WithoutACommandTopicCantBeControlled(t *testing.T) {
	f := newFixture(t)
	sent := f.recordPublishes(nil)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", `{"name":"Plug","state_topic":"garage1/switch/plug/state"}`)

	err := f.switchStub().SetState(device.SwitchStateOn)
	if err == nil || !strings.Contains(err.Error(), "command topic") || len(*sent) != 0 {
		t.Fatalf("got %v, sent %+v", err, *sent)
	}
}

// PMAAS publishes commands itself, with no ACL in the way, so a device mustn't be able to aim one at someone
// else's topics by announcing a command topic outside its own.
func TestSwitch_ACommandTopicOutsideTheDevicesTreeIsRefused(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug",
		`{"name":"Plug","state_topic":"garage1/switch/plug/state","command_topic":"garage2/switch/plug/command"}`)

	if len(f.container.registrations) != 0 {
		t.Fatalf("a switch that commands someone else's topic was registered: %+v", f.container.registrations)
	}

	if len(f.logs) == 0 || !strings.Contains(f.logs[len(f.logs)-1], "command topic") {
		t.Fatalf("expected the refusal to be logged, got %v", f.logs)
	}

	// Announced properly first and then re-announced pointing elsewhere, it's dropped rather than kept with
	// its new topic.
	f.discover("garage1", "switch", "plug2", switchConfig("garage1", "plug2", "Plug 2"))
	f.discover("garage1", "switch", "plug2",
		`{"name":"Plug 2","state_topic":"garage1/switch/plug2/state","command_topic":"garage2/switch/plug2/command"}`)

	if want := []string{"esphome_garage1/plug2"}; !reflect.DeepEqual(f.container.deregistered, want) {
		t.Fatalf("deregistered %v, want %v", f.container.deregistered, want)
	}
}

func TestSwitch_ConnectivityFollowsTheDevice(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	f.advance(time.Second)
	f.registry.HandleConnected("garage1", "10.0.0.1")

	if got := lastSwitchState(t, f.container); got.Connectivity != environment.ConnectivityOnline || !got.OnlineSince.Equal(f.clock) {
		t.Fatalf("expected online, got %+v", got)
	}

	f.state("garage1", "garage1/switch/plug/state", "ON")
	f.advance(time.Minute)
	f.registry.HandleDisconnected("garage1")

	got := lastSwitchState(t, f.container)
	if got.Connectivity != environment.ConnectivityOffline || !got.OfflineSince.Equal(f.clock) || got.State != device.SwitchStateOn {
		t.Fatalf("expected offline with the last state kept, got %+v", got)
	}
}

func TestSwitch_RemovedWhenTheDeviceRemovesIt(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	stub := f.switchStub()
	f.discover("garage1", "switch", "plug", "")

	if want := []string{"esphome_garage1/plug"}; !reflect.DeepEqual(f.container.deregistered, want) {
		t.Fatalf("deregistered %v, want %v", f.container.deregistered, want)
	}

	before := len(f.container.broadcasts)
	f.state("garage1", "garage1/switch/plug/state", "ON")

	if len(f.container.broadcasts) != before {
		t.Fatal("state for a removed switch was processed")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("expected a call through a closed stub to panic, like the other plugins' stubs")
		}
	}()

	_ = stub.SetState(device.SwitchStateOn)
}

func TestSwitch_RenamedOnReannouncement(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Heater"))

	if got := lastSwitchState(t, f.container); got.Name != "Garage 1 Heater" {
		t.Fatalf("unexpected name %q", got.Name)
	}

	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Heater"))

	before := len(f.container.broadcasts)
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Heater"))

	if len(f.container.broadcasts) != before {
		t.Fatal("re-announcing with no change was broadcast")
	}
}

func TestShutdown_DeregistersSwitchesAndPowerMonitors(t *testing.T) {
	f := newFixture(t)
	f.recordPublishes(nil)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))
	f.discover("garage1", "sensor", "power", sensorConfig("garage1", "power", "Power", "power", "W"))

	f.registry.Shutdown()

	if len(f.container.deregistered) != 2 {
		t.Fatalf("deregistered %v", f.container.deregistered)
	}
}
