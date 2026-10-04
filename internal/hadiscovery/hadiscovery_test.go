package hadiscovery

import (
	"reflect"
	"testing"
)

func TestParseTopic(t *testing.T) {
	cases := []struct {
		topic string
		want  Key
		ok    bool
	}{
		{"homeassistant/sensor/garage1/temp/config", Key{"sensor", "garage1", "temp"}, true},
		{"homeassistant/sensor/temp/config", Key{"sensor", "", "temp"}, true},
		{"homeassistant/sensor/garage1/temp/state", Key{}, false},
		{"homeassistant/sensor/config", Key{}, false},
		{"homeassistant/a/b/c/d/config", Key{}, false},
		{"other/sensor/garage1/temp/config", Key{}, false},
		{"homeassistant//garage1/temp/config", Key{}, false},
		{"homeassistant/sensor//temp/config", Key{}, false},
	}

	for _, c := range cases {
		got, ok := ParseTopic("homeassistant", c.topic)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseTopic(%q) = %+v, %v; want %+v, %v", c.topic, got, ok, c.want, c.ok)
		}
	}
}

func TestParse_FullKeys(t *testing.T) {
	payload := `{
		"name": "Garage Temperature",
		"unique_id": "abc123",
		"state_topic": "garage1/sensor/garage_temperature/state",
		"availability_topic": "garage1/status",
		"device_class": "temperature",
		"unit_of_measurement": "°C",
		"device": {"identifiers": ["abc"], "name": "garage1", "model": "esp32dev", "sw_version": "2025.1"}
	}`

	e, key, remove, err := Parse("homeassistant", "homeassistant/sensor/garage1/garage_temperature/config", []byte(payload))
	if err != nil || remove {
		t.Fatalf("unexpected result: err=%v remove=%v", err, remove)
	}

	want := Entity{
		Component: "sensor", NodeID: "garage1", ObjectID: "garage_temperature",
		Name: "Garage Temperature", UniqueID: "abc123",
		StateTopic:        "garage1/sensor/garage_temperature/state",
		AvailabilityTopic: "garage1/status",
		DeviceClass:       "temperature", UnitOfMeasurement: "°C",
		StatePayloadOn: "ON", StatePayloadOff: "OFF", CommandPayloadOn: "ON", CommandPayloadOff: "OFF",
		Device: Device{Identifiers: []string{"abc"}, Name: "garage1", Model: "esp32dev", SWVersion: "2025.1"},
	}
	if !reflect.DeepEqual(*e, want) {
		t.Fatalf("got  %+v\nwant %+v", *e, want)
	}
	if key != (Key{"sensor", "garage1", "garage_temperature"}) || e.Key() != key {
		t.Fatalf("unexpected key %+v", key)
	}
}

func TestParse_AbbreviationsAndBaseTopic(t *testing.T) {
	payload := `{
		"~": "garage1/binary_sensor/door",
		"name": "Door", "uniq_id": "u1",
		"stat_t": "~/state", "cmd_t": "~/command", "avty_t": "garage1/~",
		"dev_cla": "garage_door", "pl_on": "OPEN", "pl_off": "CLOSED",
		"dev": {"ids": "single", "mf": "acme"}
	}`

	e, _, _, err := Parse("homeassistant", "homeassistant/binary_sensor/garage1/door/config", []byte(payload))
	if err != nil {
		t.Fatal(err)
	}

	if e.StateTopic != "garage1/binary_sensor/door/state" || e.CommandTopic != "garage1/binary_sensor/door/command" {
		t.Errorf("base topic not applied: %+v", e)
	}
	if e.AvailabilityTopic != "garage1/garage1/binary_sensor/door" {
		t.Errorf("trailing ~ not applied: %q", e.AvailabilityTopic)
	}
	if e.UniqueID != "u1" || e.DeviceClass != "garage_door" {
		t.Errorf("abbreviated keys not read: %+v", e)
	}
	if e.StatePayloadOn != "OPEN" || e.StatePayloadOff != "CLOSED" {
		t.Errorf("payload_on/off not used as state payloads: %+v", e)
	}
	if !reflect.DeepEqual(e.Device.Identifiers, []string{"single"}) || e.Device.Manufacturer != "acme" {
		t.Errorf("device block not read: %+v", e.Device)
	}
}

func TestParse_SwitchStatePayloadsOverrideCommandPayloads(t *testing.T) {
	payload := `{"name":"Fan","command_topic":"c","state_topic":"s","payload_on":"ON","payload_off":"OFF","state_on":"1","state_off":"0"}`

	e, _, _, err := Parse("homeassistant", "homeassistant/switch/n/fan/config", []byte(payload))
	if err != nil {
		t.Fatal(err)
	}

	if e.CommandPayloadOn != "ON" || e.StatePayloadOn != "1" || e.StatePayloadOff != "0" {
		t.Fatalf("unexpected payloads: %+v", e)
	}
}

func TestParse_AvailabilityList(t *testing.T) {
	payload := `{"name":"x","availability":[{"topic":"n/status"}]}`

	e, _, _, err := Parse("homeassistant", "homeassistant/sensor/n/x/config", []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if e.AvailabilityTopic != "n/status" {
		t.Fatalf("availability list not read: %q", e.AvailabilityTopic)
	}
}

func TestParse_ValueTemplateReported(t *testing.T) {
	payload := `{"name":"x","value_template":"{{ value_json.temp }}"}`

	e, _, _, err := Parse("homeassistant", "homeassistant/sensor/n/x/config", []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if e.ValueTemplate == "" {
		t.Fatal("expected value template to be reported")
	}
}

func TestParse_EmptyPayloadIsRemoval(t *testing.T) {
	for _, payload := range []string{"", "  \n"} {
		e, key, remove, err := Parse("homeassistant", "homeassistant/sensor/n/x/config", []byte(payload))
		if err != nil || !remove || e != nil || key != (Key{"sensor", "n", "x"}) {
			t.Errorf("payload %q: e=%v key=%+v remove=%v err=%v", payload, e, key, remove, err)
		}
	}
}

func TestParse_Errors(t *testing.T) {
	if _, _, _, err := Parse("homeassistant", "homeassistant/sensor/n/x/config", []byte("{not json")); err == nil {
		t.Error("expected error for invalid JSON")
	}
	if _, _, _, err := Parse("homeassistant", "homeassistant/sensor/n/x/state", []byte("{}")); err == nil {
		t.Error("expected error for non-config topic")
	}
}
