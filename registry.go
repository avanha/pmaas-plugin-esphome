package esphome

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/internal/entities"
	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/alert"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
	"google.golang.org/protobuf/proto"
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

	// publish sends a command to a device, by publishing to one of its topics. It returns once the message is
	// queued, not once it's delivered. The plugin sets it once there's a broker to publish with.
	publish func(topic string, payload []byte) error

	// control is how each device's switches are guarded, by the device's name, for those that are; failures is the
	// wrong passwords given for each.
	control  map[string]SwitchControl
	failures map[string]*passwordFailures

	// nativeSend sends a message to a device over the native API, and like publish returns once it's queued.
	nativeSend func(deviceName string, message proto.Message) error

	// handles are the assembly's handles on the devices, by name, for those it asked for. They're told when a
	// device's switches come and go.
	handles map[string]*DeviceHandle

	// offlineAlerts is the alert each device is to have for going offline, and offlineWatches what holds it, by the
	// device's name. startedAt is when the plugin started, which is when a device that has never connected has been offline
	// since.
	offlineAlerts  map[string]alert.OfflineAlert
	offlineWatches map[string]*alert.OfflineWatch
	startedAt      time.Time

	logf func(format string, args ...any)
	now  func() time.Time
}

// node is one ESPHome device: everything announced under its name.
type node struct {
	name         string
	connected    bool
	onlineSince  time.Time
	offlineSince time.Time

	// address is the IP address the device last connected from, as the broker saw it.
	address string

	// info is what the device says about itself in the device block of what it announces (firmware version,
	// model, ...). Empty until it has announced something.
	info hadiscovery.Device

	// lastMessage is when the device last sent anything at all.
	lastMessage time.Time

	// Keyed by the discovery object id.
	temps  map[string]*temperatureChannel
	humids map[string]*sensorChannel
	doors  map[string]*doorChannel

	switches     map[string]*switchChannel
	powerSensors map[string]*powerSensorChannel

	// power is the device's power monitor: one entity for all its power, voltage, current and energy sensors,
	// registered when the first of them is announced and deregistered when the last is gone.
	power *entities.PowerMonitor

	// What's only used by a device that's reached over the native API: see registry_native.go.
	native nativeState
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

			switches:     make(map[string]*switchChannel),
			powerSensors: make(map[string]*powerSensorChannel),
		}
		r.nodes[name] = n
	}

	return n
}

// startOfflineWatches makes what holds each device's alert for going offline. Called when the plugin starts.
func (r *registry) startOfflineWatches() {
	r.startedAt = r.now()
	r.offlineWatches = make(map[string]*alert.OfflineWatch, len(r.offlineAlerts))

	for name, policy := range r.offlineAlerts {
		shown := name
		if display, ok := r.displayNames[name]; ok {
			shown = display
		}

		r.offlineWatches[name] = policy.Watch("esphome", "offline:"+name, shown)
	}

	r.checkOfflineAlerts()
}

// checkOfflineAlerts looks at every device that is to have an alert for going offline: one that has been offline for
// the alert's time is due one without anything having changed, so this is also done regularly.
func (r *registry) checkOfflineAlerts() {
	for name := range r.offlineWatches {
		r.checkOffline(name)
	}
}

// checkOffline raises, escalates or clears one device's alert for going offline, if it is to have one.
func (r *registry) checkOffline(device string) {
	watch := r.offlineWatches[device]
	if watch == nil || r.stopped {
		return
	}

	connectivity, since := environment.ConnectivityOffline, r.startedAt

	if n, ok := r.nodes[device]; ok {
		if n.connected {
			connectivity = environment.ConnectivityOnline
		} else if !n.offlineSince.IsZero() {
			since = n.offlineSince
		}
	}

	if err := watch.Check(r.container, connectivity, since, r.now()); err != nil {
		r.logf("%s: unable to update the alert about it being offline: %v", device, err)
	}
}

// HandleConnected records that a device's session started.
func (r *registry) HandleConnected(device string, address string) {
	if r.stopped {
		return
	}

	r.logf("%s: connected from %s", device, address)

	n := r.nodeFor(device)
	n.connected = true
	n.onlineSince = r.now()
	n.address = address

	r.updateConnectivity(n)
	r.checkOffline(device)
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

	r.updateConnectivity(n)
	r.checkOffline(device)
}

// HandleMessage processes one message a device published.
func (r *registry) HandleMessage(device string, topic string, payload []byte) {
	if r.stopped {
		return
	}

	r.nodeFor(device).lastMessage = r.now()

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

	for _, ch := range n.switches {
		if ch.disc.StateTopic == topic {
			r.handleSwitchState(n, ch, payload)
		}
	}

	for _, ch := range n.powerSensors {
		if ch.disc.StateTopic == topic {
			r.handlePowerSensorState(n, ch, payload)
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
			r.deregisterDoor(n, ch)
		}

		for _, ch := range n.switches {
			r.deregisterSwitch(n, ch)
		}

		r.deregisterPowerMonitor(n)
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

	// Every entity of a device carries the same device block, including the ones PMAAS doesn't track.
	n.info = mergeDeviceInfo(n.info, entity.Device)

	// An entity that stops being usable on re-announcement (e.g. it gained a value template) must
	// not linger with its old, now-wrong topics.
	if reason := unusable(device, *entity); reason != "" {
		r.logf("%s: ignoring %s %q: %s", device, entity.Component, entity.ObjectID, reason)
		r.removeChannel(n, key)

		return
	}

	r.upsertEntity(n, *entity, true)
}

// upsertEntity registers, or updates, the entity an announcement describes, whichever way it was announced, and
// reports whether it's one PMAAS tracks. One that isn't is logged, with what to do about it, if logUnsupported:
// a device announcing over MQTT has few entities and each is worth a line, but a device on the native API lists
// everything it has, and says so in one line instead.
func (r *registry) upsertEntity(n *node, entity hadiscovery.Entity, logUnsupported bool) bool {
	switch {
	case entity.Component == "sensor" && entity.DeviceClass == "temperature":
		r.upsertTemperature(n, entity)
	case entity.Component == "sensor" && entity.DeviceClass == "humidity":
		r.upsertHumidity(n, entity)
	case entity.Component == "sensor" && powerKindFor(entity.DeviceClass) != powerKindNone:
		r.upsertPowerSensor(n, entity)
	case entity.Component == "binary_sensor" && isDoorClass(entity.DeviceClass):
		r.upsertDoor(n, entity)
	case entity.Component == "switch":
		r.upsertSwitch(n, entity)
	default:
		if logUnsupported {
			r.logf("%s: ignoring unsupported %s %q (device_class %q): %s",
				n.name, entity.Component, entity.ObjectID, entity.DeviceClass, unsupportedHint(entity))
		}

		return false
	}

	return true
}

// unsupportedHint says what to do about an entity that isn't tracked. The one that comes up in practice is a
// binary sensor whose YAML doesn't say what it is: ESPHome only announces a device_class if one is set, and
// without one there's no telling a door from a motion sensor.
func unsupportedHint(e hadiscovery.Entity) string {
	if e.Component == "binary_sensor" && e.DeviceClass == "" {
		return "if it's a door, set device_class: garage_door (or door) on it in the device's YAML"
	}

	return "PMAAS tracks switches, sensors with device_class temperature, humidity, power, voltage, current or " +
		"energy, and binary_sensors with device_class garage_door, door or opening"
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

	// PMAAS publishes to a command topic itself, with no ACL in the way, so a device mustn't be able to point
	// one at another device's topics.
	if e.CommandTopic != "" && !strings.HasPrefix(e.CommandTopic, device+"/") {
		return fmt.Sprintf("command topic %q is outside the device's own topic tree", e.CommandTopic)
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

		if handle := r.handles[n.name]; handle != nil {
			handle.doorRenamed(ch.door.PmaasEntityId, name)
		}

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

		if _, ok := n.powerSensors[key.ObjectID]; ok {
			delete(n.powerSensors, key.ObjectID)

			if len(n.powerSensors) == 0 {
				r.deregisterPowerMonitor(n)
			} else {
				r.refreshPowerMonitor(n)
			}
		}
	case "switch":
		if ch, ok := n.switches[key.ObjectID]; ok {
			r.deregisterSwitch(n, ch)
			delete(n.switches, key.ObjectID)
		}
	case "binary_sensor":
		if ch, ok := n.doors[key.ObjectID]; ok {
			r.deregisterDoor(n, ch)
			delete(n.doors, key.ObjectID)
		}
	}
}

// entityName builds the name shown for an entity: the device's label, then the entity's own name.
// ESPHome names an entity relative to its device ("Temperature"), so on its own that would be
// ambiguous across devices.
func (r *registry) entityName(deviceName string, e hadiscovery.Entity) string {
	label := r.deviceLabel(deviceName, e.Device.Name)

	itself, own := splitEntityName(e.Name, label, e.Device.Name)
	if itself {
		return label
	}

	return label + " " + own
}

var (
	nameWordPattern = regexp.MustCompile(`[\p{L}\p{N}]+`)

	// ESPHome ends a device's name with six hex digits of its MAC address to tell devices of one kind apart.
	macSuffixPattern = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)
)

// nameWords is where each word of a name starts and ends. Punctuation and spacing between them aren't words, so
// "kauf-plug-7aad7c" and "Kauf Plug 7aad7c" are the same three.
func nameWords(name string) [][]int {
	return nameWordPattern.FindAllStringIndex(name, -1)
}

func wordsEqual(name string, nameSpans [][]int, other string, otherSpans [][]int) bool {
	if len(nameSpans) != len(otherSpans) {
		return false
	}

	for i := range nameSpans {
		if !strings.EqualFold(name[nameSpans[i][0]:nameSpans[i][1]], other[otherSpans[i][0]:otherSpans[i][1]]) {
			return false
		}
	}

	return true
}

// deviceNameForms are the ways an entity's name can refer to its device: by the label it's been given, and by what
// it calls itself, each also without the end of its MAC address.
func deviceNameForms(label string, announcedName string) []struct {
	text  string
	spans [][]int
} {
	var forms []struct {
		text  string
		spans [][]int
	}

	for _, text := range []string{label, announcedName} {
		spans := nameWords(text)
		if len(spans) == 0 {
			continue
		}

		forms = append(forms, struct {
			text  string
			spans [][]int
		}{text, spans})

		if last := spans[len(spans)-1]; len(spans) > 1 && macSuffixPattern.MatchString(text[last[0]:last[1]]) {
			forms = append(forms, struct {
				text  string
				spans [][]int
			}{text, spans[:len(spans)-1]})
		}
	}

	return forms
}

// splitEntityName says whether an entity is its device's main entity, and if it isn't, what its own name is.
//
// The main entity is called by the device's name and has none of its own: its name is empty, or is the device's, or
// the start of it. A device usually calls itself more than its main entity is called, since the end of the MAC
// address is on the device's name and not on the entity's: a Kauf plug's relay is "Kauf Plug" on a device called
// "Kauf Plug 7981c8". Both what the device is labelled and what it calls itself are checked, since an entity is
// named for the second, whatever the first has been set to.
//
// An entity's own name is what the device called it, without the device's name in front of it if it had that too:
// older Kauf firmware has "Kauf Plug Power" on that plug, and the device's name is already on show.
func splitEntityName(entityName string, label string, announcedName string) (itself bool, own string) {
	entityName = strings.TrimSpace(entityName)
	words := nameWords(entityName)

	if len(words) == 0 {
		return true, ""
	}

	for _, form := range deviceNameForms(label, announcedName) {
		switch {
		case len(words) <= len(form.spans) && wordsEqual(entityName, words, form.text, form.spans[:len(words)]):
			return true, ""
		case len(words) > len(form.spans) && wordsEqual(entityName, words[:len(form.spans)], form.text, form.spans):
			return false, strings.TrimSpace(entityName[words[len(form.spans)][0]:])
		}
	}

	return false, entityName
}

// deviceLabel is what to call a device: the display name it was configured with, else the name it announces
// for itself, else its ESPHome name.
func (r *registry) deviceLabel(deviceName string, announcedName string) string {
	if label := r.displayNames[deviceName]; label != "" {
		return label
	}

	if announcedName != "" {
		return announcedName
	}

	return deviceName
}

// mergeDeviceInfo returns current with every field that update has a value for taken from update. A
// device announces the same block with each entity, but not every field with every one, so what's already
// known isn't lost when one comes in without it.
func mergeDeviceInfo(current hadiscovery.Device, update hadiscovery.Device) hadiscovery.Device {
	if len(update.Identifiers) > 0 {
		current.Identifiers = update.Identifiers
	}

	if update.Name != "" {
		current.Name = update.Name
	}

	if update.Model != "" {
		current.Model = update.Model
	}

	if update.Manufacturer != "" {
		current.Manufacturer = update.Manufacturer
	}

	if update.SWVersion != "" {
		current.SWVersion = update.SWVersion
	}

	return current
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

	r.applySensorValue(n, ch, value)
}

// applySensorValue takes in a reading, in the sensor's own unit, however it arrived.
func (r *registry) applySensorValue(n *node, ch *sensorChannel, value float32) {
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
	r.applyConnectivity(n, &d.Data.Connectivity, &d.Data.OnlineSince, &d.Data.OfflineSince)

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

	if handle := r.handles[n.name]; handle != nil {
		handle.doorRegistered(id, d.Data.Name, d.GetStub(r.container))
	}
}

func (r *registry) deregisterDoor(n *node, ch *doorChannel) {
	if ch.door == nil {
		return
	}

	if handle := r.handles[n.name]; handle != nil {
		handle.doorDeregistered(ch.door.PmaasEntityId)
	}

	if err := r.container.DeregisterEntity(ch.door.PmaasEntityId); err != nil {
		r.logf("unable to deregister door sensor %s: %v", ch.door.PmaasEntityId, err)
	}

	ch.door.CloseStubIfPresent()
	ch.door = nil
}

// applyConnectivity copies the device's connectivity into an entity's data.
func (r *registry) applyConnectivity(n *node, connectivity *environment.Connectivity, onlineSince *time.Time, offlineSince *time.Time) {
	*onlineSince = n.onlineSince
	*offlineSince = n.offlineSince

	if n.connected {
		*connectivity = environment.ConnectivityOnline
	} else {
		*connectivity = environment.ConnectivityOffline
	}
}

// updateConnectivity brings every entity of a device up to date with whether the device is connected.
func (r *registry) updateConnectivity(n *node) {
	for _, ch := range n.doors {
		r.updateDoorConnectivity(n, ch)
	}

	for _, ch := range n.switches {
		r.updateSwitchConnectivity(n, ch)
	}

	r.refreshPowerMonitor(n)
}

func (r *registry) updateDoorConnectivity(n *node, ch *doorChannel) {
	if ch.door == nil {
		return
	}

	r.applyConnectivity(n, &ch.door.Data.Connectivity, &ch.door.Data.OnlineSince, &ch.door.Data.OfflineSince)
	r.broadcastState(ch.door.PmaasEntityId, device.IDoorSensorType, ch.door.Data.Name, ch.door.Data)
}

func (r *registry) handleDoorState(n *node, ch *doorChannel, payload []byte) {
	if ch.door == nil {
		return
	}

	// Home Assistant's convention for door-like binary sensors: on means open.
	switch strings.TrimSpace(string(payload)) {
	case ch.disc.StatePayloadOn:
		r.applyDoorOpen(n, ch, true)
	case ch.disc.StatePayloadOff:
		r.applyDoorOpen(n, ch, false)
	default:
		r.logf("%s: unrecognized door state %q on %s", n.name, payload, ch.disc.StateTopic)
	}
}

// applyDoorOpen takes in whether the door is open, however it arrived.
func (r *registry) applyDoorOpen(n *node, ch *doorChannel, open bool) {
	if ch.door == nil {
		return
	}

	position := device.DoorPositionClosed
	if open {
		position = device.DoorPositionOpen
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

// deviceDisplayName is what the device is called: what it was configured to be called, or its name.
func (r *registry) deviceDisplayName(deviceName string) string {
	if name, ok := r.displayNames[deviceName]; ok {
		return name
	}

	return deviceName
}
