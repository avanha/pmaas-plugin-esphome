package esphome

import (
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
	"github.com/avanha/pmaas-spi/device"
)

// A device reached over the native API describes its entities and reports their states as typed messages, not as
// topics and payload strings, but what PMAAS makes of them is the same, so each is turned into what an MQTT
// device's announcement would have said and goes through the same registration and the same state handling.

// nativeRef names an entity the way the registry does: the kind of thing it is, and what the device calls it.
type nativeRef struct {
	component string
	objectID  string
}

// nativeState is what the registry keeps for a device that's reached over the native API.
type nativeState struct {
	// keys maps the number the device identifies each entity by to the entity, for the states that name one.
	keys map[uint32]nativeRef

	// listing is the entities the device has described since the last time it said it had finished, which is
	// what's left of what it described before when it's done: anything not in it has been removed.
	listing map[nativeRef]bool

	// ignored counts, by kind, what the device has described in this listing that PMAAS doesn't track, and hidden
	// those it marks as its own configuration or has disabled. They're reported in one line when it's done.
	ignored map[string]int
	hidden  int
}

// HandleNativeMessage processes one message a device sent over the native API.
func (r *registry) HandleNativeMessage(deviceName string, message proto.Message) {
	if r.stopped {
		return
	}

	n := r.nodeFor(deviceName)
	n.lastMessage = r.now()

	switch m := message.(type) {
	case *pb.DeviceInfoResponse:
		n.info = mergeDeviceInfo(n.info, nativeDeviceInfo(m))
		r.updateNames(n)
	case *pb.ListEntitiesSensorResponse:
		if hiddenNativeEntity(m.EntityCategory, m.DisabledByDefault) {
			n.native.hidden++
			return
		}

		r.describeNative(n, m.Key, hadiscovery.Entity{
			Component: "sensor", ObjectID: r.nativeObjectID(n, "sensor", m.ObjectId, m.Name, m.Key), Name: m.Name,
			DeviceClass: m.DeviceClass, UnitOfMeasurement: m.UnitOfMeasurement,
		})
	case *pb.ListEntitiesBinarySensorResponse:
		if hiddenNativeEntity(m.EntityCategory, m.DisabledByDefault) {
			n.native.hidden++
			return
		}

		r.describeNative(n, m.Key, hadiscovery.Entity{
			Component: "binary_sensor", ObjectID: r.nativeObjectID(n, "binary_sensor", m.ObjectId, m.Name, m.Key),
			Name: m.Name, DeviceClass: m.DeviceClass,
		})
	case *pb.ListEntitiesSwitchResponse:
		if hiddenNativeEntity(m.EntityCategory, m.DisabledByDefault) {
			n.native.hidden++
			return
		}

		r.describeNative(n, m.Key, hadiscovery.Entity{
			Component: "switch", ObjectID: r.nativeObjectID(n, "switch", m.ObjectId, m.Name, m.Key),
			Name: m.Name, DeviceClass: m.DeviceClass,
		})
	case *pb.ListEntitiesDoneResponse:
		r.finishNativeListing(n)
	case *pb.SensorStateResponse:
		if !m.MissingState {
			r.applyNativeSensor(n, m.Key, float64(m.State))
		}
	case *pb.BinarySensorStateResponse:
		if !m.MissingState {
			r.applyNativeBinarySensor(n, m.Key, m.State)
		}
	case *pb.SwitchStateResponse:
		if !m.MissingState {
			r.applyNativeSwitch(n, m.Key, m.State)
		}
	default:
		r.countIgnoredNative(n, message)
	}
}

var nonIdentifierPattern = regexp.MustCompile(`[^a-z0-9]+`)

// nativeObjectID is what the registry calls an entity the device describes: what the device calls it, the object
// id, which is what everything else is keyed by.
//
// Not every device sends one. A Kauf plug on ESPHome 2025.8.2 sent none for any of its sensors, and every sensor
// then had the same "name", so they all shared one entry in the registry and the last to arrive won: the plug's
// voltage was shown as its power. So when there isn't one it's worked out from the entity's name the way ESPHome
// does, in lower case with underscores between words, which is what the device would have sent, so an entity is the
// same whether or not it does. And when two entities still end up with the same one (whatever the reason) the
// second has its key, which the device makes unique, put on the end, so they can never share an entry.
func (r *registry) nativeObjectID(n *node, component string, objectID string, name string, key uint32) string {
	id := objectID

	// Called by the same name as last time, which is what an entity is: a device that listed it twice with a
	// different order mustn't swap what's registered under each.
	if id == "" {
		if existing, known := n.native.keys[key]; known && existing.component == component {
			return existing.objectID
		}
	}

	if id == "" {
		id = strings.Trim(nonIdentifierPattern.ReplaceAllString(strings.ToLower(name), "_"), "_")
	}

	if id == "" {
		return fmt.Sprintf("%s_%d", component, key)
	}

	if n.native.listing[nativeRef{component, id}] {
		if existing, known := n.native.keys[key]; !known || existing != (nativeRef{component, id}) {
			return fmt.Sprintf("%s_%d", id, key)
		}
	}

	return id
}

// hiddenNativeEntity is whether a device marks an entity as one that isn't for ordinary use: a setting of the
// device itself (the LEDs on a Kauf plug, "No HASS", "Early Publish") or a diagnostic, and usually also disabled by
// default. Home Assistant hides these, and so does this: a plug's own configuration shouldn't turn up as
// switches that could be flipped by mistake. What it doesn't list never becomes an entity, and if one that was
// listed before is hidden now it's dropped like anything else the device no longer has.
func hiddenNativeEntity(category pb.EntityCategory, disabledByDefault bool) bool {
	return disabledByDefault || category != pb.EntityCategory_ENTITY_CATEGORY_NONE
}

// nativeDeviceInfo is what a device says about itself, as an MQTT device would have put it in its device block.
func nativeDeviceInfo(info *pb.DeviceInfoResponse) hadiscovery.Device {
	device := hadiscovery.Device{
		Name:         info.FriendlyName,
		Model:        info.Model,
		Manufacturer: info.Manufacturer,
		SWVersion:    info.EsphomeVersion,
	}

	if device.Name == "" {
		device.Name = info.Name
	}

	// ESPHome's identifier for a device is its MAC address as twelve hex digits.
	if mac := strings.ReplaceAll(info.MacAddress, ":", ""); mac != "" {
		device.Identifiers = []string{mac}
	}

	return device
}

// updateNames gives every entity of a device the name it would be given now: what the device calls itself is part
// of what each is called, and is only known once it has said.
func (r *registry) updateNames(n *node) {
	for _, ch := range n.temps {
		ch.disc.Device = n.info
	}

	for _, ch := range n.humids {
		ch.disc.Device = n.info
	}

	for _, ch := range n.powerSensors {
		ch.disc.Device = n.info
	}

	for _, ch := range n.doors {
		ch.disc.Device = n.info

		if ch.door != nil {
			r.upsertDoor(n, ch.disc)
		}
	}

	for _, ch := range n.switches {
		ch.disc.Device = n.info

		if ch.sw != nil {
			r.upsertSwitch(n, ch.disc)
		}
	}

	r.refreshThermometers(n)
	r.refreshPowerMonitor(n)
}

// describeNative registers an entity a device has described, and remembers its key so its states can be found.
func (r *registry) describeNative(n *node, key uint32, entity hadiscovery.Entity) {
	entity.Device = n.info

	if !r.upsertEntity(n, entity, false) {
		r.addIgnored(n, entity.Component)

		return
	}

	if n.native.keys == nil {
		n.native.keys = make(map[uint32]nativeRef)
	}

	if n.native.listing == nil {
		n.native.listing = make(map[nativeRef]bool)
	}

	ref := nativeRef{entity.Component, entity.ObjectID}
	n.native.keys[key] = ref
	n.native.listing[ref] = true

	if entity.Component == "switch" {
		if ch := n.switches[entity.ObjectID]; ch != nil {
			ch.native, ch.nativeKey = true, key
		}
	}
}

// finishNativeListing drops what a device no longer has: a device describes all its entities each time it's
// connected to, so one that was there before and isn't now was removed, as an MQTT device says by clearing the
// announcement. It then says in one line what it made of the listing.
func (r *registry) finishNativeListing(n *node) {
	for key, ref := range n.native.keys {
		if n.native.listing[ref] {
			continue
		}

		r.removeChannel(n, hadiscovery.Key{Component: ref.component, ObjectID: ref.objectID})
		delete(n.native.keys, key)
	}

	r.logNativeListing(n)

	n.native.listing = nil
	n.native.ignored = nil
	n.native.hidden = 0
}

// logNativeListing is the one line a device on the native API gets for what it listed: what's tracked, and the
// rest by kind. A stock device lists a couple of dozen entities that are of no use here, such as its numbers,
// selects and buttons, and a line for each every time it connects would bury everything else in the log.
func (r *registry) logNativeListing(n *node) {
	line := fmt.Sprintf("%s: tracking %d entities", n.name, len(n.native.listing))

	if n.native.hidden > 0 {
		line += fmt.Sprintf(", hiding %d marked as configuration or disabled", n.native.hidden)
	}

	if len(n.native.ignored) > 0 {
		kinds := make([]string, 0, len(n.native.ignored))
		total := 0

		for kind, count := range n.native.ignored {
			kinds = append(kinds, kind)
			total += count
		}

		slices.Sort(kinds)

		counted := make([]string, len(kinds))
		for i, kind := range kinds {
			counted[i] = fmt.Sprintf("%d %s", n.native.ignored[kind], kind)
		}

		line += fmt.Sprintf(", ignoring %d it has no use for (%s)", total, strings.Join(counted, ", "))
	}

	r.logf(slog.LevelInfo, "%s", line)
}

func (r *registry) addIgnored(n *node, kind string) {
	if n.native.ignored == nil {
		n.native.ignored = make(map[string]int)
	}

	n.native.ignored[kind]++
}

// countIgnoredNative counts a description of a kind of entity PMAAS doesn't track. Anything else a device sends
// that isn't a description is none of its business.
func (r *registry) countIgnoredNative(n *node, message proto.Message) {
	name := string(message.ProtoReflect().Descriptor().Name())

	kind, isEntity := strings.CutPrefix(name, "ListEntities")
	if !isEntity {
		return
	}

	kind, isEntity = strings.CutSuffix(kind, "Response")
	if !isEntity || kind == "Done" {
		return
	}

	r.addIgnored(n, strings.ToLower(kind))
}

// nativeEntityName is what a description calls its entity, which every kind of description has in a field of the
// same name.
func nativeEntityName(message proto.Message) string {
	reflected := message.ProtoReflect()

	for _, field := range []protoreflect.Name{"name", "object_id"} {
		if descriptor := reflected.Descriptor().Fields().ByName(field); descriptor != nil && reflected.Has(descriptor) {
			return reflected.Get(descriptor).String()
		}
	}

	return ""
}

func (r *registry) applyNativeSensor(n *node, key uint32, value float64) {
	ref, ok := n.native.keys[key]
	if !ok || ref.component != "sensor" {
		return
	}

	if ch, ok := n.temps[ref.objectID]; ok {
		r.applySensorValue(n, &ch.sensorChannel, float32(value))
	}

	if ch, ok := n.humids[ref.objectID]; ok {
		r.applySensorValue(n, ch, float32(value))
	}

	if ch, ok := n.powerSensors[ref.objectID]; ok {
		r.applyPowerValue(n, ch, value)
	}
}

func (r *registry) applyNativeBinarySensor(n *node, key uint32, on bool) {
	ref, ok := n.native.keys[key]
	if !ok || ref.component != "binary_sensor" {
		return
	}

	if ch, ok := n.doors[ref.objectID]; ok {
		// The same convention as for MQTT: on means open.
		r.applyDoorOpen(n, ch, on)
	}
}

func (r *registry) applyNativeSwitch(n *node, key uint32, on bool) {
	ref, ok := n.native.keys[key]
	if !ok || ref.component != "switch" {
		return
	}

	if ch, ok := n.switches[ref.objectID]; ok {
		state := device.SwitchStateOff
		if on {
			state = device.SwitchStateOn
		}

		r.applySwitchState(n, ch, state)
	}
}

// commandNativeSwitch asks a device to turn a switch on or off, by sending it the command, which returns once the
// message is queued. Runs on the plugin goroutine.
func (r *registry) commandNativeSwitch(n *node, ch *switchChannel, name string, state device.SwitchState) error {
	if r.nativeSend == nil {
		return fmt.Errorf("unable to set %q to %s: the native API isn't available", name, state)
	}

	if err := r.nativeSend(n.name, &pb.SwitchCommandRequest{Key: ch.nativeKey, State: state == device.SwitchStateOn}); err != nil {
		return fmt.Errorf("unable to set %q to %s: %w", name, state, err)
	}

	return nil
}
