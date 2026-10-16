package esphome

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
)

func lastPowerState(t *testing.T, c *fakeContainer) device.PowerMonitor {
	t.Helper()

	return c.lastBroadcast(t).event.(events.EntityStateChangedEvent).NewState.(device.PowerMonitor)
}

func (f *fixture) powerSensor(deviceName string, object string, class string, unit string) {
	f.discover(deviceName, "sensor", object, sensorConfig(deviceName, object, strings.ToUpper(object[:1])+object[1:], class, unit))
}

func (f *fixture) powerState(deviceName string, object string, payload string) {
	f.state(deviceName, deviceName+"/sensor/"+object+"/state", payload)
}

func TestPower_OneEntityPerDeviceNoMatterHowManySensors(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power", "power", "W")
	f.powerSensor("garage1", "voltage", "voltage", "V")
	f.powerSensor("garage1", "current", "current", "A")
	f.powerSensor("garage1", "energy", "energy", "kWh")

	if len(f.container.registrations) != 1 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	reg := f.container.registrations[0]
	if reg.entityType != device.IPowerMonitorType || reg.name != "Garage 1 Power" || reg.uniqueData != "garage1/_power" {
		t.Fatalf("unexpected registration %+v", reg)
	}

	// Another device gets its own.
	f.powerSensor("garage2", "power", "power", "W")

	if len(f.container.registrations) != 2 || f.container.registrations[1].uniqueData != "garage2/_power" {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}
}

func TestPower_FlagsSayWhichReadingsAreThere(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power", "power", "W")
	f.powerSensor("garage1", "voltage", "voltage", "V")

	stub, _ := f.container.registrations[0].stubFactory()
	monitor := func() device.PowerMonitor { return stub.(device.IPowerMonitor).GetPowerMonitorData() }

	if got := monitor(); got.HasData || got.HasPower || got.HasVoltage || got.HasCurrent || got.HasEnergy {
		t.Fatalf("nothing has been reported, but %+v", got)
	}

	f.advance(time.Second)
	f.powerState("garage1", "power", "42.5")

	got := lastPowerState(t, f.container)
	if !got.HasData || !got.HasPower || got.Power != 42.5 || got.HasVoltage || got.HasCurrent || got.HasEnergy ||
		got.Voltage != 0 || !got.LastUpdateTime.Equal(f.clock) {
		t.Fatalf("unexpected state %+v", got)
	}

	f.advance(time.Second)
	f.powerState("garage1", "voltage", "121.3")

	got = lastPowerState(t, f.container)
	if !got.HasPower || got.Power != 42.5 || !got.HasVoltage || got.Voltage != 121.3 || got.HasCurrent || got.HasEnergy ||
		!got.LastUpdateTime.Equal(f.clock) {
		t.Fatalf("unexpected state %+v", got)
	}

	if monitor() != got {
		t.Fatalf("the entity and its broadcast disagree: %+v vs %+v", monitor(), got)
	}
}

func TestPower_ZeroIsAReadingNotAbsence(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power", "power", "W")
	f.powerState("garage1", "power", "0")

	if got := lastPowerState(t, f.container); !got.HasPower || got.Power != 0 || !got.HasData {
		t.Fatalf("a plug drawing nothing has reported: %+v", got)
	}
}

func TestPower_UnitsAreConvertedToWattsVoltsAmpsAndKilowattHours(t *testing.T) {
	for _, c := range []struct {
		class, unit, payload string
		want                 float64
	}{
		{"power", "W", "100", 100},
		{"power", "", "100", 100},
		{"power", "kW", "1.5", 1500},
		{"power", "mW", "2500", 2.5},
		{"voltage", "V", "230", 230},
		{"voltage", "mV", "230000", 230},
		{"current", "A", "0.5", 0.5},
		{"current", "mA", "500", 0.5},
		{"energy", "kWh", "12.5", 12.5},
		{"energy", "Wh", "12500", 12.5},
		{"energy", "MWh", "0.0125", 12.5},
	} {
		f := newFixture(t)
		f.powerSensor("garage1", "reading", c.class, c.unit)
		f.powerState("garage1", "reading", c.payload)

		got := lastPowerState(t, f.container)
		value := map[string]float64{"power": got.Power, "voltage": got.Voltage, "current": got.Current, "energy": got.Energy}[c.class]

		if math.Abs(value-c.want) > 1e-9 {
			t.Errorf("%s in %q: %s = %v, want %v (%+v)", c.class, c.unit, c.payload, value, c.want, got)
		}
	}
}

func TestPower_EnergyKeepsItsPrecision(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "energy", "energy", "kWh")
	f.powerState("garage1", "energy", "12345.678")

	// A float32 would read this as 12345.6777...
	if got := lastPowerState(t, f.container); got.Energy != 12345.678 {
		t.Fatalf("energy %v", got.Energy)
	}
}

func TestPower_UnusableReadingsAndUnitsIgnored(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power", "power", "W")
	f.powerState("garage1", "power", "10")

	before := len(f.container.broadcasts)

	for _, payload := range []string{"nan", "NaN", "inf", "", "unavailable", "12abc"} {
		f.powerState("garage1", "power", payload)
	}

	if len(f.container.broadcasts) != before {
		t.Fatalf("an unusable reading was taken: %+v", f.container.broadcasts[before:])
	}

	// A unit it doesn't know means it can't be put in watts, so there's nothing to track.
	g := newFixture(t)
	g.powerSensor("garage1", "power", "power", "BTU/h")

	if len(g.container.registrations) != 0 || len(g.logs) == 0 || !strings.Contains(g.logs[0], "unsupported unit") {
		t.Fatalf("registrations %+v, logs %v", g.container.registrations, g.logs)
	}
}

func TestPower_TwoSensorsOfAKindUseTheFirstByObjectIdWhicheverReportsLast(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power_b", "power", "W")
	f.powerSensor("garage1", "power_a", "power", "W")

	f.powerState("garage1", "power_a", "1")
	f.powerState("garage1", "power_b", "2")

	if got := lastPowerState(t, f.container); got.Power != 1 {
		t.Fatalf("expected power_a's reading, got %+v", got)
	}

	f.powerState("garage1", "power_a", "3")

	if got := lastPowerState(t, f.container); got.Power != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestPower_ConnectivityFollowsTheDevice(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power", "power", "W")

	f.advance(time.Second)
	f.registry.HandleConnected("garage1", "10.0.0.1")

	if got := lastPowerState(t, f.container); got.Connectivity != environment.ConnectivityOnline || !got.OnlineSince.Equal(f.clock) {
		t.Fatalf("expected online, got %+v", got)
	}

	f.powerState("garage1", "power", "5")
	f.advance(time.Minute)
	f.registry.HandleDisconnected("garage1")

	got := lastPowerState(t, f.container)
	if got.Connectivity != environment.ConnectivityOffline || !got.OfflineSince.Equal(f.clock) || got.Power != 5 {
		t.Fatalf("expected offline with the last reading kept, got %+v", got)
	}
}

func TestPower_RegisteredWhileOfflineStartsOffline(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power", "power", "W")

	stub, _ := f.container.registrations[0].stubFactory()

	if got := stub.(device.IPowerMonitor).GetPowerMonitorData(); got.Connectivity != environment.ConnectivityOffline {
		t.Fatalf("unexpected connectivity %v", got.Connectivity)
	}
}

func TestPower_RemovedWithItsLastSensor(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power", "power", "W")
	f.powerSensor("garage1", "voltage", "voltage", "V")
	f.powerState("garage1", "power", "10")
	f.powerState("garage1", "voltage", "120")
	stub, _ := f.container.registrations[0].stubFactory()

	// One sensor going doesn't remove the monitor, but its reading goes with it.
	f.discover("garage1", "sensor", "voltage", "")

	if len(f.container.deregistered) != 0 {
		t.Fatalf("deregistered %v", f.container.deregistered)
	}

	if got := lastPowerState(t, f.container); got.HasVoltage || !got.HasPower || got.Power != 10 {
		t.Fatalf("unexpected state %+v", got)
	}

	f.discover("garage1", "sensor", "power", "")

	if want := []string{"esphome_garage1/_power"}; !reflect.DeepEqual(f.container.deregistered, want) {
		t.Fatalf("deregistered %v, want %v", f.container.deregistered, want)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("expected a call through a closed stub to panic, like the other plugins' stubs")
		}
	}()

	stub.(device.IPowerMonitor).GetPowerMonitorData()
}

func TestPower_ReannouncingASensorWithABadUnitRemovesIt(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage1", "power", "power", "W")
	f.powerSensor("garage1", "power", "power", "BTU/h")

	if want := []string{"esphome_garage1/_power"}; !reflect.DeepEqual(f.container.deregistered, want) {
		t.Fatalf("deregistered %v, want %v", f.container.deregistered, want)
	}
}

func TestPower_RegistrationFailureIsRetriedOnTheNextAnnouncement(t *testing.T) {
	f := newFixture(t)
	f.container.registerErr = errTest
	f.powerSensor("garage1", "power", "power", "W")
	f.powerState("garage1", "power", "5") // must not panic with no entity

	f.container.registerErr = nil
	f.powerSensor("garage1", "power", "power", "W")

	if len(f.container.registrations) != 1 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	// The reading from before is still held, so it shows at once.
	if got := lastPowerState(t, f.container); !got.HasPower || got.Power != 5 {
		t.Fatalf("unexpected state %+v", got)
	}
}

func TestPower_NamedAfterTheDevicesLabelAndFollowsIt(t *testing.T) {
	f := newFixture(t)
	f.powerSensor("garage2", "power", "power", "W") // no display name configured: the announced one is used

	if got := f.container.registrations[0].name; got != "garage2 Power" {
		t.Fatalf("unexpected name %q", got)
	}
}
