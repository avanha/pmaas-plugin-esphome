package hadiscovery

import (
	"reflect"
	"testing"
)

// These are discovery configs exactly as an ESP32 running ESPHome 2026.8.0 published them (a Sensirion SHT3x
// for temperature and humidity, a binary sensor, and a status LED), captured from a real device. ESPHome
// writes them with Home Assistant's abbreviated keys, which is the shape the parser has to cope with.
const (
	capturedTemperatureConfig = `{"sug_dsp_prc":1,"unit_of_meas":"°C","stat_cla":"measurement","name":"Garage Temperature","dev_cla":"temperature","stat_t":"garagedoor1/sensor/garage_temperature/state","avty_t":"garagedoor1/status","uniq_id":"ESPsensorgarage_temperature","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x0dd1e1e6)","mdl":"esp32","mf":"Espressif","cns":[["mac","20500de464ec"]]}}`
	capturedHumidityConfig    = `{"sug_dsp_prc":1,"unit_of_meas":"%","stat_cla":"measurement","name":"Garage Humidity","dev_cla":"humidity","stat_t":"garagedoor1/sensor/garage_humidity/state","avty_t":"garagedoor1/status","uniq_id":"ESPsensorgarage_humidity","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x0dd1e1e6)","mdl":"esp32","mf":"Espressif","cns":[["mac","20500de464ec"]]}}`

	// The binary sensor as the device published it: its YAML didn't set a device_class, so there's no dev_cla.
	capturedDoorPositionConfig = `{"name":"Garage Door Position","stat_t":"garagedoor1/binary_sensor/garage_door_position/state","avty_t":"garagedoor1/status","uniq_id":"ESPbinary_sensorgarage_door_position","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x0dd1e1e6)","mdl":"esp32","mf":"Espressif","cns":[["mac","20500de464ec"]]}}`

	capturedStatusLedConfig = `{"schema":"json","supported_color_modes":["onoff"],"name":"Garage ESP Status LED","stat_t":"garagedoor1/light/garage_esp_status_led/state","cmd_t":"garagedoor1/light/garage_esp_status_led/command","avty_t":"garagedoor1/status","uniq_id":"ESPlightgarage_esp_status_led","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x0dd1e1e6)","mdl":"esp32","mf":"Espressif","cns":[["mac","20500de464ec"]]}}`
)

var capturedDevice = Device{
	Identifiers:  []string{"20500de464ec"},
	Name:         "GarageDoor1",
	Model:        "esp32",
	Manufacturer: "Espressif",
	SWVersion:    "2026.8.0 (config hash 0x0dd1e1e6)",
}

func TestParse_CapturedFromARealDevice(t *testing.T) {
	for _, c := range []struct {
		name    string
		topic   string
		payload string
		want    Entity
	}{
		{
			name:    "temperature",
			topic:   "homeassistant/sensor/garagedoor1/garage_temperature/config",
			payload: capturedTemperatureConfig,
			want: Entity{
				Component: "sensor", NodeID: "garagedoor1", ObjectID: "garage_temperature",
				Name: "Garage Temperature", UniqueID: "ESPsensorgarage_temperature",
				StateTopic: "garagedoor1/sensor/garage_temperature/state", AvailabilityTopic: "garagedoor1/status",
				DeviceClass: "temperature", UnitOfMeasurement: "°C",
			},
		},
		{
			name:    "humidity",
			topic:   "homeassistant/sensor/garagedoor1/garage_humidity/config",
			payload: capturedHumidityConfig,
			want: Entity{
				Component: "sensor", NodeID: "garagedoor1", ObjectID: "garage_humidity",
				Name: "Garage Humidity", UniqueID: "ESPsensorgarage_humidity",
				StateTopic: "garagedoor1/sensor/garage_humidity/state", AvailabilityTopic: "garagedoor1/status",
				DeviceClass: "humidity", UnitOfMeasurement: "%",
			},
		},
		{
			name:    "binary sensor with no device class",
			topic:   "homeassistant/binary_sensor/garagedoor1/garage_door_position/config",
			payload: capturedDoorPositionConfig,
			want: Entity{
				Component: "binary_sensor", NodeID: "garagedoor1", ObjectID: "garage_door_position",
				Name: "Garage Door Position", UniqueID: "ESPbinary_sensorgarage_door_position",
				StateTopic: "garagedoor1/binary_sensor/garage_door_position/state", AvailabilityTopic: "garagedoor1/status",
			},
		},
		{
			name:    "status LED, a component PMAAS doesn't use, with a JSON schema and an array",
			topic:   "homeassistant/light/garagedoor1/garage_esp_status_led/config",
			payload: capturedStatusLedConfig,
			want: Entity{
				Component: "light", NodeID: "garagedoor1", ObjectID: "garage_esp_status_led",
				Name: "Garage ESP Status LED", UniqueID: "ESPlightgarage_esp_status_led",
				StateTopic:        "garagedoor1/light/garage_esp_status_led/state",
				CommandTopic:      "garagedoor1/light/garage_esp_status_led/command",
				AvailabilityTopic: "garagedoor1/status",
			},
		},
	} {
		got, key, remove, err := Parse("homeassistant", c.topic, []byte(c.payload))
		if err != nil || remove {
			t.Errorf("%s: err=%v remove=%v", c.name, err, remove)
			continue
		}

		// Everything below is the same for every entity on the device, or a default.
		c.want.Device = capturedDevice
		c.want.StatePayloadOn, c.want.StatePayloadOff = "ON", "OFF"
		c.want.CommandPayloadOn, c.want.CommandPayloadOff = "ON", "OFF"

		if !reflect.DeepEqual(*got, c.want) {
			t.Errorf("%s:\ngot  %+v\nwant %+v", c.name, *got, c.want)
		}

		if key != c.want.Key() {
			t.Errorf("%s: key %+v, want %+v", c.name, key, c.want.Key())
		}
	}
}
