package esphome

import (
	"net"
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

// The shapes below follow what ESPHome publishes for these entities (see the README), but have not
// been captured from a real device; replace them with captured fixtures when one is available.
const (
	espTemperatureConfig = `{"name":"Temperature","unique_id":"aabbcc-sensor-temperature","state_topic":"garage1/sensor/temperature/state","availability_topic":"garage1/status","device_class":"temperature","unit_of_measurement":"°C","device":{"identifiers":["aabbccddeeff"],"name":"garage1","model":"esp32dev","sw_version":"2025.9.0"}}`
	espHumidityConfig    = `{"name":"Humidity","unique_id":"aabbcc-sensor-humidity","state_topic":"garage1/sensor/humidity/state","availability_topic":"garage1/status","device_class":"humidity","unit_of_measurement":"%","device":{"identifiers":["aabbccddeeff"],"name":"garage1"}}`
	espDoorConfig        = `{"name":"Door","unique_id":"aabbcc-binary_sensor-door","state_topic":"garage1/binary_sensor/door/state","availability_topic":"garage1/status","device_class":"garage_door","device":{"identifiers":["aabbccddeeff"],"name":"garage1"}}`
)

func TestPlugin_EndToEndWithAnESPHomeStyleDevice(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "garage1", Password: "secret1", DisplayName: "Garage 1"})

	p, c := startPlugin(t, config)

	client, err := func() (paho.Client, error) {
		opts := paho.NewClientOptions().
			AddBroker("tcp://"+config.ListenAddress).
			SetClientID("garage1-aabbcc").
			SetUsername("garage1").
			SetPassword("secret1").
			SetAutoReconnect(false).
			SetWill("garage1/status", "offline", 0, true)

		cl := paho.NewClient(opts)
		token := cl.Connect()
		token.WaitTimeout(5 * time.Second)

		return cl, token.Error()
	}()
	if err != nil {
		t.Fatalf("device connect: %v", err)
	}

	publish := func(topic string, payload string) {
		token := client.Publish(topic, 0, true, payload)
		token.WaitTimeout(2 * time.Second)

		if err := token.Error(); err != nil {
			t.Fatal(err)
		}
	}

	// What ESPHome does on connect: birth message, then the retained discovery configs, then state.
	publish("garage1/status", "online")
	publish("homeassistant/sensor/garage1/temperature/config", espTemperatureConfig)
	publish("homeassistant/sensor/garage1/humidity/config", espHumidityConfig)
	publish("homeassistant/binary_sensor/garage1/door/config", espDoorConfig)
	publish("garage1/sensor/temperature/state", "21.50")
	publish("garage1/sensor/humidity/state", "48.25")
	publish("garage1/binary_sensor/door/state", "OFF")

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
			t.Errorf("expected a thermometer and a door sensor, got %+v", c.registrations)
		}
	})

	if thermometer.Name != "Garage 1 Temperature" || thermometer.SensorData.Temperature != 21.5 ||
		thermometer.SensorData.Humidity != 48.25 {
		t.Errorf("unexpected thermometer %+v", thermometer)
	}

	if door.Name != "Garage 1 Door" || door.Position != device.DoorPositionClosed ||
		door.Connectivity != environment.ConnectivityOnline {
		t.Errorf("unexpected door %+v", door)
	}

	// Open the door.
	publish("garage1/binary_sensor/door/state", "ON")

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
