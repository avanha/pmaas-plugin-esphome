package esphome

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/data"
	"github.com/avanha/pmaas-spi/device"
)

func TestSwitchViews(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("plug1", "10.0.0.5")
	f.discover("plug1", "switch", "relay", switchConfig("plug1", "relay", "Relay"))
	f.discover("plug1", "switch", "no_command", `{"name":"Indicator","state_topic":"plug1/switch/no_command/state"}`)
	f.advance(time.Second)
	f.state("plug1", "plug1/switch/relay/state", "ON")

	views := f.registry.switchViews()
	if len(views) != 2 {
		t.Fatalf("unexpected views %+v", views)
	}

	// Sorted by name: "plug1 Indicator" before "plug1 Relay".
	indicator, relay := views[0], views[1]

	if relay.Name != "plug1 Relay" || relay.DeviceName != "plug1" || relay.ObjectID != "relay" || !relay.DeviceConnected ||
		!relay.CanControl || !relay.HasData || relay.State != "On" || relay.StateClass != "on" ||
		!relay.LastUpdateTime.Equal(f.clock) {
		t.Errorf("unexpected relay view %+v", relay)
	}

	// With nowhere to send a command there's nothing to offer, however connected it is.
	if indicator.CanControl || !indicator.DeviceConnected || indicator.HasData || indicator.StateClass != "unknown" {
		t.Errorf("unexpected indicator view %+v", indicator)
	}

	f.advance(time.Minute)
	f.registry.HandleDisconnected("plug1")

	for _, v := range f.registry.switchViews() {
		if v.DeviceConnected || v.CanControl {
			t.Errorf("an offline device's switch must not be offered for control: %+v", v)
		}
	}
}

func TestPowerViews(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("plug1", "10.0.0.5")
	f.powerSensor("plug1", "power", "power", "kW")
	f.powerSensor("plug1", "energy", "energy", "Wh")
	f.advance(time.Second)
	f.powerState("plug1", "power", "1.5")
	f.powerState("plug1", "energy", "2500")
	f.powerSensor("plug2", "voltage", "voltage", "V")

	views := f.registry.powerViews()
	if len(views) != 2 {
		t.Fatalf("unexpected views %+v", views)
	}

	want := data.PowerView{
		Name: "plug1 Power", DeviceName: "plug1", DeviceConnected: true, HasData: true,
		HasPower: true, Power: 1500, HasEnergy: true, Energy: 2.5, LastUpdateTime: f.clock,
		OnlineSince: views[0].OnlineSince,
	}

	if !reflect.DeepEqual(views[0], want) {
		t.Errorf("got  %+v\nwant %+v", views[0], want)
	}

	// A device that has announced a sensor but not reported has a monitor with nothing in it yet.
	if views[1].DeviceName != "plug2" || views[1].HasData || views[1].HasVoltage || views[1].DeviceConnected {
		t.Errorf("unexpected second view %+v", views[1])
	}
}

func TestDeviceStatus_CountsSwitchesAndPowerMonitorAsEntities(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("plug1", "10.0.0.5")
	f.discover("plug1", "switch", "relay", switchConfig("plug1", "relay", "Relay"))
	f.powerSensor("plug1", "power", "power", "W")
	f.powerSensor("plug1", "voltage", "voltage", "V")

	// A relay, and one power monitor for however many sensors.
	if got := f.registry.deviceStatus("plug1").EntityCount; got != 2 {
		t.Fatalf("got %d", got)
	}
}

func TestOrderEntities_APlugsSwitchAndPowerSitTogetherAfterTheRest(t *testing.T) {
	got := orderEntities(
		[]data.ThermometerView{{DeviceName: "b", Name: "t"}, {DeviceName: "a", Name: "t"}},
		[]data.DoorSensorView{{DeviceName: "a", Name: "d"}},
		[]data.SwitchView{{DeviceName: "b", Name: "s2"}, {DeviceName: "b", Name: "s1"}, {DeviceName: "a", Name: "s"}},
		[]data.PowerView{{DeviceName: "b", Name: "p"}, {DeviceName: "a", Name: "p"}})

	var order []string

	for _, e := range got {
		switch v := e.(type) {
		case *data.ThermometerView:
			order = append(order, v.DeviceName+":thermometer")
		case *data.DoorSensorView:
			order = append(order, v.DeviceName+":door")
		case *data.SwitchView:
			order = append(order, v.DeviceName+":switch:"+v.Name)
		case *data.PowerView:
			order = append(order, v.DeviceName+":power")
		}
	}

	want := []string{
		"a:thermometer", "a:door", "a:switch:s", "a:power",
		"b:thermometer", "b:switch:s1", "b:switch:s2", "b:power",
	}

	if !reflect.DeepEqual(order, want) {
		t.Fatalf("got  %v\nwant %v", order, want)
	}
}

func TestGetStatusAndEntities_CountsAndListsEverything(t *testing.T) {
	config := NewPluginConfig()
	config.AddDevice(DeviceConfig{Name: "plug1", Password: "x"})

	f := newFixture(t)
	p := NewPlugin(config).(*plugin)
	p.registry = f.registry
	f.registry.HandleConnected("plug1", "10.0.0.5")
	f.discover("plug1", "switch", "relay", switchConfig("plug1", "relay", "Relay"))
	f.powerSensor("plug1", "power", "power", "W")

	got := p.getStatusAndEntities()

	if got.Status.SwitchCount != 1 || got.Status.PowerMonitorCount != 1 || len(got.Entities) != 2 {
		t.Fatalf("unexpected status %+v with %d entities", got.Status, len(got.Entities))
	}
}

func TestEntityStoreAdapter_SetSwitchRunsOnThePluginGoroutineAndReportsWhatTheSwitchSays(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "plug1", Password: "secret1"})

	p, c := startPlugin(t, config)
	adapter := &entityStoreAdapter{parent: p}

	var sent []published

	c.onPluginGoroutine(func() {
		p.registry.publish = func(topic string, payload []byte) error {
			sent = append(sent, published{topic, string(payload)})
			return nil
		}

		p.registry.HandleConnected("plug1", "10.0.0.5")
		p.registry.HandleMessage("plug1", "homeassistant/switch/plug1/relay/config", []byte(switchConfig("plug1", "relay", "Relay")))
	})

	// The command is accepted for delivery, which is all a request can know, and on means ON.
	if err := adapter.SetSwitch("plug1", "relay", true); err != nil {
		t.Fatal(err)
	}

	if err := adapter.SetSwitch("plug1", "relay", false); err != nil {
		t.Fatal(err)
	}

	c.onPluginGoroutine(func() {
		want := []published{{"plug1/switch/relay/command", "ON"}, {"plug1/switch/relay/command", "OFF"}}
		if !reflect.DeepEqual(sent, want) {
			t.Errorf("sent %+v, want %+v", sent, want)
		}
	})

	for _, c := range []struct {
		device, object, wantErr string
	}{
		{"nobody", "relay", `there's no device called "nobody"`},
		{"plug1", "nothing", `plug1 has no switch called "nothing"`},
	} {
		if err := adapter.SetSwitch(c.device, c.object, true); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s/%s: got %v, want one containing %q", c.device, c.object, err, c.wantErr)
		}
	}

	var done chan func()
	c.onPluginGoroutine(func() { done = p.Stop() })
	<-done

	// With the plugin stopped there's nothing to send a command with, and the error says so.
	if err := adapter.SetSwitch("plug1", "relay", false); err == nil {
		t.Error("expected an error once the plugin has stopped")
	}
}

func TestRegistry_SetSwitchThroughTheEntityRefusesUnknownState(t *testing.T) {
	f := newFixture(t)
	f.recordPublishes(nil)
	f.registry.HandleConnected("plug1", "10.0.0.5")
	f.discover("plug1", "switch", "relay", switchConfig("plug1", "relay", "Relay"))

	if err := f.registry.setSwitch("plug1", "relay", device.SwitchStateUnknown); err == nil {
		t.Fatal("expected an error")
	}
}
