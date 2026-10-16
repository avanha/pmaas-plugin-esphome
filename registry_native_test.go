package esphome

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/environment"
)

// kaufEntities is how a stock Kauf plug describes its entities over the native API.
func kaufEntities() []proto.Message {
	return []proto.Message{
		&pb.ListEntitiesSwitchResponse{ObjectId: "relay", Key: 11, Name: "Relay"},
		&pb.ListEntitiesSensorResponse{ObjectId: "power", Key: 12, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesSensorResponse{ObjectId: "voltage", Key: 13, Name: "Voltage", DeviceClass: "voltage", UnitOfMeasurement: "V"},
		&pb.ListEntitiesSensorResponse{
			ObjectId: "total_daily_energy", Key: 14, Name: "Total Daily Energy", DeviceClass: "energy", UnitOfMeasurement: "Wh",
		},
		&pb.ListEntitiesDoneResponse{},
	}
}

// nativeKaufPlug is a stock Kauf plug connecting, and saying who it is and what it has.
func (f *fixture) nativeKaufPlug(deviceName string) {
	f.registry.HandleConnected(deviceName, "10.0.0.7")
	f.native(deviceName, &pb.DeviceInfoResponse{
		Name: deviceName, FriendlyName: "Kauf Plug", MacAddress: "AA:BB:CC:DD:EE:FF", EsphomeVersion: "2026.9.1",
		Model: "PLF10", Manufacturer: "Kauf",
	})
	f.native(deviceName, kaufEntities()...)
}

func (f *fixture) native(deviceName string, messages ...proto.Message) {
	for _, m := range messages {
		f.registry.HandleNativeMessage(deviceName, m)
	}
}

func TestNative_EntitiesAreRegisteredLikeAnMQTTDevicesAre(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.nativeKaufPlug("plug1")

	got := map[string]string{}
	for _, r := range f.container.registrations {
		got[r.uniqueData] = r.entityType.String() + " " + r.name
	}

	want := map[string]string{
		"plug1/relay":  "device.ISwitch Kauf Plug Relay",
		"plug1/_power": "device.IPowerMonitor Kauf Plug Power",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}

	// What it said about itself is what the status page shows.
	status := f.registry.deviceStatus("plug1")
	if status.Model != "PLF10" || status.Manufacturer != "Kauf" || status.Firmware != "2026.9.1" ||
		status.MAC != "aa:bb:cc:dd:ee:ff" || status.Label != "Kauf Plug" || status.Address != "10.0.0.7" ||
		!status.Connected || status.EntityCount != 2 {
		t.Fatalf("unexpected status %+v", status)
	}
}

func TestNative_StatesReachTheEntitiesByKey(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.nativeKaufPlug("plug1")

	f.advance(time.Second)
	f.native("plug1",
		&pb.SwitchStateResponse{Key: 11, State: false},
		&pb.SensorStateResponse{Key: 12, State: 1512.5},
		&pb.SensorStateResponse{Key: 13, State: 121.2},
		&pb.SensorStateResponse{Key: 14, State: 2500}) // Wh

	relay := f.switchStub().GetSwitchData()
	if !relay.HasData || relay.State != device.SwitchStateOff || relay.Connectivity != environment.ConnectivityOnline {
		t.Fatalf("unexpected switch %+v", relay)
	}

	power := lastPowerState(t, f.container)
	if !power.HasPower || power.Power != 1512.5 || !power.HasVoltage || power.Voltage != float64(float32(121.2)) ||
		!power.HasEnergy || power.Energy != 2.5 || power.HasCurrent {
		t.Fatalf("unexpected power %+v", power)
	}

	f.native("plug1", &pb.SwitchStateResponse{Key: 11, State: true})

	change := f.container.lastBroadcast(t).event.(device.SwitchStateChangedEvent)
	if change.OldState != device.SwitchStateOff || change.NewState != device.SwitchStateOn {
		t.Fatalf("unexpected change %+v", change)
	}
}

func TestNative_AStateWithNoValueOrForAnUnknownKeyIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.nativeKaufPlug("plug1")
	before := len(f.container.broadcasts)

	f.native("plug1",
		&pb.SwitchStateResponse{Key: 11, State: true, MissingState: true},
		&pb.SensorStateResponse{Key: 12, State: 5, MissingState: true},
		&pb.SwitchStateResponse{Key: 999, State: true},
		&pb.SensorStateResponse{Key: 999, State: 5},
		&pb.BinarySensorStateResponse{Key: 999, State: true},
		// The right key for the wrong kind of thing.
		&pb.SwitchStateResponse{Key: 12, State: true},
		&pb.SensorStateResponse{Key: 11, State: 5})

	if len(f.container.broadcasts) != before {
		t.Fatalf("something was taken in: %+v", f.container.broadcasts[before:])
	}
}

func TestNative_ADoorAndATemperatureSensorAreTrackedToo(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.HandleConnected("door1", "10.0.0.8")
	f.native("door1",
		&pb.DeviceInfoResponse{Name: "door1", FriendlyName: "Garage"},
		&pb.ListEntitiesBinarySensorResponse{ObjectId: "door", Key: 1, Name: "Door", DeviceClass: "garage_door"},
		&pb.ListEntitiesBinarySensorResponse{ObjectId: "motion", Key: 2, Name: "Motion", DeviceClass: "motion"},
		&pb.ListEntitiesSensorResponse{ObjectId: "temp", Key: 3, Name: "Temperature", DeviceClass: "temperature", UnitOfMeasurement: "°F"},
		&pb.ListEntitiesSensorResponse{ObjectId: "hum", Key: 4, Name: "Humidity", DeviceClass: "humidity", UnitOfMeasurement: "%"},
		&pb.ListEntitiesDoneResponse{})

	f.native("door1",
		&pb.BinarySensorStateResponse{Key: 1, State: false},
		&pb.SensorStateResponse{Key: 3, State: 68},
		&pb.SensorStateResponse{Key: 4, State: 40})

	var doorStub device.IDoorSensor

	var thermometerStub environment.IWirelessThermometer

	for _, reg := range f.container.registrations {
		stub, _ := reg.stubFactory()

		switch s := stub.(type) {
		case device.IDoorSensor:
			doorStub = s
		case environment.IWirelessThermometer:
			thermometerStub = s
		}
	}

	if doorStub == nil || thermometerStub == nil || len(f.container.registrations) != 2 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	if got := doorStub.GetDoorSensorData(); got.Position != device.DoorPositionClosed || got.Name != "Garage Door" {
		t.Fatalf("unexpected door %+v", got)
	}

	f.native("door1", &pb.BinarySensorStateResponse{Key: 1, State: true})

	if got := doorStub.GetDoorSensorData().Position; got != device.DoorPositionOpen {
		t.Fatalf("on means open, got %v", got)
	}

	th := thermometerStub.GetWirelessThermometerData()
	if th.SensorData.Temperature < 19.9 || th.SensorData.Temperature > 20.1 || th.SensorData.Humidity != 40 {
		t.Fatalf("unexpected thermometer %+v", th) // 68 F is 20 C
	}
}

func TestNative_WhatItDoesNotTrackIsSummarisedInOneLineAndDoesNotRegister(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1",
		&pb.ListEntitiesLightResponse{ObjectId: "led", Key: 1, Name: "Status LED"},
		&pb.ListEntitiesTextSensorResponse{ObjectId: "ip", Key: 2, Name: "IP Address"},
		&pb.ListEntitiesNumberResponse{ObjectId: "n1", Key: 20, Name: "N1"},
		&pb.ListEntitiesNumberResponse{ObjectId: "n2", Key: 21, Name: "N2"},
		&pb.ListEntitiesSensorResponse{ObjectId: "rssi", Key: 3, Name: "WiFi Signal", DeviceClass: "signal_strength"},
		&pb.ListEntitiesBinarySensorResponse{ObjectId: "status", Key: 4, Name: "Status"},
		&pb.ListEntitiesSwitchResponse{ObjectId: "relay", Key: 5, Name: "Relay"},
		&pb.ListEntitiesSwitchResponse{ObjectId: "led2", Key: 6, Name: "LED", EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_CONFIG},
		&pb.ListEntitiesDoneResponse{})

	if len(f.container.registrations) != 1 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	// The connection, and one line for everything the device listed.
	if len(f.logs) != 2 {
		t.Fatalf("expected a single summary line, got %v", f.logs)
	}

	want := "plug1: tracking 1 entities, hiding 1 marked as configuration or disabled, ignoring 6 it has no use for " +
		"(1 binary_sensor, 1 light, 2 number, 1 sensor, 1 textsensor)"
	if f.logs[1] != want {
		t.Fatalf("got  %q\nwant %q", f.logs[1], want)
	}

	// A state for any of them has nowhere to go.
	f.native("plug1", &pb.SensorStateResponse{Key: 3, State: -60}, &pb.BinarySensorStateResponse{Key: 4, State: true})
}

// A device that lists the same things again on reconnecting gets the same line again, and not one that has grown.
func TestNative_TheSummaryIsPerListingNotCumulative(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("plug1", "10.0.0.7")

	listing := []proto.Message{
		&pb.ListEntitiesNumberResponse{ObjectId: "n1", Key: 20, Name: "N1"},
		&pb.ListEntitiesSwitchResponse{ObjectId: "relay", Key: 5, Name: "Relay"},
		&pb.ListEntitiesSwitchResponse{ObjectId: "led", Key: 6, Name: "LED", EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_CONFIG},
		&pb.ListEntitiesDoneResponse{},
	}

	f.native("plug1", listing...)
	f.registry.HandleDisconnected("plug1")
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1", listing...)

	summaries := []string{}

	for _, l := range f.logs {
		if strings.Contains(l, "tracking") {
			summaries = append(summaries, l)
		}
	}

	if len(summaries) != 2 || summaries[0] != summaries[1] ||
		summaries[0] != "plug1: tracking 1 entities, hiding 1 marked as configuration or disabled, ignoring 1 it has no use for (1 number)" {
		t.Fatalf("unexpected summaries %v", summaries)
	}
}

func TestNative_MessagesThatAreNotAboutEntitiesAreIgnored(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("plug1", "10.0.0.7")

	f.native("plug1", &pb.PingResponse{}, &pb.HelloResponse{}, &pb.SubscribeLogsResponse{Message: []byte("hi")})

	if len(f.logs) != 1 || len(f.container.registrations) != 0 { // just the connection itself
		t.Fatalf("logs %v, registrations %+v", f.logs, f.container.registrations)
	}
}

// A device describes all its entities each time it's connected to, so what it left out has been removed.
func TestNative_ReconnectingDropsWhatTheDeviceNoLongerHas(t *testing.T) {
	f := newFixture(t)
	f.nativeKaufPlug("plug1")
	f.registry.HandleDisconnected("plug1")

	// Back, with no relay, and no energy.
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1",
		&pb.ListEntitiesSensorResponse{ObjectId: "power", Key: 12, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesSensorResponse{ObjectId: "voltage", Key: 13, Name: "Voltage", DeviceClass: "voltage", UnitOfMeasurement: "V"},
		&pb.ListEntitiesDoneResponse{})

	if want := []string{"esphome_plug1/relay"}; !reflect.DeepEqual(f.container.deregistered, want) {
		t.Fatalf("deregistered %v, want %v", f.container.deregistered, want)
	}

	// The power monitor stays, minus the energy reading.
	f.native("plug1", &pb.SensorStateResponse{Key: 12, State: 5}, &pb.SensorStateResponse{Key: 14, State: 9999})

	if got := lastPowerState(t, f.container); !got.HasPower || got.HasEnergy {
		t.Fatalf("unexpected power %+v", got)
	}
}

func TestNative_ReconnectingKeepsEntitiesAndTheirState(t *testing.T) {
	f := newFixture(t)
	f.nativeKaufPlug("plug1")
	f.native("plug1", &pb.SwitchStateResponse{Key: 11, State: true})
	f.registry.HandleDisconnected("plug1")

	if got := f.switchStub().GetSwitchData(); got.Connectivity != environment.ConnectivityOffline || got.State != device.SwitchStateOn {
		t.Fatalf("expected offline with the last state kept: %+v", got)
	}

	registered := len(f.container.registrations)
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1", kaufEntities()...)

	if len(f.container.registrations) != registered || len(f.container.deregistered) != 0 {
		t.Fatal("reconnecting re-registered something")
	}

	f.native("plug1", &pb.SwitchStateResponse{Key: 11, State: false})

	if got := f.switchStub().GetSwitchData(); got.Connectivity != environment.ConnectivityOnline || got.State != device.SwitchStateOff {
		t.Fatalf("unexpected switch %+v", got)
	}
}

// If a device renumbers its entities (a reflash can), states must follow the new numbers.
func TestNative_AKeyThatChangesIsFollowed(t *testing.T) {
	f := newFixture(t)
	f.nativeKaufPlug("plug1")

	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1",
		&pb.ListEntitiesSwitchResponse{ObjectId: "relay", Key: 77, Name: "Relay"},
		&pb.ListEntitiesDoneResponse{})
	f.native("plug1", &pb.SwitchStateResponse{Key: 77, State: true})

	if got := f.switchStub().GetSwitchData().State; got != device.SwitchStateOn {
		t.Fatalf("got %v", got)
	}

	recorded := f.recordNativeSends(nil)
	f.registry.HandleConnected("plug1", "10.0.0.7")

	if err := f.switchStub().SetState(device.SwitchStateOff); err != nil {
		t.Fatal(err)
	}

	if len(*recorded) != 1 || (*recorded)[0].message.(*pb.SwitchCommandRequest).Key != 77 {
		t.Fatalf("the command went to the old key: %+v", *recorded)
	}
}

type nativeSend struct {
	device  string
	message proto.Message
}

func (f *fixture) recordNativeSends(err error) *[]nativeSend {
	var sent []nativeSend

	f.registry.nativeSend = func(deviceName string, message proto.Message) error {
		if err != nil {
			return err
		}

		sent = append(sent, nativeSend{deviceName, message})

		return nil
	}

	return &sent
}

func TestNative_ASwitchIsCommandedByKeyAndTheStateComesBackFromTheDevice(t *testing.T) {
	f := newFixture(t)
	sent := f.recordNativeSends(nil)
	f.nativeKaufPlug("plug1")
	f.native("plug1", &pb.SwitchStateResponse{Key: 11, State: false})
	broadcasts := len(f.container.broadcasts)

	if err := f.switchStub().SetState(device.SwitchStateOn); err != nil {
		t.Fatal(err)
	}

	if len(*sent) != 1 || (*sent)[0].device != "plug1" {
		t.Fatalf("sent %+v", *sent)
	}

	command := (*sent)[0].message.(*pb.SwitchCommandRequest)
	if command.Key != 11 || !command.State {
		t.Fatalf("unexpected command %+v", command)
	}

	// Asking doesn't change it: the device does that, by saying so.
	if got := f.switchStub().GetSwitchData().State; got != device.SwitchStateOff || len(f.container.broadcasts) != broadcasts {
		t.Fatal("the request changed the state")
	}

	if err := f.switchStub().SetState(device.SwitchStateOff); err != nil {
		t.Fatal(err)
	}

	if command := (*sent)[1].message.(*pb.SwitchCommandRequest); command.State {
		t.Fatalf("unexpected command %+v", command)
	}
}

func TestNative_ASwitchRefusesWhatItCannotSend(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(f *fixture)
		state   device.SwitchState
		wantErr string
		is      error
	}{
		{"offline", func(f *fixture) { f.registry.HandleDisconnected("plug1") }, device.SwitchStateOn, "the device is offline", nil},
		{"unknown", func(f *fixture) {}, device.SwitchStateUnknown, "a switch can only be set to on or off", device.ErrInvalidSwitchState},
		{"the send fails", func(f *fixture) { f.recordNativeSends(errTest) }, device.SwitchStateOn, "nope", errTest},
		{"no native API", func(f *fixture) { f.registry.nativeSend = nil }, device.SwitchStateOn, "native API isn't available", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			sent := f.recordNativeSends(nil)
			f.nativeKaufPlug("plug1")
			c.setup(f)

			err := f.switchStub().SetState(c.state)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %v, want one containing %q", err, c.wantErr)
			}

			if c.is != nil && !errors.Is(err, c.is) {
				t.Fatalf("%v should wrap %v", err, c.is)
			}

			if c.name != "the send fails" && c.name != "no native API" && len(*sent) != 0 {
				t.Fatalf("something was sent: %+v", *sent)
			}
		})
	}
}

func TestNative_ASwitchOnAnMQTTDeviceStillPublishes(t *testing.T) {
	f := newFixture(t)
	published := f.recordPublishes(nil)
	sent := f.recordNativeSends(nil)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.discover("garage1", "switch", "plug", switchConfig("garage1", "plug", "Plug"))

	if err := f.switchStub().SetState(device.SwitchStateOn); err != nil {
		t.Fatal(err)
	}

	if len(*published) != 1 || len(*sent) != 0 {
		t.Fatalf("published %+v, native %+v", *published, *sent)
	}
}

func TestNative_TheDeviceNameFromDeviceInfoRenamesWhatWasRegisteredBeforeIt(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.HandleConnected("plug1", "10.0.0.7")

	// Entities before the device's info: the name it calls itself isn't known yet.
	f.native("plug1",
		&pb.ListEntitiesSwitchResponse{ObjectId: "relay", Key: 11, Name: "Relay"},
		&pb.ListEntitiesDoneResponse{})
	f.native("plug1", &pb.DeviceInfoResponse{Name: "plug1", FriendlyName: "Heater"})

	if got := f.switchStub().GetSwitchData().Name; got != "Heater Relay" {
		t.Fatalf("got %q", got)
	}
}

func TestNative_DeviceInfoWithNoFriendlyNameUsesTheName(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1", &pb.DeviceInfoResponse{Name: "kauf-plug-1", MacAddress: "AABBCCDDEEFF"})

	if status := f.registry.deviceStatus("plug1"); status.Label != "kauf-plug-1" || status.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("unexpected status %+v", status)
	}
}

func TestNative_NothingIsTakenInOnceTheRegistryHasStopped(t *testing.T) {
	f := newFixture(t)
	f.registry.Shutdown()
	f.native("plug1", &pb.ListEntitiesSwitchResponse{ObjectId: "relay", Key: 1, Name: "Relay"})

	if len(f.container.registrations) != 0 {
		t.Fatal("registered after shutdown")
	}
}

// ESPHome derives an entity's object id from its name, so a switch and a sensor both called "Power" have the same
// one. They're different entities, and a state belongs to the one its key names.
func TestNative_ASwitchAndASensorWithTheSameObjectIdAreKeptApart(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1",
		&pb.ListEntitiesSwitchResponse{ObjectId: "power", Key: 21, Name: "Power"},
		&pb.ListEntitiesSensorResponse{ObjectId: "power", Key: 22, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W"},
		&pb.ListEntitiesDoneResponse{})

	if len(f.container.registrations) != 2 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	f.native("plug1", &pb.SwitchStateResponse{Key: 21, State: true})
	f.native("plug1", &pb.SensorStateResponse{Key: 22, State: 40})

	if got := f.switchStub().GetSwitchData(); got.State != device.SwitchStateOn {
		t.Fatalf("unexpected switch %+v", got)
	}

	if got := lastPowerState(t, f.container); got.Power != 40 {
		t.Fatalf("unexpected power %+v", got)
	}

	// A state of the wrong kind for a key is a device bug, and must not land on the other entity.
	before := len(f.container.broadcasts)
	f.native("plug1", &pb.SensorStateResponse{Key: 21, State: 99})
	f.native("plug1", &pb.SwitchStateResponse{Key: 22, State: false})

	if len(f.container.broadcasts) != before {
		t.Fatalf("a state reached the wrong entity: %+v", f.container.broadcasts[before:])
	}
}

// Found on a real Kauf plug: besides its relay it lists four switches that are its own settings (the LEDs, "No
// HASS", "Early Publish"), marked as configuration and disabled by default. They aren't switches to offer.
func TestNative_ConfigurationAndDisabledEntitiesAreNotEntities(t *testing.T) {
	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1",
		&pb.ListEntitiesSwitchResponse{ObjectId: "kauf_plug", Key: 1, Name: "Kauf Plug"},
		&pb.ListEntitiesSwitchResponse{
			ObjectId: "blue_led", Key: 2, Name: "Blue LED", EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_CONFIG, DisabledByDefault: true,
		},
		&pb.ListEntitiesSwitchResponse{ObjectId: "no_hass", Key: 3, Name: "No HASS", EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_CONFIG},
		&pb.ListEntitiesSwitchResponse{ObjectId: "quiet", Key: 4, Name: "Quiet", DisabledByDefault: true},
		&pb.ListEntitiesSensorResponse{
			ObjectId: "power", Key: 5, Name: "Power", DeviceClass: "power", UnitOfMeasurement: "W",
			EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_DIAGNOSTIC,
		},
		&pb.ListEntitiesSensorResponse{ObjectId: "voltage", Key: 6, Name: "Voltage", DeviceClass: "voltage", UnitOfMeasurement: "V"},
		&pb.ListEntitiesBinarySensorResponse{
			ObjectId: "door", Key: 7, Name: "Door", DeviceClass: "door", EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_DIAGNOSTIC,
		},
		&pb.ListEntitiesDoneResponse{})

	// The relay, and a power monitor for the voltage: nothing else.
	got := map[string]bool{}
	for _, r := range f.container.registrations {
		got[r.uniqueData] = true
	}

	if len(got) != 2 || !got["plug1/kauf_plug"] || !got["plug1/_power"] {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	// Nothing is said about them but a count: they're hidden on purpose.
	if len(f.logs) != 2 || !strings.Contains(f.logs[1], "hiding 5 marked as configuration or disabled") {
		t.Fatalf("unexpected log lines %v", f.logs)
	}

	// States for them go nowhere.
	before := len(f.container.broadcasts)
	f.native("plug1", &pb.SwitchStateResponse{Key: 2, State: true}, &pb.SensorStateResponse{Key: 5, State: 50})

	if len(f.container.broadcasts) != before {
		t.Fatalf("a hidden entity's state was taken in: %+v", f.container.broadcasts[before:])
	}
}

// If a device that listed an entity before marks it as configuration now, it's dropped like anything else the
// device no longer offers.
func TestNative_AnEntityThatBecomesHiddenIsDropped(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.native("plug1", &pb.ListEntitiesSwitchResponse{ObjectId: "led", Key: 2, Name: "LED"}, &pb.ListEntitiesDoneResponse{})
	f.native("plug1",
		&pb.ListEntitiesSwitchResponse{ObjectId: "led", Key: 2, Name: "LED", EntityCategory: pb.EntityCategory_ENTITY_CATEGORY_CONFIG},
		&pb.ListEntitiesDoneResponse{})

	if want := []string{"esphome_plug1/led"}; !reflect.DeepEqual(f.container.deregistered, want) {
		t.Fatalf("deregistered %v, want %v", f.container.deregistered, want)
	}
}
