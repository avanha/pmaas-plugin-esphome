package esphome

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/internal/entities"
	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
	"github.com/avanha/pmaas-spi/device"
)

// powerKind is which electrical reading a sensor takes.
type powerKind int

const (
	powerKindNone powerKind = iota
	powerKindPower
	powerKindVoltage
	powerKindCurrent
	powerKindEnergy
)

// powerKindFor says which reading a sensor with this device class takes, if any.
func powerKindFor(deviceClass string) powerKind {
	switch deviceClass {
	case "power":
		return powerKindPower
	case "voltage":
		return powerKindVoltage
	case "current":
		return powerKindCurrent
	case "energy":
		return powerKindEnergy
	default:
		return powerKindNone
	}
}

// powerConverter returns a function converting a reading of kind in unit to the unit device.PowerMonitor
// uses (watts, volts, amps, kilowatt-hours), or nil if the unit isn't one it knows. A sensor that doesn't
// say is taken to already be in that unit.
func powerConverter(kind powerKind, unit string) func(float64) float64 {
	same := func(v float64) float64 { return v }
	scale := func(factor float64) func(float64) float64 {
		return func(v float64) float64 { return v * factor }
	}

	switch kind {
	case powerKindPower:
		switch unit {
		case "", "W":
			return same
		case "kW":
			return scale(1000)
		case "mW":
			return scale(0.001)
		}
	case powerKindVoltage:
		switch unit {
		case "", "V":
			return same
		case "mV":
			return scale(0.001)
		case "kV":
			return scale(1000)
		}
	case powerKindCurrent:
		switch unit {
		case "", "A":
			return same
		case "mA":
			return scale(0.001)
		}
	case powerKindEnergy:
		switch unit {
		case "", "kWh":
			return same
		case "Wh":
			return scale(0.001)
		case "MWh":
			return scale(1000)
		}
	}

	return nil
}

// powerSensorChannel is a discovered power, voltage, current or energy sensor and its latest reading.
type powerSensorChannel struct {
	disc       hadiscovery.Entity
	kind       powerKind
	toBase     func(float64) float64
	value      float64
	hasValue   bool
	lastUpdate time.Time
}

// powerEntityObjectID is what a device's power monitor is registered under, alongside the object ids of its
// own entities. It isn't something ESPHome derives from a name, which keeps it from colliding with one.
const powerEntityObjectID = "_power"

func (r *registry) upsertPowerSensor(n *node, e hadiscovery.Entity) {
	kind := powerKindFor(e.DeviceClass)

	toBase := powerConverter(kind, e.UnitOfMeasurement)
	if toBase == nil {
		r.logf("%s: ignoring %s sensor %q: unsupported unit %q", n.name, e.DeviceClass, e.ObjectID, e.UnitOfMeasurement)
		r.removeChannel(n, e.Key())

		return
	}

	ch, ok := n.powerSensors[e.ObjectID]
	if !ok {
		ch = &powerSensorChannel{}
		n.powerSensors[e.ObjectID] = ch
	}

	ch.disc = e
	ch.kind = kind
	ch.toBase = toBase

	if n.power == nil {
		r.registerPowerMonitor(n)
	}

	r.refreshPowerMonitor(n)
}

func (r *registry) powerMonitorName(n *node) string {
	return r.deviceLabel(n.name, n.info.Name) + " Power"
}

func (r *registry) registerPowerMonitor(n *node) {
	p := &entities.PowerMonitor{}
	p.Data.Name = r.powerMonitorName(n)
	r.applyConnectivity(n, &p.Data.Connectivity, &p.Data.OnlineSince, &p.Data.OfflineSince)

	id, err := r.container.RegisterEntity(
		n.name+"/"+powerEntityObjectID,
		device.IPowerMonitorType,
		p.Data.Name,
		func() (any, error) { return p.GetStub(r.container), nil })
	if err != nil {
		// Leave n.power nil; the next time the device announces a sensor we'll try again.
		r.logf("%s: unable to register power monitor: %v", n.name, err)
		return
	}

	p.PmaasEntityId = id
	n.power = p
}

func (r *registry) deregisterPowerMonitor(n *node) {
	if n.power == nil {
		return
	}

	if err := r.container.DeregisterEntity(n.power.PmaasEntityId); err != nil {
		r.logf("unable to deregister power monitor %s: %v", n.power.PmaasEntityId, err)
	}

	n.power.CloseStubIfPresent()
	n.power = nil
}

// powerSensorFor returns the sensor to take a kind of reading from. A device with two of a kind (say, two
// power sensors) has no way of saying which is the one, so it's the first by object id: the same answer
// every time, rather than whichever happened to report last.
func (n *node) powerSensorFor(kind powerKind) *powerSensorChannel {
	ids := make([]string, 0, len(n.powerSensors))

	for id, ch := range n.powerSensors {
		if ch.kind == kind {
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		return nil
	}

	return n.powerSensors[slices.Min(ids)]
}

// refreshPowerMonitor recomputes the device's power monitor from the readings and connectivity currently held,
// and broadcasts it if that differs from what it last showed.
func (r *registry) refreshPowerMonitor(n *node) {
	if n.power == nil {
		return
	}

	data := device.PowerMonitor{Name: r.powerMonitorName(n)}
	r.applyConnectivity(n, &data.Connectivity, &data.OnlineSince, &data.OfflineSince)

	take := func(kind powerKind, has *bool, value *float64) {
		ch := n.powerSensorFor(kind)
		if ch == nil || !ch.hasValue {
			return
		}

		*has = true
		*value = ch.value
		data.HasData = true

		if ch.lastUpdate.After(data.LastUpdateTime) {
			data.LastUpdateTime = ch.lastUpdate
		}
	}

	take(powerKindPower, &data.HasPower, &data.Power)
	take(powerKindVoltage, &data.HasVoltage, &data.Voltage)
	take(powerKindCurrent, &data.HasCurrent, &data.Current)
	take(powerKindEnergy, &data.HasEnergy, &data.Energy)

	if data == n.power.Data {
		return
	}

	n.power.Data = data
	r.broadcastState(n.power.PmaasEntityId, device.IPowerMonitorType, data.Name, data)
}

func (r *registry) handlePowerSensorState(n *node, ch *powerSensorChannel, payload []byte) {
	value, ok := parseFloat(payload)
	if !ok {
		return
	}

	ch.value = ch.toBase(value)
	ch.hasValue = true
	ch.lastUpdate = r.now()

	r.refreshPowerMonitor(n)
}

// parseFloat reads a sensor payload at full precision, which matters for an energy counter: a meter that's
// been counting for months reads in the thousands of kilowatt-hours, with fractions that matter. Like
// parseNumber, anything that isn't a finite number is no reading at all.
func parseFloat(payload []byte) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(string(payload)), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}

	return value, true
}
