package esphome

import (
	"net"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/events"
)

// A smart plug as ESPHome announces it: a relay, and power monitoring, under the device's own topics.
const (
	plugRelayConfig  = `{"name":"Relay","stat_t":"plug1/switch/relay/state","cmd_t":"plug1/switch/relay/command","avty_t":"plug1/status","uniq_id":"ESPswitchrelay","dev":{"ids":"aabbccddeeff","name":"Plug1","sw":"2026.8.0","mdl":"plf10","mf":"Kauf"}}`
	plugPowerConfig  = `{"name":"Power","dev_cla":"power","unit_of_meas":"W","stat_t":"plug1/sensor/power/state","uniq_id":"ESPsensorpower","dev":{"ids":"aabbccddeeff","name":"Plug1"}}`
	plugEnergyConfig = `{"name":"Energy","dev_cla":"energy","unit_of_meas":"Wh","stat_t":"plug1/sensor/energy/state","uniq_id":"ESPsensorenergy","dev":{"ids":"aabbccddeeff","name":"Plug1"}}`
)

// stubFor returns the stub registered for an entity type, creating it on the plugin goroutine as the container
// does.
func (c *pluginContainer) stubFor(t *testing.T, entityType any) any {
	t.Helper()

	var stub any
	var err error

	c.onPluginGoroutine(func() {
		for _, r := range c.registrations {
			if r.entityType == entityType {
				stub, err = r.stubFactory()
				return
			}
		}

		t.Errorf("nothing registered as %v", entityType)
	})

	if err != nil {
		t.Fatal(err)
	}

	return stub
}

func TestPlugin_ASwitchIsCommandedThroughTheBrokerAndTheDeviceReportsTheResult(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "plug1", Password: "secret1", DisplayName: "Heater"})

	p, c := startPlugin(t, config)

	commands := make(chan string, 8)
	opts := paho.NewClientOptions().
		AddBroker("tcp://" + config.ListenAddress).
		SetClientID("plug1-aabbccddeeff").
		SetUsername("plug1").
		SetPassword("secret1").
		SetAutoReconnect(false)

	client := paho.NewClient(opts)

	if token := client.Connect(); !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("device connect: %v", token.Error())
	}

	// ESPHome subscribes to the command topic of each switch when it connects, before it announces anything.
	subscribed := client.Subscribe("plug1/switch/relay/command", 1, func(_ paho.Client, m paho.Message) {
		commands <- string(m.Payload())
	})
	if !subscribed.WaitTimeout(2*time.Second) || subscribed.Error() != nil {
		t.Fatalf("subscribe: %v", subscribed.Error())
	}

	publish := func(topic string, payload string) {
		token := client.Publish(topic, 0, true, payload)
		token.WaitTimeout(2 * time.Second)

		if err := token.Error(); err != nil {
			t.Fatal(err)
		}
	}

	publish("plug1/status", "online")
	publish("homeassistant/switch/plug1/relay/config", plugRelayConfig)
	publish("homeassistant/sensor/plug1/power/config", plugPowerConfig)
	publish("homeassistant/sensor/plug1/energy/config", plugEnergyConfig)
	publish("plug1/switch/relay/state", "OFF")
	publish("plug1/sensor/power/state", "1500")
	publish("plug1/sensor/energy/state", "2500")

	var monitor device.PowerMonitor

	eventually(t, c, "the relay's state and the power readings", func() bool {
		var relay device.Switch

		monitor = device.PowerMonitor{}

		for _, b := range c.broadcasts {
			if e, ok := b.event.(events.EntityStateChangedEvent); ok {
				switch s := e.NewState.(type) {
				case device.Switch:
					relay = s
				case device.PowerMonitor:
					monitor = s
				}
			}
		}

		return relay.HasData && monitor.HasPower && monitor.HasEnergy
	})

	if monitor.Power != 1500 || monitor.Energy != 2.5 || monitor.HasVoltage || monitor.Name != "Heater Power" {
		t.Fatalf("unexpected power monitor %+v", monitor)
	}

	relay := c.stubFor(t, device.ISwitchType).(device.ISwitch)

	if got := relay.GetSwitchData(); got.State != device.SwitchStateOff || got.Name != "Heater Relay" {
		t.Fatalf("unexpected switch %+v", got)
	}

	// Turn it on: the request goes out through the broker to the device, which is subscribed to its own
	// command topic, and the state doesn't change until the device says it has.
	if err := relay.SetState(device.SwitchStateOn); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-commands:
		if got != "ON" {
			t.Fatalf("the device was told %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command never reached the device")
	}

	if got := relay.GetSwitchData().State; got != device.SwitchStateOff {
		t.Fatalf("the state changed before the device reported it: %v", got)
	}

	publish("plug1/switch/relay/state", "ON")

	eventually(t, c, "the device's report", func() bool {
		for _, b := range c.broadcasts {
			if change, ok := b.event.(device.SwitchStateChangedEvent); ok && change.NewState == device.SwitchStateOn {
				return true
			}
		}

		return false
	})

	if got := relay.GetSwitchData().State; got != device.SwitchStateOn {
		t.Fatalf("unexpected state %v", got)
	}

	// A command still queued when the plugin stops is handed to the broker before it closes, so stopping doesn't
	// hang on it. Whether the device gets it isn't promised: the broker may close its connection first.
	if err := relay.SetState(device.SwitchStateOff); err != nil {
		t.Fatal(err)
	}

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

	if conn, err := net.DialTimeout("tcp", config.ListenAddress, time.Second); err == nil {
		_ = conn.Close()
		t.Error("the broker is still listening after Stop")
	}
}

func TestPlugin_PublishCommandFailsWithoutABroker(t *testing.T) {
	p := &plugin{}

	if err := p.publishCommand("plug1/switch/relay/command", []byte("ON")); err != errBrokerNotRunning {
		t.Fatalf("got %v", err)
	}
}
