package esphome

import (
	"testing"

	"github.com/avanha/pmaas-plugin-esphome/data"
	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
	"github.com/avanha/pmaas-spi/device"
)

func TestShortName(t *testing.T) {
	r := newRegistry(&fakeContainer{}, "homeassistant", map[string]string{"garage1": "Left Garage"})

	for _, c := range []struct {
		device   string // the name the device has, which a display name may stand in for
		announce string // what the device calls itself
		entity   string // what the device calls the entity
		fallback string
		want     string
	}{
		// What the device calls it, as it is.
		{"garage1", "GarageDoor1", "Temperature", "Temperature", "Temperature"},
		{"garage1", "GarageDoor1", "Door Position", "Door", "Door Position"},
		{"plug", "Kauf Plug 7981c8", "Power", "Switch", "Power"},

		// The device's name in front of it, which the card has already said.
		{"garage1", "GarageDoor1", "Left Garage Door", "Door", "Door"},
		{"garage1", "GarageDoor1", "left garage door position", "Door", "door position"},
		{"plug", "Kauf Plug 7981c8", "Kauf Plug 7981c8 Relay", "Switch", "Relay"},

		// No name of its own: the device's main entity, so it's called what it is. A Kauf plug's relay is named for the
		// plug, and ESPHome puts the end of the MAC address on the device's name but not on the entity's.
		{"plug", "Kauf Plug 7981c8", "Kauf Plug", "Switch", "Switch"},
		{"plug", "Kauf Plug 7981c8", "kauf plug", "Switch", "Switch"},
		{"plug", "Kauf Plug 7981c8", "Kauf Plug 7981c8", "Switch", "Switch"},
		{"plug", "Kauf Plug 7981c8", "", "Switch", "Switch"},
		{"plug", "Kauf Plug 7981c8", "   ", "Door", "Door"},
		{"garage1", "GarageDoor1", "Left Garage", "Door", "Door"},

		// Named for what the device calls itself, which isn't what it's been given to be called on the page.
		{"garage1", "Kauf Plug 7981c8", "Kauf Plug", "Switch", "Switch"},
		{"garage1", "Kauf Plug 7981c8", "Kauf Plug 7981c8", "Switch", "Switch"},
		{"garage1", "Kauf Plug 7981c8", "Power", "Switch", "Power"},

		// Older Kauf firmware announces no friendly name, so the device is "kauf-plug-7aad7c", and puts the plug's
		// model-style name on every entity: "Kauf Plug Power", and "Kauf Plug" for the relay.
		{"kauf-plug-7aad7c", "kauf-plug-7aad7c", "Kauf Plug", "Switch", "Switch"},
		{"kauf-plug-7aad7c", "kauf-plug-7aad7c", "Kauf Plug Power", "Switch", "Power"},
		{"kauf-plug-7aad7c", "kauf-plug-7aad7c", "Kauf Plug Total Daily Energy", "Switch", "Total Daily Energy"},
		{"kauf-plug-7aad7c", "kauf-plug-7aad7c", "Kauf Plug 7aad7c Power", "Switch", "Power"},
		{"kauf-plug-7aad7c", "kauf-plug-7aad7c", "KAUF-PLUG Voltage", "Switch", "Voltage"},
		// And with a display name on top, the same: what's checked is what the device calls itself as well.
		{"garage1", "kauf-plug-7aad7c", "Kauf Plug Current", "Switch", "Current"},
		{"garage1", "kauf-plug-7aad7c", "Kauf Plug", "Switch", "Switch"},

		// The end of a MAC address is only taken off when it is one: six hex digits.
		{"p", "Plug ABCDEFG", "Plug Power", "Switch", "Plug Power"}, // seven letters, and not hex: no MAC to take off
		{"p", "Plug 12345G", "Plug Power", "Switch", "Plug Power"},  // six, but not all hex
		{"p", "Plug 7aad7c", "Plug Power", "Switch", "Power"},       // six hex digits: it's a MAC, so the device is "Plug"
		{"p", "Plug", "Plug Power", "Switch", "Power"},

		// Only at a word boundary.
		{"p", "Kaufman Plug", "Kauf", "Switch", "Kauf"},
		{"p", "Kauf Plug", "Kauf Plugs", "Switch", "Kauf Plugs"},

		// A name that is only the device's name and a space has nothing left, so it's called what it is.
		{"p", "Plug", "Plug ", "Switch", "Switch"},
	} {
		e := hadiscovery.Entity{Name: c.entity, Device: hadiscovery.Device{Name: c.announce}}

		if got := r.shortName(c.device, e, c.fallback); got != c.want {
			t.Errorf("device %q (%q), entity %q: got %q, want %q", c.device, c.announce, c.entity, got, c.want)
		}
	}
}

// Both names are the same for as long as the entity is: nothing is stored, so renaming something on the device
// changes both.
func TestShortName_FollowsARenameAndTheFullNameIsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	view := f.registry.switchViews()[0]
	if view.Name != "Garage 1 Plug" || view.ShortName != "Plug" {
		t.Fatalf("unexpected view %+v", view)
	}

	// What the core is told is called by the full name.
	if got := f.container.registrations[0].name; got != "Garage 1 Plug" {
		t.Fatalf("registered as %q", got)
	}

	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Heater"))

	if view := f.registry.switchViews()[0]; view.Name != "Garage 1 Heater" || view.ShortName != "Heater" {
		t.Fatalf("unexpected view after the rename %+v", view)
	}
}

// Found on a real Kauf plug: its relay is called "Kauf Plug" on a device called "Kauf Plug 7981c8".
func TestShortName_ThePlugsRelayIsASwitchAndItsPowerIsPower(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.HandleConnected("kauf-plug-7981c8", "10.47.6.74")
	f.native("kauf-plug-7981c8",
		&pb.DeviceInfoResponse{Name: "kauf-plug-7981c8", FriendlyName: "Kauf Plug 7981c8"},
		&pb.ListEntitiesSwitchResponse{ObjectId: "kauf_plug", Key: 41191675, Name: "Kauf Plug"},
		&pb.ListEntitiesSensorResponse{ObjectId: "power", Key: 2391494160, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesDoneResponse{})

	view := f.registry.deviceView("kauf-plug-7981c8")

	var names []string

	for _, child := range view.Children {
		switch c := child.(type) {
		case data.SwitchView:
			names = append(names, c.Kind()+": "+c.Name+" / "+c.ShortName)
		case data.PowerView:
			names = append(names, c.Kind()+": "+c.Name+" / "+c.ShortName)
		}
	}

	// The relay is named for the plug, so it has no name of its own, and its full name is just the plug's.
	want := []string{"switch: Kauf Plug 7981c8 / Switch", "power: Kauf Plug 7981c8 Power / Power"}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func TestShortName_EveryKindOfChildHasOne(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "sensor", "temp", `{"name":"Garage 1 Temperature","dev_cla":"temperature","stat_t":"garage1/sensor/temp/state"}`)
	f.discover("garage1", "binary_sensor", "door", `{"name":"","dev_cla":"door","stat_t":"garage1/binary_sensor/door/state"}`)
	f.discover("garage1", "switch", "relay", switchConfig("garage1", "relay", "Garage 1"))
	f.powerSensor("garage1", "power", "power", "W")

	got := map[string]string{}

	for _, child := range f.registry.deviceView("garage1").Children {
		switch c := child.(type) {
		case data.ThermometerView:
			got[c.Kind()] = c.ShortName
		case data.DoorSensorView:
			got[c.Kind()] = c.ShortName
		case data.SwitchView:
			got[c.Kind()] = c.ShortName
		case data.PowerView:
			got[c.Kind()] = c.ShortName
		}
	}

	want := map[string]string{"thermometer": "Temperature", "door": "Door", "switch": "Switch", "power": "Power"}
	for kind, name := range want {
		if got[kind] != name {
			t.Errorf("%s: got %q, want %q (all: %v)", kind, got[kind], name, got)
		}
	}
}

var _ = device.SwitchStateOn

// The listing a real Kauf plug on ESPHome 2023.6.3 (10.47.6.71) sent, which has no friendly name and puts the plug's
// name on every entity.
func TestShortName_AnOldKaufPlugsWholeListing(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.HandleConnected("kauf-plug-7aad7c", "10.47.6.71")
	f.native("kauf-plug-7aad7c",
		&pb.DeviceInfoResponse{Name: "kauf-plug-7aad7c", MacAddress: "70:03:9F:7A:AD:7C", EsphomeVersion: "2023.6.3", Model: "esp01_1m"},
		&pb.ListEntitiesBinarySensorResponse{ObjectId: "kauf_plug_button", Key: 1, Name: "Kauf Plug Button"},
		&pb.ListEntitiesBinarySensorResponse{ObjectId: "kauf_plug_device_in_use", Key: 2, Name: "Kauf Plug Device In Use"},
		&pb.ListEntitiesSensorResponse{ObjectId: "kauf_plug_voltage", Key: 3, Name: "Kauf Plug Voltage", DeviceClass: "voltage", UnitOfMeasurement: "V"},
		&pb.ListEntitiesSensorResponse{ObjectId: "kauf_plug_power", Key: 4, Name: "Kauf Plug Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesSensorResponse{ObjectId: "kauf_plug_uptime", Key: 5, Name: "Kauf Plug Uptime", DeviceClass: "duration", UnitOfMeasurement: "s", DisabledByDefault: true},
		&pb.ListEntitiesSwitchResponse{ObjectId: "kauf_plug", Key: 6, Name: "Kauf Plug"},
		&pb.ListEntitiesSwitchResponse{ObjectId: "kauf_plug_blue_led", Key: 7, Name: "Kauf Plug Blue LED", DisabledByDefault: true},
		&pb.ListEntitiesSwitchResponse{
			ObjectId: "kauf_plug_no_hass", Key: 8, Name: "Kauf Plug No HASS", DisabledByDefault: true,
			EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_CONFIG,
		},
		&pb.ListEntitiesDoneResponse{})

	view := f.registry.deviceView("kauf-plug-7aad7c")

	var got []string

	for _, child := range view.Children {
		switch c := child.(type) {
		case data.SwitchView:
			got = append(got, c.Kind()+": "+c.Name+" | "+c.ShortName)
		case data.PowerView:
			got = append(got, c.Kind()+": "+c.Name+" | "+c.ShortName)
		}
	}

	// The relay is the device itself; the rest are the plug's readings. The LEDs and settings are hidden.
	want := []string{"switch: kauf-plug-7aad7c | Switch", "power: kauf-plug-7aad7c Power | Power"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] || len(view.Children) != 2 {
		t.Fatalf("got %v with %d children, want %v", got, len(view.Children), want)
	}
}
