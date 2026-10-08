package esphome

import (
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/avanha/pmaas-common/mailbox"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
)

// pluginContainer is a fakeContainer whose plugin goroutine is a real serial one, so the broker's
// goroutines hand work over the way they do in PMAAS. The embedded fakeContainer's fields belong to
// that goroutine: read them only through onPluginGoroutine.
type pluginContainer struct {
	fakeContainer
	mailbox *mailbox.Mailbox
}

func (c *pluginContainer) EnqueueOnPluginGoRoutine(f func()) error {
	return c.mailbox.Send(f)
}

func (c *pluginContainer) ClosedCallbackChannel() chan func() {
	ch := make(chan func())
	close(ch)

	return ch
}

func (c *pluginContainer) onPluginGoroutine(f func()) {
	if err := c.mailbox.ExecVoidFn(f); err != nil {
		panic(err)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	return l.Addr().String()
}

func startPlugin(t *testing.T, config PluginConfig) (*plugin, *pluginContainer) {
	t.Helper()

	c := &pluginContainer{mailbox: mailbox.NewMailbox()}
	t.Cleanup(c.mailbox.Stop)

	p := NewPlugin(config).(*plugin)
	p.registry = nil
	p.Init(c)
	p.registry.logf = func(string, ...any) {}

	c.onPluginGoroutine(p.Start)

	return p, c
}

func eventually(t *testing.T, c *pluginContainer, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		var ok bool
		c.onPluginGoroutine(func() { ok = condition() })

		if ok {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// What an ESP32 running ESPHome 2026.8.0 publishes when it connects, as captured from a real device (an SHT3x
// for temperature and humidity, a binary sensor and a status LED).
const (
	espNodeDiscoveryAnswer = `{"ip":"10.47.7.17","name":"garagedoor1","friendly_name":"GarageDoor1","version":"2026.8.0","mac":"20500de464ec","platform":"ESP32","board":"esp32","network":"wifi"}`

	espTemperatureConfig = `{"sug_dsp_prc":1,"unit_of_meas":"°C","stat_cla":"measurement","name":"Garage Temperature","dev_cla":"temperature","stat_t":"garagedoor1/sensor/garage_temperature/state","avty_t":"garagedoor1/status","uniq_id":"ESPsensorgarage_temperature","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x0dd1e1e6)","mdl":"esp32","mf":"Espressif","cns":[["mac","20500de464ec"]]}}`
	espHumidityConfig    = `{"sug_dsp_prc":1,"unit_of_meas":"%","stat_cla":"measurement","name":"Garage Humidity","dev_cla":"humidity","stat_t":"garagedoor1/sensor/garage_humidity/state","avty_t":"garagedoor1/status","uniq_id":"ESPsensorgarage_humidity","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x0dd1e1e6)","mdl":"esp32","mf":"Espressif","cns":[["mac","20500de464ec"]]}}`

	// The captured binary sensor had no dev_cla, because its YAML didn't set a device_class; this is the same
	// message with the garage_door class the README tells you to set.
	espDoorConfig = `{"name":"Garage Door Position","dev_cla":"garage_door","stat_t":"garagedoor1/binary_sensor/garage_door_position/state","avty_t":"garagedoor1/status","uniq_id":"ESPbinary_sensorgarage_door_position","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x0dd1e1e6)","mdl":"esp32","mf":"Espressif","cns":[["mac","20500de464ec"]]}}`

	espStatusLedConfig = `{"schema":"json","supported_color_modes":["onoff"],"name":"Garage ESP Status LED","stat_t":"garagedoor1/light/garage_esp_status_led/state","cmd_t":"garagedoor1/light/garage_esp_status_led/command","avty_t":"garagedoor1/status","uniq_id":"ESPlightgarage_esp_status_led","dev":{"ids":"20500de464ec","name":"GarageDoor1","sw":"2026.8.0 (config hash 0x0dd1e1e6)","mdl":"esp32","mf":"Espressif","cns":[["mac","20500de464ec"]]}}`
	espStatusLedState  = `{"color_mode":"onoff","state":"OFF","color":{}}`
)

func TestPlugin_EndToEndWithADeviceAsESPHomeActuallyBehaves(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "garagedoor1", Password: "secret1", DisplayName: "Double Garage Door"})

	p, c := startPlugin(t, config)

	client, err := func() (paho.Client, error) {
		opts := paho.NewClientOptions().
			AddBroker("tcp://"+config.ListenAddress).
			SetClientID("garagedoor1-20500de464ec").
			SetUsername("garagedoor1").
			SetPassword("secret1").
			SetAutoReconnect(false).
			SetWill("garagedoor1/status", "offline", 0, true)

		cl := paho.NewClient(opts)
		token := cl.Connect()
		token.WaitTimeout(5 * time.Second)

		return cl, token.Error()
	}()
	if err != nil {
		t.Fatalf("device connect: %v", err)
	}

	publish := func(topic string, payload string, qos byte) {
		token := client.Publish(topic, qos, true, payload)
		token.WaitTimeout(2 * time.Second)

		if err := token.Error(); err != nil {
			t.Fatal(err)
		}
	}

	// What ESPHome does on connect, in the order it did it: its node discovery (a QoS 1 publish outside the
	// device's own topics, which used to get it disconnected), the birth message, the retained discovery
	// configs, then state.
	publish("esphome/discover/garagedoor1", espNodeDiscoveryAnswer, 1)
	publish("garagedoor1/status", "online", 0)
	publish("homeassistant/sensor/garagedoor1/garage_temperature/config", espTemperatureConfig, 0)
	publish("garagedoor1/sensor/garage_temperature/state", "23.8", 0)
	publish("homeassistant/sensor/garagedoor1/garage_humidity/config", espHumidityConfig, 0)
	publish("garagedoor1/sensor/garage_humidity/state", "39.1", 0)
	publish("homeassistant/binary_sensor/garagedoor1/garage_door_position/config", espDoorConfig, 0)
	publish("garagedoor1/binary_sensor/garage_door_position/state", "OFF", 0)
	publish("homeassistant/light/garagedoor1/garage_esp_status_led/config", espStatusLedConfig, 0)
	publish("garagedoor1/light/garage_esp_status_led/state", espStatusLedState, 0)

	if !client.IsConnected() {
		t.Fatal("the device was disconnected")
	}

	var thermometer environment.WirelessThermometer
	var door device.DoorSensor

	eventually(t, c, "thermometer and door state", func() bool {
		thermometer, door = environment.WirelessThermometer{}, device.DoorSensor{}

		for _, b := range c.broadcasts {
			if e, ok := b.event.(events.EntityStateChangedEvent); ok {
				switch s := e.NewState.(type) {
				case environment.WirelessThermometer:
					thermometer = s
				case device.DoorSensor:
					door = s
				}
			}
		}

		return thermometer.SensorData.HasHumidity && door.HasData
	})

	c.onPluginGoroutine(func() {
		if len(c.registrations) != 2 {
			t.Errorf("expected a thermometer and a door sensor (and nothing for the LED), got %+v", c.registrations)
		}
	})

	if thermometer.Name != "Double Garage Door Garage Temperature" || thermometer.SensorData.Temperature != 23.8 ||
		thermometer.SensorData.Humidity != 39.1 {
		t.Errorf("unexpected thermometer %+v", thermometer)
	}

	if door.Name != "Double Garage Door Garage Door Position" || door.Position != device.DoorPositionClosed ||
		door.Connectivity != environment.ConnectivityOnline {
		t.Errorf("unexpected door %+v", door)
	}

	// Open the door.
	publish("garagedoor1/binary_sensor/garage_door_position/state", "ON", 0)

	eventually(t, c, "the door opening to be announced", func() bool {
		for _, b := range c.broadcasts {
			if change, ok := b.event.(device.DoorPositionChangedEvent); ok &&
				change.NewPosition == device.DoorPositionOpen && change.OldPosition == device.DoorPositionClosed {
				return true
			}
		}

		return false
	})

	// The device drops off the network.
	client.Disconnect(0)

	eventually(t, c, "the door to be marked offline", func() bool {
		last := c.broadcasts[len(c.broadcasts)-1].event
		e, ok := last.(events.EntityStateChangedEvent)

		if !ok {
			return false
		}

		s, ok := e.NewState.(device.DoorSensor)

		return ok && s.Connectivity == environment.ConnectivityOffline && s.Position == device.DoorPositionOpen
	})

	// Stopping deregisters everything, and the returned channel closes once the broker is down.
	var done chan func()
	c.onPluginGoroutine(func() { done = p.Stop() })

	select {
	case _, open := <-done:
		if open {
			t.Fatal("unexpected callback")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete")
	}

	c.onPluginGoroutine(func() {
		if len(c.deregistered) != 2 {
			t.Errorf("expected both entities deregistered, got %v", c.deregistered)
		}
	})

	if conn, err := net.DialTimeout("tcp", config.ListenAddress, time.Second); err == nil {
		_ = conn.Close()
		t.Error("the broker is still listening after Stop")
	}
}

func TestPlugin_RefusesDevicesNotConfigured(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "garage1", Password: "secret1"})

	p, c := startPlugin(t, config)
	defer c.onPluginGoroutine(func() { <-p.Stop() })

	opts := paho.NewClientOptions().
		AddBroker("tcp://" + config.ListenAddress).
		SetClientID("x").SetUsername("garage1").SetPassword("wrong").
		SetAutoReconnect(false).SetConnectRetry(false)

	client := paho.NewClient(opts)
	token := client.Connect()
	token.WaitTimeout(5 * time.Second)

	if token.Error() == nil {
		client.Disconnect(0)
		t.Fatal("expected a device with the wrong password to be refused")
	}
}

func TestPlugin_StartWithoutDevicesDoesNotListen(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)

	p, c := startPlugin(t, config)

	if conn, err := net.DialTimeout("tcp", config.ListenAddress, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("the broker started with no devices configured")
	}

	var done chan func()
	c.onPluginGoroutine(func() { done = p.Stop() })

	if _, open := <-done; open {
		t.Fatal("unexpected callback")
	}
}

func TestPlugin_StartWithBadConfigurationDoesNotListenOrPanic(t *testing.T) {
	inUse, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inUse.Close()

	for name, config := range map[string]PluginConfig{
		"address in use": {
			ListenAddress: inUse.Addr().String(),
			Devices:       []DeviceConfig{{Name: "garage1", Password: "x"}},
		},
		"duplicate device": {
			ListenAddress: freeAddress(t),
			Devices:       []DeviceConfig{{Name: "garage1", Password: "x"}, {Name: "garage1", Password: "y"}},
		},
		"invalid device name": {
			ListenAddress: freeAddress(t),
			Devices:       []DeviceConfig{{Name: "garage/1", Password: "x"}},
		},
		"missing password": {
			ListenAddress: freeAddress(t),
			Devices:       []DeviceConfig{{Name: "garage1"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, c := startPlugin(t, config)

			c.onPluginGoroutine(func() {
				if p.broker != nil {
					t.Error("the broker started")
				}
			})

			// A rejected configuration mustn't leave a socket bound. (The address-in-use case
			// is the test's own listener, which is expected to still be there.)
			if name != "address in use" {
				if conn, err := net.DialTimeout("tcp", config.ListenAddress, 200*time.Millisecond); err == nil {
					_ = conn.Close()
					t.Error("a socket was left listening")
				}
			}

			var done chan func()
			c.onPluginGoroutine(func() { done = p.Stop() })

			if _, open := <-done; open {
				t.Fatal("unexpected callback")
			}
		})
	}
}

// A device the broker refuses something just keeps reconnecting, so what the broker refuses has to reach
// the log. The broker only logs where it's told to, which is what this guards.
func TestPlugin_LogsWhatTheBrokerRefuses(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "garage1", Password: "secret1"})

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	original := os.Stdout
	os.Stdout = writer // the broker's logger is created when the plugin starts

	defer func() { os.Stdout = original }()

	p, c := startPlugin(t, config)

	client := paho.NewClient(paho.NewClientOptions().
		AddBroker("tcp://" + config.ListenAddress).SetClientID("c").SetUsername("garage1").SetPassword("secret1").
		SetAutoReconnect(false))

	if token := client.Connect(); !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("connect: %v", token.Error())
	}

	// QoS 0, so the device stays connected while the refusal is logged.
	client.Publish("homeassistant/sensor/garage2/temp/config", 0, false, "{}").WaitTimeout(time.Second)
	time.Sleep(300 * time.Millisecond)

	client.Disconnect(0)

	// Take the channel out of the plugin goroutine before waiting on it: closing the broker can need that
	// goroutine to run what a connection is still handing it, so waiting from inside it would deadlock.
	var done chan func()
	c.onPluginGoroutine(func() { done = p.Stop() })
	<-done

	os.Stdout = original
	_ = writer.Close()

	output, _ := io.ReadAll(reader)

	if !strings.Contains(string(output), "denying publish") ||
		!strings.Contains(string(output), "homeassistant/sensor/garage2/temp/config") {
		t.Fatalf("the refusal never reached the log:\n%s", output)
	}
}

func TestBrokerLogger_GivesTheUnnamedConnectionWarningSomeWords(t *testing.T) {
	var out strings.Builder

	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{
		Level:       slog.LevelWarn,
		ReplaceAttr: describeUnnamedBrokerWarnings,
	}))

	logger.Warn("", "listener", "esphome", "error", io.EOF)
	logger.Warn("something with a message of its own", "k", "v")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected output %q", out.String())
	}

	if !strings.Contains(lines[0], "a device's connection ended with an error") || !strings.Contains(lines[0], "error=EOF") ||
		strings.Contains(lines[0], `msg=""`) {
		t.Errorf("the unnamed warning wasn't described: %q", lines[0])
	}

	if !strings.Contains(lines[1], `msg="something with a message of its own"`) {
		t.Errorf("a warning that already had a message was changed: %q", lines[1])
	}
}

func TestBrokerLogger_IsWiredToDescribeUnnamedWarnings(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	original := os.Stdout
	os.Stdout = writer

	defer func() { os.Stdout = original }()

	brokerLogger().Warn("", "listener", "esphome", "error", io.EOF) // the logger writes to stdout as of creation
	brokerLogger().Info("not shown: below warning level")

	os.Stdout = original
	_ = writer.Close()

	output, _ := io.ReadAll(reader)

	if !strings.Contains(string(output), "a device's connection ended with an error") || strings.Contains(string(output), "not shown") {
		t.Fatalf("unexpected output %q", output)
	}
}
