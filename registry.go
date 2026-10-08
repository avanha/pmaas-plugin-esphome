package esphome

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/internal/entities"
	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
)

var wirelessThermometerType = reflect.TypeFor[environment.IWirelessThermometer]()

// registry turns what devices publish (discovery configs, then state) into PMAAS entities and
// events. Everything in it runs on the plugin's own goroutine; the plugin hands it work from the
// broker's goroutines through the container.
type registry struct {
	container       spi.IPMAASContainer
	discoveryPrefix string
	// displayNames maps a device name to the name to show for it, instead of the one it announces.
	displayNames map[string]string

	nodes   map[string]*node
	stopped bool

	logf func(format string, args ...any)
	now  func() time.Time
}

// node is one ESPHome device: everything announced under its name.
type node struct {
	name         string
	connected    bool
	onlineSince  time.Time
	offlineSince time.Time

	// Keyed by the discovery object id.
	temps  map[string]*temperatureChannel
	humids map[string]*sensorChannel
	doors  map[string]*doorChannel
}

// sensorChannel is a discovered numeric sensor and its latest reading.
type sensorChannel struct {
	disc       hadiscovery.Entity
	toBase     func(float32) float32
	value      float32
	hasValue   bool
	lastUpdate time.Time
}

type temperatureChannel struct {
	sensorChannel
	thermometer *entities.Thermometer
}

type doorChannel struct {
	disc hadiscovery.Entity
	door *entities.Door
}

func newRegistry(container spi.IPMAASContainer, discoveryPrefix string, displayNames map[string]string) *registry {
	return &registry{
		container:       container,
		discoveryPrefix: discoveryPrefix,
		displayNames:    displayNames,
		nodes:           make(map[string]*node),
		logf: func(format string, args ...any) {
			fmt.Printf("esphome: "+format+"\n", args...)
		},
		now: time.Now,
	}
}

func (r *registry) nodeFor(name string) *node {
	n, ok := r.nodes[name]
	if !ok {
		n = &node{
			name:   name,
			temps:  make(map[string]*temperatureChannel),
			humids: make(map[string]*sensorChannel),
			doors:  make(map[string]*doorChannel),
		}
		r.nodes[name] = n
	}

	return n
}

// HandleConnected records that a device's session started.
func (r *registry) HandleConnected(device string) {
	if r.stopped {
		return
	}

	r.logf("%s: connected", device)

	n := r.nodeFor(device)
	n.connected = true
	n.onlineSince = r.now()

	for _, ch := range n.doors {
		r.updateDoorConnectivity(n, ch)
	}
}

// HandleDisconnected records that a device's session ended. Its entities stay registered, marked
// offline: it'll be back, and the last known state is still worth showing.
func (r *registry) HandleDisconnected(device string) {
	if r.stopped {
		return
	}

	r.logf("%s: disconnected", device)

	n := r.nodeFor(device)
	n.connected = false
	n.offlineSince = r.now()

	for _, ch := range n.doors {
		r.updateDoorConnectivity(n, ch)
	}
}

// HandleMessage processes one message a device published.
func (r *registry) HandleMessage(device string, topic string, payload []byte) {
	if r.stopped {
		return
	}

	if _, isDiscovery := hadiscovery.ParseTopic(r.discoveryPrefix, topic); isDiscovery {
		r.handleDiscovery(device, topic, payload)
		return
	}

	n, ok := r.nodes[device]
	if !ok {
		return
	}

	for _, ch := range n.temps {
		if ch.disc.StateTopic == topic {
			r.handleSensorState(n, &ch.sensorChannel, payload)
		}
	}

	for _, ch := range n.humids {
		if ch.disc.StateTopic == topic {
			r.handleSensorState(n, ch, payload)
		}
	}

	for _, ch := range n.doors {
		if ch.disc.StateTopic == topic {
			r.handleDoorState(n, ch, payload)
		}
	}
}

// Shutdown deregisters every entity and ignores everything after.
func (r *registry) Shutdown() {
	r.stopped = true

	for _, n := range r.nodes {
		for _, ch := range n.temps {
			r.deregisterThermometer(ch)
		}

		for _, ch := range n.doors {
			r.deregisterDoor(ch)
		}
	}

	clear(r.nodes)
}

func (r *registry) handleDiscovery(device string, topic string, payload []byte) {
	entity, key, remove, err := hadiscovery.Parse(r.discoveryPrefix, topic, payload)
	if err != nil {
		r.logf("%s: %v", device, err)
		return
	}

	// The broker's ACL only lets a device publish its own configs, but this is what decides which
	// device an entity belongs to, so don't depend on that.
	if key.NodeID != device {
		r.logf("%s: ignoring discovery config for node %q", device, key.NodeID)
		return
	}

	n := r.nodeFor(device)

	if remove {
		r.removeChannel(n, key)
		return
	}

	// An entity that stops being usable on re-announcement (e.g. it gained a value template) must
	// not linger with its old, now-wrong topics.
	if reason := unusable(device, *entity); reason != "" {
		r.logf("%s: ignoring %s %q: %s", device, entity.Component, entity.ObjectID, reason)
		r.removeChannel(n, key)

		return
	}

	switch {
	case entity.Component == "sensor" && entity.DeviceClass == "temperature":
		r.upsertTemperature(n, *entity)
	case entity.Component == "sensor" && entity.DeviceClass == "humidity":
		r.upsertHumidity(n, *entity)
	case entity.Component == "binary_sensor" && isDoorClass(entity.DeviceClass):
		r.upsertDoor(n, *entity)
	default:
		r.logf("%s: ignoring unsupported %s %q (device_class %q): %s",
			device, entity.Component, entity.ObjectID, entity.DeviceClass, unsupportedHint(*entity))
	}
}

// unsupportedHint says what to do about an entity that isn't tracked. The one that comes up in practice is a
// binary sensor whose YAML doesn't say what it is: ESPHome only announces a device_class if one is set, and
// without one there's no telling a door from a motion sensor.
func unsupportedHint(e hadiscovery.Entity) string {
	if e.Component == "binary_sensor" && e.DeviceClass == "" {
		return "if it's a door, set device_class: garage_door (or door) on it in the device's YAML"
	}

	return "PMAAS tracks sensors with device_class temperature or humidity, and binary_sensors with device_class " +
		"garage_door, door or opening"
}

// unusable returns why e can't be tracked, or "" if it can.
func unusable(device string, e hadiscovery.Entity) string {
	if e.StateTopic == "" {
		return "no state topic"
	}

	// Besides being how ESPHome lays its topics out, this is what makes the broker's ACL cover
	// state topics: a device can only be listened to on topics it's allowed to publish to.
	if !strings.HasPrefix(e.StateTopic, device+"/") {
		return fmt.Sprintf("state topic %q is outside the device's own topic tree", e.StateTopic)
	}

	if e.ValueTemplate != "" {
		return "uses a value_template, which isn't supported"
	}

	return ""
}

func isDoorClass(deviceClass string) bool {
	switch deviceClass {
	case "garage_door", "door", "opening":
		return true
	default:
		return false
	}
}

// temperatureConverter returns a function converting a reading in unit to Celsius, or nil if the
// unit isn't one it knows. A sensor that doesn't say is taken to be Celsius.
func temperatureConverter(unit string) func(float32) float32 {
	switch unit {
	case "", "°C":
		return func(v float32) float32 { return v }
	case "°F":
		return func(v float32) float32 { return (v - 32) * 5 / 9 }
	default:
		return nil
	}
}

func (r *registry) upsertTemperature(n *node, e hadiscovery.Entity) {
	toBase := temperatureConverter(e.UnitOfMeasurement)
	if toBase == nil {
		r.logf("%s: ignoring temperature sensor %q: unsupported unit %q", n.name, e.ObjectID, e.UnitOfMeasurement)
		r.removeChannel(n, e.Key())

		return
	}

	ch, ok := n.temps[e.ObjectID]
	if !ok {
		ch = &temperatureChannel{}
		n.temps[e.ObjectID] = ch
	}

	ch.disc = e
	ch.toBase = toBase

	if ch.thermometer == nil {
		r.registerThermometer(n, ch)
	}

	r.refreshThermometers(n)
}

func (r *registry) upsertHumidity(n *node, e hadiscovery.Entity) {
	ch, ok := n.humids[e.ObjectID]
	if !ok {
		ch = &sensorChannel{}
		n.humids[e.ObjectID] = ch
	}

	ch.disc = e
	ch.toBase = func(v float32) float32 { return v }

	r.refreshThermometers(n)
}

func (r *registry) upsertDoor(n *node, e hadiscovery.Entity) {
	ch, ok := n.doors[e.ObjectID]
	if !ok {
		ch = &doorChannel{}
		n.doors[e.ObjectID] = ch
	}

	ch.disc = e

	if ch.door == nil {
		r.registerDoor(n, ch)
		return
	}

	// Already registered: only a changed name is worth telling anyone.
	if name := r.entityName(n.name, e); name != ch.door.Data.Name {
		ch.door.Data.Name = name
		r.broadcastState(ch.door.PmaasEntityId, device.IDoorSensorType, name, ch.door.Data)
	}
}

func (r *registry) removeChannel(n *node, key hadiscovery.Key) {
	switch key.Component {
	case "sensor":
		if ch, ok := n.temps[key.ObjectID]; ok {
			r.deregisterThermometer(ch)
			delete(n.temps, key.ObjectID)
		}

		delete(n.humids, key.ObjectID)

		// Removing a sensor can change which humidity sensor belongs with which thermometer.
		r.refreshThermometers(n)
	case "binary_sensor":
		if ch, ok := n.doors[key.ObjectID]; ok {
			r.deregisterDoor(ch)
			delete(n.doors, key.ObjectID)
		}
	}
}

// entityName builds the name shown for an entity: the device's label, then the entity's own name.
// ESPHome names an entity relative to its device ("Temperature"), so on its own that would be
// ambiguous across devices.
func (r *registry) entityName(deviceName string, e hadiscovery.Entity) string {
	label := r.displayNames[deviceName]
	if label == "" {
		label = e.Device.Name
	}

	if label == "" {
		label = deviceName
	}

	name := strings.TrimSpace(e.Name)

	switch {
	case name == "" || strings.EqualFold(name, label):
		return label
	case strings.HasPrefix(strings.ToLower(name), strings.ToLower(label)+" "):
		return name
	default:
		return label + " " + name
	}
}

// --- thermometers ---

func (r *registry) registerThermometer(n *node, ch *temperatureChannel) {
	th := &entities.Thermometer{}
	th.Data.Name = r.entityName(n.name, ch.disc)

	id, err := r.container.RegisterEntity(
		n.name+"/"+ch.disc.ObjectID,
		wirelessThermometerType,
		th.Data.Name,
		func() (any, error) { return th.GetStub(r.container), nil })
	if err != nil {
		// Leave ch.thermometer nil; the next time the device announces itself we'll try again.
		r.logf("%s: unable to register thermometer %q: %v", n.name, ch.disc.ObjectID, err)
		return
	}

	th.PmaasEntityId = id
	ch.thermometer = th
}

func (r *registry) deregisterThermometer(ch *temperatureChannel) {
	if ch.thermometer == nil {
		return
	}

	if err := r.container.DeregisterEntity(ch.thermometer.PmaasEntityId); err != nil {
		r.logf("unable to deregister thermometer %s: %v", ch.thermometer.PmaasEntityId, err)
	}

	ch.thermometer.CloseStubIfPresent()
	ch.thermometer = nil
}

// humidityFor returns the humidity sensor to fold into ch's thermometer: the device's only one, if
// the device has exactly one temperature sensor. With several temperature sensors there's no telling
// which humidity sensor goes with which, so none is attached.
func (r *registry) humidityFor(n *node) *sensorChannel {
	if len(n.temps) != 1 || len(n.humids) == 0 {
		return nil
	}

	ids := make([]string, 0, len(n.humids))
	for id := range n.humids {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	return n.humids[ids[0]]
}

// refreshThermometers recomputes every thermometer on n from the readings and discovery info
// currently held, and broadcasts those that differ from what they last showed.
func (r *registry) refreshThermometers(n *node) {
	for _, ch := range n.temps {
		if ch.thermometer == nil {
			continue
		}

		data := environment.WirelessThermometer{Name: r.entityName(n.name, ch.disc)}

		if ch.hasValue {
			data.SensorData.HasData = true
			data.SensorData.Temperature = ch.value
			data.SensorData.LastUpdateTime = ch.lastUpdate
		}

		if humidity := r.humidityFor(n); humidity != nil && humidity.hasValue {
			data.SensorData.HasHumidity = true
			data.SensorData.Humidity = humidity.value

			if humidity.lastUpdate.After(data.SensorData.LastUpdateTime) {
				data.SensorData.LastUpdateTime = humidity.lastUpdate
			}
		}

		if data == ch.thermometer.Data {
			continue
		}

		ch.thermometer.Data = data
		r.broadcastState(ch.thermometer.PmaasEntityId, wirelessThermometerType, data.Name, data)
	}
}

func (r *registry) handleSensorState(n *node, ch *sensorChannel, payload []byte) {
	value, ok := parseNumber(payload)
	if !ok {
		return
	}

	ch.value = ch.toBase(value)
	ch.hasValue = true
	ch.lastUpdate = r.now()

	r.refreshThermometers(n)
}

// parseNumber reads a sensor payload. ESPHome publishes "nan" for a reading it couldn't take, which
// (like anything else that isn't a finite number) is no reading at all.
func parseNumber(payload []byte) (float32, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(string(payload)), 32)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}

	return float32(value), true
}

// --- doors ---

func (r *registry) registerDoor(n *node, ch *doorChannel) {
	d := &entities.Door{}
	d.Data.Name = r.entityName(n.name, ch.disc)
	r.applyConnectivity(n, &d.Data)

	id, err := r.container.RegisterEntity(
		n.name+"/"+ch.disc.ObjectID,
		device.IDoorSensorType,
		d.Data.Name,
		func() (any, error) { return d.GetStub(r.container), nil })
	if err != nil {
		r.logf("%s: unable to register door sensor %q: %v", n.name, ch.disc.ObjectID, err)
		return
	}

	d.PmaasEntityId = id
	ch.door = d
}

func (r *registry) deregisterDoor(ch *doorChannel) {
	if ch.door == nil {
		return
	}

	if err := r.container.DeregisterEntity(ch.door.PmaasEntityId); err != nil {
		r.logf("unable to deregister door sensor %s: %v", ch.door.PmaasEntityId, err)
	}

	ch.door.CloseStubIfPresent()
	ch.door = nil
}

// applyConnectivity copies the device's connectivity onto a door's data.
func (r *registry) applyConnectivity(n *node, data *device.DoorSensor) {
	data.OnlineSince = n.onlineSince
	data.OfflineSince = n.offlineSince

	if n.connected {
		data.Connectivity = environment.ConnectivityOnline
	} else {
		data.Connectivity = environment.ConnectivityOffline
	}
}

func (r *registry) updateDoorConnectivity(n *node, ch *doorChannel) {
	if ch.door == nil {
		return
	}

	r.applyConnectivity(n, &ch.door.Data)
	r.broadcastState(ch.door.PmaasEntityId, device.IDoorSensorType, ch.door.Data.Name, ch.door.Data)
}

func (r *registry) handleDoorState(n *node, ch *doorChannel, payload []byte) {
	if ch.door == nil {
		return
	}

	var position device.DoorPosition

	// Home Assistant's convention for door-like binary sensors: on means open.
	switch strings.TrimSpace(string(payload)) {
	case ch.disc.StatePayloadOn:
		position = device.DoorPositionOpen
	case ch.disc.StatePayloadOff:
		position = device.DoorPositionClosed
	default:
		r.logf("%s: unrecognized door state %q on %s", n.name, payload, ch.disc.StateTopic)
		return
	}

	d := ch.door
	old := d.Data
	now := r.now()

	d.Data.LastUpdateTime = now

	switch {
	case !old.HasData:
		// The first report establishes the position; it isn't known to be a change.
		d.Data.HasData = true
		d.Data.Position = position
	case old.Position != position:
		d.Data.Position = position
		d.Data.LastChangeTime = now
	default:
		return
	}

	r.broadcastState(d.PmaasEntityId, device.IDoorSensorType, d.Data.Name, d.Data)

	if old.HasData {
		r.broadcast(d.PmaasEntityId, device.DoorPositionChangedEvent{
			EntityEvent: events.EntityEvent{
				Id: d.PmaasEntityId, EntityType: device.IDoorSensorType, Name: d.Data.Name,
			},
			NewPosition: position,
			OldPosition: old.Position,
		})
	}
}

// --- events ---

func (r *registry) broadcastState(id string, entityType reflect.Type, name string, newState any) {
	r.broadcast(id, events.EntityStateChangedEvent{
		EntityEvent: events.EntityEvent{Id: id, EntityType: entityType, Name: name},
		NewState:    newState,
	})
}

func (r *registry) broadcast(id string, event any) {
	if err := r.container.BroadcastEvent(id, event); err != nil {
		r.logf("unable to broadcast %T for %s: %v", event, id, err)
	}
}
