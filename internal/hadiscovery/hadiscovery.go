// Package hadiscovery parses the subset of Home Assistant style MQTT discovery that ESPHome (and
// Tasmota) publish to describe their entities: a retained JSON config per entity at
// <discovery_prefix>/<component>/[<node_id>/]<object_id>/config, with an empty payload meaning the
// entity was removed.
//
// Only what PMAAS needs is read: topics, payload strings, device class and unit, and the device
// block. Both the full key names and Home Assistant's abbreviations are accepted, and the "~" base
// topic substitution is applied. Jinja value templates are not evaluated; they're reported in
// Entity.ValueTemplate so the caller can decide to skip such an entity.
package hadiscovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Entity is one discovered entity.
type Entity struct {
	// Component is the discovery component: "sensor", "binary_sensor", "switch", ...
	Component string
	// NodeID is the optional node segment of the topic; ESPHome uses its device name.
	NodeID   string
	ObjectID string

	Name     string
	UniqueID string

	StateTopic        string
	CommandTopic      string
	AvailabilityTopic string

	DeviceClass       string
	UnitOfMeasurement string

	// StatePayloadOn and StatePayloadOff are the state payloads meaning on and off (binary_sensor
	// and switch). They default to "ON" and "OFF".
	StatePayloadOn  string
	StatePayloadOff string
	// CommandPayloadOn and CommandPayloadOff are what to publish to CommandTopic to turn a switch
	// on and off. They default to "ON" and "OFF".
	CommandPayloadOn  string
	CommandPayloadOff string

	// ValueTemplate is the raw value_template, if any. It is never evaluated here.
	ValueTemplate string

	Device Device
}

// Device is the optional device block grouping entities that belong to one physical device.
type Device struct {
	Identifiers  []string
	Name         string
	Model        string
	Manufacturer string
	SWVersion    string
}

// Key identifies an entity independent of its payload: what a removal refers to.
type Key struct {
	Component string
	NodeID    string
	ObjectID  string
}

func (e Entity) Key() Key {
	return Key{Component: e.Component, NodeID: e.NodeID, ObjectID: e.ObjectID}
}

// ParseTopic splits a discovery config topic into its parts. ok is false if topic isn't a
// discovery config topic under prefix.
func ParseTopic(prefix string, topic string) (key Key, ok bool) {
	rest, found := strings.CutPrefix(topic, prefix+"/")
	if !found {
		return Key{}, false
	}

	rest, found = strings.CutSuffix(rest, "/config")
	if !found {
		return Key{}, false
	}

	parts := strings.Split(rest, "/")

	switch len(parts) {
	case 2:
		key = Key{Component: parts[0], ObjectID: parts[1]}
	case 3:
		key = Key{Component: parts[0], NodeID: parts[1], ObjectID: parts[2]}
	default:
		return Key{}, false
	}

	if key.Component == "" || key.ObjectID == "" || (len(parts) == 3 && key.NodeID == "") {
		return Key{}, false
	}

	return key, true
}

// Parse parses a discovery config message. If the payload is empty it returns remove=true and a
// nil entity: the entity identified by the topic no longer exists.
func Parse(prefix string, topic string, payload []byte) (entity *Entity, key Key, remove bool, err error) {
	key, ok := ParseTopic(prefix, topic)
	if !ok {
		return nil, Key{}, false, fmt.Errorf("%q is not a discovery config topic under %q", topic, prefix)
	}

	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, key, true, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, key, false, fmt.Errorf("invalid discovery payload for %q: %w", topic, err)
	}

	c := config{raw: raw}
	base := c.str("~")

	e := &Entity{
		Component:         key.Component,
		NodeID:            key.NodeID,
		ObjectID:          key.ObjectID,
		Name:              c.str("name"),
		UniqueID:          c.str("unique_id", "uniq_id"),
		StateTopic:        expandBase(base, c.str("state_topic", "stat_t")),
		CommandTopic:      expandBase(base, c.str("command_topic", "cmd_t")),
		AvailabilityTopic: expandBase(base, c.availabilityTopic()),
		DeviceClass:       c.str("device_class", "dev_cla"),
		UnitOfMeasurement: c.str("unit_of_measurement", "unit_of_meas"),
		ValueTemplate:     c.str("value_template", "val_tpl"),
		Device:            c.device(),
	}

	e.CommandPayloadOn = orDefault(c.str("payload_on", "pl_on"), "ON")
	e.CommandPayloadOff = orDefault(c.str("payload_off", "pl_off"), "OFF")
	e.StatePayloadOn = orDefault(c.str("state_on", "stat_on"), e.CommandPayloadOn)
	e.StatePayloadOff = orDefault(c.str("state_off", "stat_off"), e.CommandPayloadOff)

	return e, key, false, nil
}

func orDefault(value string, def string) string {
	if value == "" {
		return def
	}

	return value
}

// expandBase applies Home Assistant's "~" substitution: a leading "~" is replaced by the base
// topic, as is a trailing one.
func expandBase(base string, topic string) string {
	if base == "" || topic == "" {
		return topic
	}

	if rest, ok := strings.CutPrefix(topic, "~"); ok {
		return base + rest
	}

	if rest, ok := strings.CutSuffix(topic, "~"); ok {
		return rest + base
	}

	return topic
}

type config struct {
	raw map[string]json.RawMessage
}

// str returns the first of keys present as a JSON string, or "".
func (c config) str(keys ...string) string {
	for _, key := range keys {
		value, ok := c.raw[key]
		if !ok {
			continue
		}

		var s string
		if err := json.Unmarshal(value, &s); err == nil {
			return s
		}
	}

	return ""
}

// availabilityTopic returns availability_topic, or the first entry's topic of an availability list.
func (c config) availabilityTopic() string {
	if topic := c.str("availability_topic", "avty_t"); topic != "" {
		return topic
	}

	for _, key := range []string{"availability", "avty"} {
		value, ok := c.raw[key]
		if !ok {
			continue
		}

		var list []map[string]json.RawMessage
		if err := json.Unmarshal(value, &list); err != nil || len(list) == 0 {
			continue
		}

		if topic := (config{raw: list[0]}).str("topic", "t"); topic != "" {
			return topic
		}
	}

	return ""
}

func (c config) device() Device {
	for _, key := range []string{"device", "dev"} {
		value, ok := c.raw[key]
		if !ok {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(value, &raw); err != nil {
			continue
		}

		d := config{raw: raw}

		return Device{
			Identifiers:  d.stringOrList("identifiers", "ids"),
			Name:         d.str("name"),
			Model:        d.str("model", "mdl"),
			Manufacturer: d.str("manufacturer", "mf"),
			SWVersion:    d.str("sw_version", "sw"),
		}
	}

	return Device{}
}

// stringOrList reads a value that may be written as a single string or a list of strings.
func (c config) stringOrList(keys ...string) []string {
	for _, key := range keys {
		value, ok := c.raw[key]
		if !ok {
			continue
		}

		var single string
		if err := json.Unmarshal(value, &single); err == nil {
			return []string{single}
		}

		var list []string
		if err := json.Unmarshal(value, &list); err == nil {
			return list
		}
	}

	return nil
}
