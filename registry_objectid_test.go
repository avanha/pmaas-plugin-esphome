package esphome

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

// Found on a real Kauf plug (10.47.6.76, ESPHome 2025.8.2): it listed every sensor with an empty object id, so all of
// them shared one entry in the registry, the last to report won, and its voltage (124.3) was shown as its power. This
// is that listing, in the order it came.
func kaufPlugWithNoObjectIds() []proto.Message {
	return []proto.Message{
		&pb.ListEntitiesBinarySensorResponse{Key: 977454165, Name: "Button"},
		&pb.ListEntitiesSensorResponse{Key: 2195749573, Name: "Voltage", DeviceClass: "voltage", UnitOfMeasurement: "V"},
		&pb.ListEntitiesSensorResponse{Key: 294834382, Name: "Total Daily Energy", DeviceClass: "energy", UnitOfMeasurement: "kWh"},
		&pb.ListEntitiesSensorResponse{
			Key: 1324261225, Name: "Uptime", DeviceClass: "duration", UnitOfMeasurement: "s",
			EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_DIAGNOSTIC, DisabledByDefault: true,
		},
		&pb.ListEntitiesSensorResponse{Key: 2037989808, Name: "Current", DeviceClass: "current", UnitOfMeasurement: "A"},
		&pb.ListEntitiesSensorResponse{Key: 2391494160, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesSwitchResponse{Key: 41191675, Name: "Kauf Plug"},
		&pb.ListEntitiesDoneResponse{},
	}
}

// What the plug was reporting at the time, while its own web page said "Power 1.3 W", "Voltage 124.3 V" and
// "Current 0.09 A".
func kaufPlugStates() []proto.Message {
	return []proto.Message{
		&pb.SensorStateResponse{Key: 2195749573, State: 124.31031},  // voltage
		&pb.SensorStateResponse{Key: 294834382, State: 13.396846},   // energy
		&pb.SensorStateResponse{Key: 2037989808, State: 0.09322223}, // current
		&pb.SensorStateResponse{Key: 2391494160, State: 1.3174597},  // power
		&pb.SwitchStateResponse{Key: 41191675, State: true},
	}
}

func TestNativeObjectIds_ADeviceThatSendsNoneKeepsEachSensorsReadingToItself(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.HandleConnected("kauf-plug-f1a91d", "10.47.6.76")
	f.native("kauf-plug-f1a91d", &pb.DeviceInfoResponse{Name: "kauf-plug-f1a91d", FriendlyName: "Kauf Plug f1a91d"})
	f.native("kauf-plug-f1a91d", kaufPlugWithNoObjectIds()...)
	f.native("kauf-plug-f1a91d", kaufPlugStates()...)

	views := f.registry.powerViews()
	if len(views) != 1 {
		t.Fatalf("unexpected views %+v", views)
	}

	power := views[0]

	if !power.HasPower || !power.HasVoltage || !power.HasCurrent || !power.HasEnergy {
		t.Fatalf("every reading should be there: %+v", power)
	}

	if power.Power < 1.31 || power.Power > 1.32 || power.Voltage < 124.3 || power.Voltage > 124.32 ||
		power.Current < 0.0932 || power.Current > 0.0933 || power.Energy < 13.39 || power.Energy > 13.40 {
		t.Fatalf("a reading is on the wrong sensor: %+v", power)
	}

	// The relay has its own, and what's hidden stays hidden.
	switches := f.registry.switchViews()
	if len(switches) != 1 || switches[0].State != "On" || switches[0].ShortName != "Switch" {
		t.Fatalf("unexpected switches %+v", switches)
	}
}

// The id worked out is the one the device would have sent, so an entity is the same whether or not it sends one.
func TestNativeObjectIds_AreWhatTheDeviceWouldHaveSent(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("p", "10.0.0.1")
	f.native("p", kaufPlugWithNoObjectIds()...)

	var got []string
	for _, r := range f.container.registrations {
		got = append(got, r.uniqueData)
	}

	if !strings.Contains(strings.Join(got, ","), "p/kauf_plug") || !strings.Contains(strings.Join(got, ","), "p/_power") {
		t.Fatalf("unexpected registrations %v", got)
	}

	for key, want := range map[uint32]nativeRef{
		2391494160: {"sensor", "power"}, 2195749573: {"sensor", "voltage"}, 294834382: {"sensor", "total_daily_energy"},
		2037989808: {"sensor", "current"}, 41191675: {"switch", "kauf_plug"},
	} {
		if got := f.registry.nodes["p"].native.keys[key]; got != want {
			t.Errorf("key %d is %+v, want %+v", key, got, want)
		}
	}
}

func TestNativeObjectIds_ADeviceThatSendsThemIsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("p", "10.0.0.1")
	f.native("p",
		&pb.ListEntitiesSensorResponse{ObjectId: "kauf_plug_power", Key: 1, Name: "Kauf Plug Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesDoneResponse{})

	// What it said, even where that isn't what the name would give.
	if got := f.registry.nodes["p"].native.keys[1]; got != (nativeRef{"sensor", "kauf_plug_power"}) {
		t.Fatalf("got %+v", got)
	}
}

// Two entities that end up with the same id mustn't share an entry, whatever the reason: the second has its key put
// on the end.
func TestNativeObjectIds_TwoOfTheSameNameStayApart(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("p", "10.0.0.1")
	f.native("p",
		&pb.ListEntitiesSensorResponse{Key: 10, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesSensorResponse{Key: 20, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesSensorResponse{ObjectId: "power", Key: 30, Name: "Other", DeviceClass: "voltage", UnitOfMeasurement: "V"},
		&pb.ListEntitiesSwitchResponse{ObjectId: "power", Key: 40, Name: "Power"}, // a different kind of thing may share one
		&pb.ListEntitiesDoneResponse{})

	keys := f.registry.nodes["p"].native.keys

	if keys[10] != (nativeRef{"sensor", "power"}) || keys[20] != (nativeRef{"sensor", "power_20"}) ||
		keys[30] != (nativeRef{"sensor", "power_30"}) || keys[40] != (nativeRef{"switch", "power"}) {
		t.Fatalf("unexpected keys %+v", keys)
	}

	if len(f.registry.nodes["p"].powerSensors) != 3 || len(f.registry.nodes["p"].switches) != 1 {
		t.Fatalf("they shared an entry: %d sensors, %d switches",
			len(f.registry.nodes["p"].powerSensors), len(f.registry.nodes["p"].switches))
	}
}

// What an entity was called stays what it's called when the device lists it again, even if the order changed.
func TestNativeObjectIds_AreStableWhenTheDeviceListsThemAgainInAnotherOrder(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("p", "10.0.0.1")

	first := &pb.ListEntitiesSensorResponse{Key: 10, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"}
	second := &pb.ListEntitiesSensorResponse{Key: 20, Name: "Power", DeviceClass: "voltage", UnitOfMeasurement: "V"}

	f.native("p", first, second, &pb.ListEntitiesDoneResponse{})
	f.registry.HandleDisconnected("p")
	f.registry.HandleConnected("p", "10.0.0.1")
	f.native("p", second, first, &pb.ListEntitiesDoneResponse{})

	keys := f.registry.nodes["p"].native.keys
	if keys[10] != (nativeRef{"sensor", "power"}) || keys[20] != (nativeRef{"sensor", "power_20"}) {
		t.Fatalf("they swapped: %+v", keys)
	}

	if len(f.container.deregistered) != 0 {
		t.Fatalf("something was dropped and put back: %v", f.container.deregistered)
	}
}

func TestNativeObjectIds_AnEntityWithNothingToGoByStillGetsOne(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("p", "10.0.0.1")
	f.native("p",
		&pb.ListEntitiesSwitchResponse{Key: 7, Name: ""},
		&pb.ListEntitiesSwitchResponse{Key: 8, Name: "!!!"},
		&pb.ListEntitiesDoneResponse{})

	keys := f.registry.nodes["p"].native.keys
	if keys[7] != (nativeRef{"switch", "switch_7"}) || keys[8] != (nativeRef{"switch", "switch_8"}) {
		t.Fatalf("unexpected keys %+v", keys)
	}
}

func TestNativeObjectIds_SlugsAreWhatESPHomeMakes(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("p", "10.0.0.1")
	n := f.registry.nodeFor("p")

	for name, want := range map[string]string{
		"Total Daily Energy":    "total_daily_energy",
		"Kauf Plug":             "kauf_plug",
		"  Blue  LED ":          "blue_led",
		"CO2 (ppm)":             "co2_ppm",
		"Door-Position":         "door_position",
		"Température Intérieur": "temp_rature_int_rieur",
	} {
		if got := f.registry.nativeObjectID(n, "sensor", "", name, 1); got != want {
			t.Errorf("%q: got %q, want %q", name, got, want)
		}
	}
}
