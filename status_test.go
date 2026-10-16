package esphome

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/data"
)

// A device block as ESPHome announces it: the identifier is the MAC address.
const (
	statusTemperatureConfig = `{"name":"Temperature","dev_cla":"temperature","unit_of_meas":"°C","stat_t":"garagedoor1/sensor/temperature/state","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x9ffc2670)","mdl":"esp32","mf":"Espressif"}}`
	statusDoorConfig        = `{"name":"Door Position","dev_cla":"garage_door","stat_t":"garagedoor1/binary_sensor/door_position/state","dev":{"ids":"20500de464ec","name":"GarageDoor1"}}`
)

func TestFormatMAC(t *testing.T) {
	for in, want := range map[string]string{
		"20500de464ec":  "20:50:0d:e4:64:ec",
		"20500DE464EC":  "20:50:0d:e4:64:ec",
		"20500de464e":   "",
		"20500de464ecc": "",
		"not-a-mac-adr": "",
		"20500de464eg":  "",
		"":              "",
	} {
		if got := formatMAC(in); got != want {
			t.Errorf("formatMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeviceStatus_ADeviceThatHasNeverConnected(t *testing.T) {
	f := newFixture(t)

	got := f.registry.deviceStatus("garage1")

	want := data.DeviceStatus{Name: "garage1", Label: "Garage 1"} // the label comes from its display name
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if got := f.registry.deviceStatus("unconfigured"); got.Label != "unconfigured" {
		t.Errorf("a device with no display name should be labelled with its name, got %q", got.Label)
	}
}

func TestDeviceStatus_ConnectedWithWhatItAnnounced(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}

	f.advance(time.Minute)
	f.registry.HandleConnected("garagedoor1", "10.47.7.17")
	f.advance(time.Second)
	f.discover("garagedoor1", "sensor", "temperature", statusTemperatureConfig)
	f.state("garagedoor1", "garagedoor1/sensor/temperature/state", "23.8")
	f.discover("garagedoor1", "binary_sensor", "door_position", statusDoorConfig)

	got := f.registry.deviceStatus("garagedoor1")

	want := data.DeviceStatus{
		Name: "garagedoor1", Label: "GarageDoor1", // the name it announces, as there's no display name
		Connected: true, ConnectedSince: f.clock.Add(-time.Second), Address: "10.47.7.17",
		Firmware: "2026.8.0 (config hash 0x9ffc2670)", Model: "esp32", Manufacturer: "Espressif",
		MAC: "20:50:0d:e4:64:ec", EntityCount: 2, LastMessageTime: f.clock,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestDeviceStatus_ADisconnectedDeviceSaysWhenItWentAway(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("garagedoor1", "10.47.7.17")
	f.advance(time.Hour)
	f.registry.HandleDisconnected("garagedoor1")

	got := f.registry.deviceStatus("garagedoor1")

	if got.Connected || !got.DisconnectedSince.Equal(f.clock) || !got.ConnectedSince.IsZero() {
		t.Fatalf("unexpected %+v", got)
	}

	// It keeps the address it last connected from, which is where to look for it.
	if got.Address != "10.47.7.17" {
		t.Errorf("lost the address: %q", got.Address)
	}

	// Back again: the earlier disconnection is history, not part of its status now.
	f.advance(time.Minute)
	f.registry.HandleConnected("garagedoor1", "10.47.7.18")

	got = f.registry.deviceStatus("garagedoor1")
	if !got.Connected || !got.DisconnectedSince.IsZero() || got.Address != "10.47.7.18" || !got.ConnectedSince.Equal(f.clock) {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestDeviceStatus_AnIdentifierThatIsNotAMACIsNotShownAsOne(t *testing.T) {
	f := newFixture(t)
	f.discover("garagedoor1", "sensor", "temperature",
		`{"name":"T","dev_cla":"temperature","stat_t":"garagedoor1/sensor/temperature/state","dev":{"ids":"some-other-identifier"}}`)

	if got := f.registry.deviceStatus("garagedoor1"); got.MAC != "" {
		t.Errorf("got %q", got.MAC)
	}
}

func TestDeviceStatus_WhatADeviceAnnouncedIsNotLostWhenALaterMessageLeavesItOut(t *testing.T) {
	f := newFixture(t)
	f.discover("garagedoor1", "sensor", "temperature", statusTemperatureConfig)

	// The door's device block has no model, firmware or manufacturer.
	f.discover("garagedoor1", "binary_sensor", "door_position", statusDoorConfig)

	got := f.registry.deviceStatus("garagedoor1")
	if got.Model != "esp32" || got.Firmware == "" || got.Manufacturer != "Espressif" {
		t.Fatalf("lost what the first message said: %+v", got)
	}
}

func TestDeviceStatus_AnIgnoredEntityStillTellsUsAboutTheDevice(t *testing.T) {
	f := newFixture(t)
	f.discover("garagedoor1", "light", "status_led",
		`{"name":"LED","stat_t":"garagedoor1/light/status_led/state","dev":{"ids":"20500de464ec","name":"GarageDoor1","mdl":"esp32"}}`)

	got := f.registry.deviceStatus("garagedoor1")
	if got.Model != "esp32" || got.EntityCount != 0 {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestEntityViews(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("garagedoor1", "10.47.7.17")
	f.discover("garagedoor1", "sensor", "temperature", statusTemperatureConfig)
	f.state("garagedoor1", "garagedoor1/sensor/temperature/state", "23.8")
	f.discover("garagedoor1", "binary_sensor", "door_position", statusDoorConfig)
	f.state("garagedoor1", "garagedoor1/binary_sensor/door_position/state", "OFF")
	f.advance(time.Minute)
	f.state("garagedoor1", "garagedoor1/binary_sensor/door_position/state", "ON")

	thermometers := f.registry.thermometerViews()
	if len(thermometers) != 1 {
		t.Fatalf("got %+v", thermometers)
	}

	thermometer := thermometers[0]
	if thermometer.Name != "GarageDoor1 Temperature" || thermometer.DeviceName != "garagedoor1" || !thermometer.DeviceConnected ||
		!thermometer.HasData || thermometer.Temperature != 23.8 || thermometer.HasHumidity {
		t.Errorf("unexpected thermometer %+v", thermometer)
	}

	doors := f.registry.doorSensorViews()
	if len(doors) != 1 {
		t.Fatalf("got %+v", doors)
	}

	door := doors[0]
	if door.Name != "GarageDoor1 Door Position" || door.DeviceName != "garagedoor1" || !door.DeviceConnected || !door.HasData ||
		door.Position != "Open" || door.PositionClass != "open" || door.LastChangeTime.IsZero() {
		t.Errorf("unexpected door %+v", door)
	}

	// Offline, it still shows the last position it reported, flagged as that.
	f.registry.HandleDisconnected("garagedoor1")

	if door := f.registry.doorSensorViews()[0]; door.DeviceConnected || door.Position != "Open" || door.OfflineSince.IsZero() {
		t.Errorf("unexpected door after disconnecting %+v", door)
	}

	if thermometer := f.registry.thermometerViews()[0]; thermometer.DeviceConnected || thermometer.Temperature != 23.8 {
		t.Errorf("unexpected thermometer after disconnecting %+v", thermometer)
	}
}

func TestEntityViews_ADoorThatHasntReportedYet(t *testing.T) {
	f := newFixture(t)
	f.discover("garagedoor1", "binary_sensor", "door_position", statusDoorConfig)

	door := f.registry.doorSensorViews()[0]
	if door.HasData || door.Position != "Unknown" || door.PositionClass != "unknown" {
		t.Fatalf("unexpected door %+v", door)
	}
}

func TestEntityViews_AreOrderedAndOnlyIncludeWhatIsRegistered(t *testing.T) {
	f := newFixture(t)

	for _, device := range []string{"zeta", "alpha"} {
		f.discover(device, "sensor", "b_temperature",
			`{"name":"B","dev_cla":"temperature","stat_t":"`+device+`/sensor/b/state"}`)
		f.discover(device, "sensor", "a_temperature",
			`{"name":"A","dev_cla":"temperature","stat_t":"`+device+`/sensor/a/state"}`)
	}

	// One that couldn't be registered isn't shown, because it isn't there.
	f.container.registerErr = errTest
	f.discover("alpha", "sensor", "c_temperature", `{"name":"C","dev_cla":"temperature","stat_t":"alpha/sensor/c/state"}`)

	var got []string
	for _, v := range f.registry.thermometerViews() {
		got = append(got, v.DeviceName+"/"+v.Name)
	}

	want := []string{"alpha/alpha A", "alpha/alpha B", "zeta/zeta A", "zeta/zeta B"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The plugin's status: the broker, the devices in the order configured, and the entities of every device in one
// list.
func TestGetStatusAndEntities(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = "127.0.0.1:1883"
	config.CaptureMessages = true
	config.AddDevice(DeviceConfig{Name: "garagedoor2", Password: "b"})
	config.AddDevice(DeviceConfig{Name: "garagedoor1", Password: "a", DisplayName: "Double Garage Door"})
	config.AddDevice(DeviceConfig{Name: "silent", Password: "c"})

	p := NewPlugin(config).(*plugin)
	p.registry = newRegistry(&fakeContainer{}, "homeassistant", config.displayNames())
	f := &fixture{t: t, container: p.registry.container.(*fakeContainer), registry: p.registry,
		clock: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	p.registry.now = func() time.Time { return f.clock }
	p.registry.logf = func(string, ...any) {}

	p.brokerProblem = "unable to listen on 127.0.0.1:1883: address already in use"

	for _, device := range []string{"garagedoor2", "garagedoor1"} {
		f.registry.HandleConnected(device, "10.0.0.1")
		f.discover(device, "binary_sensor", "door", `{"name":"Door","dev_cla":"door","stat_t":"`+device+`/binary_sensor/door/state"}`)
		f.discover(device, "sensor", "temp", `{"name":"Temp","dev_cla":"temperature","stat_t":"`+device+`/sensor/temp/state"}`)
	}

	f.advance(time.Minute)
	f.state("garagedoor1", "garagedoor1/sensor/temp/state", "20")

	got := p.getStatusAndEntities()

	status := got.Status
	if status.BrokerRunning || status.BrokerProblem == "" || status.ListenAddress != "127.0.0.1:1883" ||
		status.DiscoveryPrefix != "homeassistant" || !status.CaptureMessages {
		t.Errorf("unexpected broker status %+v", status)
	}

	var devices []string
	for _, d := range status.Devices {
		devices = append(devices, d.Name)
	}

	if strings.Join(devices, ",") != "garagedoor2,garagedoor1,silent" {
		t.Errorf("devices should be in the order configured, got %v", devices)
	}

	if status.ConnectedDevices != 2 || status.ThermometerCount != 2 || status.DoorSensorCount != 2 ||
		!status.LastMessageTime.Equal(f.clock) {
		t.Errorf("unexpected counts or last message in %+v", status)
	}

	// One list, by device, with a device's thermometers before its door sensors.
	var order []string

	for _, entity := range got.Entities {
		switch e := entity.(type) {
		case *data.ThermometerView:
			order = append(order, e.DeviceName+":thermometer")
		case *data.DoorSensorView:
			order = append(order, e.DeviceName+":door")
		default:
			t.Fatalf("unexpected entity %T", entity)
		}
	}

	want := "garagedoor1:thermometer,garagedoor1:door,garagedoor2:thermometer,garagedoor2:door"
	if strings.Join(order, ",") != want {
		t.Errorf("got %v, want %s", order, want)
	}
}

func TestStart_RecordsWhyTheBrokerIsNotRunning(t *testing.T) {
	for name, c := range map[string]struct {
		devices []DeviceConfig
		address string
		want    string
	}{
		"no devices":        {nil, "127.0.0.1:0", "no devices are configured"},
		"duplicate devices": {[]DeviceConfig{{Name: "a", Password: "x"}, {Name: "a", Password: "y"}}, "127.0.0.1:0", "more than once"},
		"invalid name":      {[]DeviceConfig{{Name: "a/b", Password: "x"}}, "127.0.0.1:0", "invalid device name"},
		"cannot listen":     {[]DeviceConfig{{Name: "a", Password: "x"}}, "not an address", "unable to listen on not an address"},
	} {
		config := NewPluginConfig()
		config.ListenAddress = c.address
		config.Devices = c.devices

		p, ctr := startPlugin(t, config)

		ctr.onPluginGoroutine(func() {
			status := p.getStatusAndEntities().Status

			if status.BrokerRunning || !strings.Contains(status.BrokerProblem, c.want) {
				t.Errorf("%s: got running=%v problem=%q, want it to mention %q", name, status.BrokerRunning, status.BrokerProblem, c.want)
			}
		})
	}
}

func TestStart_ARunningBrokerReportsItsAddress(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "garage1", Password: "secret1"})

	p, c := startPlugin(t, config)

	c.onPluginGoroutine(func() {
		status := p.getStatusAndEntities().Status

		if !status.BrokerRunning || status.BrokerProblem != "" || status.ListenAddress != config.ListenAddress ||
			len(status.Devices) != 1 || status.Devices[0].Connected {
			t.Errorf("unexpected status %+v", status)
		}
	})

	var done chan func()
	c.onPluginGoroutine(func() { done = p.Stop() })
	<-done
}

func TestInit_RegistersThePageAndTheRenderers(t *testing.T) {
	container := &fakeContainer{}
	p := NewPlugin(NewPluginConfig()).(*plugin)
	p.Init(container)

	page, ok := container.routes[""]
	if !ok || page.MenuLabel != "ESPHome" || page.MenuIcon != "" || !page.SupportsXsrfValidation {
		t.Fatalf("unexpected page route %+v", page)
	}

	got := map[reflect.Type]bool{}
	for _, entityType := range container.rendererTypes {
		got[entityType] = true
	}

	for _, want := range []reflect.Type{
		reflect.TypeFor[data.PluginStatus](), reflect.TypeFor[data.ThermometerView](), reflect.TypeFor[data.DoorSensorView](),
		reflect.TypeFor[data.SwitchView](), reflect.TypeFor[data.PowerView](),
	} {
		if !got[want] {
			t.Errorf("no renderer registered for %v", want)
		}
	}

	if container.jsonRoutes["switch"] == nil {
		t.Error("no route registered to turn a switch on and off")
	}

	if len(container.rendererTypes) != 5 || container.staticDir != "static" {
		t.Errorf("unexpected registrations: %v, static %q", container.rendererTypes, container.staticDir)
	}
}

func TestEntityStoreAdapter_RunsOnThePluginGoroutine(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t) // never the default, which would take the real MQTT port
	config.AddDevice(DeviceConfig{Name: "garage1", Password: "secret1"})

	p, c := startPlugin(t, config)

	// Not on the plugin goroutine, as an HTTP request wouldn't be.
	got, err := (&entityStoreAdapter{parent: p}).GetStatusAndEntities()
	if err != nil || len(got.Status.Devices) != 1 || got.Status.Devices[0].Name != "garage1" {
		t.Fatalf("got %+v, %v", got, err)
	}

	var done chan func()
	c.onPluginGoroutine(func() { done = p.Stop() })
	<-done
}

func TestGetStatusAndEntities_BeforeStartShowsTheConfiguredAddress(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = "127.0.0.1:1884"

	p := NewPlugin(config).(*plugin)
	p.registry = newRegistry(&fakeContainer{}, "homeassistant", nil)

	if got := p.getStatusAndEntities().Status.ListenAddress; got != "127.0.0.1:1884" {
		t.Fatalf("got %q", got)
	}
}
