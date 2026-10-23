package esphome

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/bluetooth"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
)

var errTest = errors.New("nope")

type registration struct {
	id          string
	uniqueData  string
	entityType  reflect.Type
	name        string
	stubFactory spi.EntityStubFactoryFunc
}

type broadcast struct {
	id    string
	event any
}

// fakeContainer implements just what the registry uses. Calling anything else panics.
type fakeContainer struct {
	spi.IPMAASContainer

	registrations []registration
	views         map[string]spi.EntityViewFunc
	deregistered  []string
	broadcasts    []broadcast
	broadcastMu   sync.Mutex
	registerErr   error
	broadcastErr  error

	// receiver is the clock event receiver the plugin registered, and receiversDeregistered the handles it gave back.
	receiver              events.EventReceiver
	receiversDeregistered []int

	// interestReceiver is the one for what the bluetooth plugin wants.
	interestPredicate events.EventPredicate
	interestReceiver  events.EventReceiver

	// What Init registers for the status page.
	routes        map[string]*spi.HttpHandlerOptions
	jsonRoutes    map[string]spi.JsonHandlerFunc
	rendererTypes []reflect.Type
	staticDir     string
}

func (c *fakeContainer) ProvideContentFS(fs.FS, string) {}

func (c *fakeContainer) EnableStaticContent(staticContentDir string) { c.staticDir = staticContentDir }

func (c *fakeContainer) AddRouteWithOptions(path string, _ http.HandlerFunc, options *spi.HttpHandlerOptions) {
	if c.routes == nil {
		c.routes = make(map[string]*spi.HttpHandlerOptions)
	}

	c.routes[path] = options
}

func (c *fakeContainer) AddJsonRoute(path string, _ spi.RequestObjectFactoryFunc, handler spi.JsonHandlerFunc) {
	if c.jsonRoutes == nil {
		c.jsonRoutes = make(map[string]spi.JsonHandlerFunc)
	}

	c.jsonRoutes[path] = handler
}

func (c *fakeContainer) RegisterEntityRenderer(entityType reflect.Type, _ spi.EntityRendererFactory) {
	c.rendererTypes = append(c.rendererTypes, entityType)
}

func (c *fakeContainer) RegisterEntity(
	uniqueData string, entityType reflect.Type, name string, stubFactoryFn spi.EntityStubFactoryFunc) (string, error) {
	if c.registerErr != nil {
		return "", c.registerErr
	}

	id := "esphome_" + uniqueData
	c.registrations = append(c.registrations, registration{id, uniqueData, entityType, name, stubFactoryFn})

	return id, nil
}

func (c *fakeContainer) RegisterEntityView(id string, view spi.EntityViewFunc) error {
	if c.views == nil {
		c.views = make(map[string]spi.EntityViewFunc)
	}

	c.views[id] = view

	return nil
}

func (c *fakeContainer) DeregisterEntity(id string) error {
	c.deregistered = append(c.deregistered, id)
	return nil
}

func (c *fakeContainer) RegisterEventReceiver(predicate events.EventPredicate, receiver events.EventReceiver) (int, error) {
	c.broadcastMu.Lock()
	defer c.broadcastMu.Unlock()

	// What the bluetooth plugin wants is kept apart from the clock.
	if predicate(&events.EventInfo{Event: bluetooth.InterestEvent{}}) {
		c.interestPredicate, c.interestReceiver = predicate, receiver

		return 8, nil
	}

	c.receiver = receiver

	return 7, nil
}

func (c *fakeContainer) DeregisterEventReceiver(handle int) error {
	if handle == 8 {
		c.interestReceiver = nil
	} else {
		c.receiver = nil
	}

	c.receiversDeregistered = append(c.receiversDeregistered, handle)

	return nil
}

func (c *fakeContainer) ClosedCallbackChannel() chan func() {
	done := make(chan func())
	close(done)

	return done
}

func (c *fakeContainer) BroadcastEvent(id string, event any) error {
	c.broadcastMu.Lock()
	defer c.broadcastMu.Unlock()

	if c.broadcastErr != nil {
		return c.broadcastErr
	}

	c.broadcasts = append(c.broadcasts, broadcast{id, event})
	return nil
}

// broadcastsSoFar is a copy of the broadcasts, safe to take while other goroutines are still broadcasting.
func (c *fakeContainer) broadcastsSoFar() []broadcast {
	c.broadcastMu.Lock()
	defer c.broadcastMu.Unlock()

	return append([]broadcast(nil), c.broadcasts...)
}

// EnqueueOnPluginGoRoutine runs f on another goroutine, as the real one does, so stubs can be
// called from the test goroutine.
func (c *fakeContainer) EnqueueOnPluginGoRoutine(f func()) error {
	go f()
	return nil
}

func (c *fakeContainer) lastBroadcast(t *testing.T) broadcast {
	t.Helper()

	if len(c.broadcasts) == 0 {
		t.Fatal("expected a broadcast, got none")
	}

	return c.broadcasts[len(c.broadcasts)-1]
}

func (c *fakeContainer) stateBroadcasts() []events.EntityStateChangedEvent {
	var result []events.EntityStateChangedEvent

	for _, b := range c.broadcasts {
		if e, ok := b.event.(events.EntityStateChangedEvent); ok {
			result = append(result, e)
		}
	}

	return result
}

type fixture struct {
	t         *testing.T
	container *fakeContainer
	registry  *registry
	clock     time.Time
	logs      []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{t: t, container: &fakeContainer{}, clock: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	f.registry = newRegistry(f.container, "homeassistant", map[string]string{"garage1": "Garage 1"})
	f.registry.now = func() time.Time { return f.clock }
	f.registry.log = slog.New(recordingHandler{&f.logs})

	return f
}

func (f *fixture) advance(d time.Duration) { f.clock = f.clock.Add(d) }

func (f *fixture) discover(device string, component string, object string, payload string) {
	f.registry.HandleMessage(device, fmt.Sprintf("homeassistant/%s/%s/%s/config", component, device, object), []byte(payload))
}

func (f *fixture) state(device string, topic string, payload string) {
	f.registry.HandleMessage(device, topic, []byte(payload))
}

func sensorConfig(device string, object string, name string, class string, unit string) string {
	return fmt.Sprintf(
		`{"name":%q,"state_topic":"%s/sensor/%s/state","device_class":%q,"unit_of_measurement":%q,"device":{"name":%q}}`,
		name, device, object, class, unit, device)
}

func doorConfig(device string, object string, name string) string {
	return fmt.Sprintf(
		`{"name":%q,"state_topic":"%s/binary_sensor/%s/state","device_class":"garage_door"}`, name, device, object)
}

func lastThermometerState(t *testing.T, c *fakeContainer) environment.WirelessThermometer {
	t.Helper()

	states := c.stateBroadcasts()
	if len(states) == 0 {
		t.Fatal("no state broadcasts")
	}

	return states[len(states)-1].NewState.(environment.WirelessThermometer)
}

func TestThermometer_RegisteredOnDiscoveryAndUpdatedOnState(t *testing.T) {
	f := newFixture(t)

	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))

	if len(f.container.registrations) != 1 {
		t.Fatalf("expected one registration, got %+v", f.container.registrations)
	}

	reg := f.container.registrations[0]
	if reg.uniqueData != "garage1/temperature" || reg.entityType != wirelessThermometerType || reg.name != "Garage 1 Temperature" {
		t.Fatalf("unexpected registration %+v", reg)
	}

	if len(f.container.broadcasts) != 0 {
		t.Fatalf("expected no broadcast before the first reading, got %+v", f.container.broadcasts)
	}

	f.advance(time.Second)
	f.state("garage1", "garage1/sensor/temperature/state", "21.5")

	got := lastThermometerState(t, f.container)
	if got.Name != "Garage 1 Temperature" || !got.SensorData.HasData || got.SensorData.Temperature != 21.5 ||
		got.SensorData.HasHumidity || !got.SensorData.LastUpdateTime.Equal(f.clock) {
		t.Fatalf("unexpected state %+v", got)
	}

	// The stub is how the environment plugin reads the initial state of an entity it discovers late.
	stub, err := reg.stubFactory()
	if err != nil {
		t.Fatal(err)
	}

	if stubbed := stub.(environment.IWirelessThermometer).GetWirelessThermometerData(); stubbed != got {
		t.Fatalf("stub returned %+v, want %+v", stubbed, got)
	}
}

func TestThermometer_EveryReadingIsABroadcastEvenIfUnchanged(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))

	f.state("garage1", "garage1/sensor/temperature/state", "21.5")
	f.advance(time.Minute)
	f.state("garage1", "garage1/sensor/temperature/state", "21.5")

	if got := len(f.container.stateBroadcasts()); got != 2 {
		t.Fatalf("expected a broadcast per reading, got %d", got)
	}
}

func TestThermometer_HumidityFoldedInRegardlessOfOrder(t *testing.T) {
	for _, humidityFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("humidityFirst=%v", humidityFirst), func(t *testing.T) {
			f := newFixture(t)
			temp := func() {
				f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))
			}
			humidity := func() {
				f.discover("garage1", "sensor", "humidity", sensorConfig("garage1", "humidity", "Humidity", "humidity", "%"))
			}

			if humidityFirst {
				humidity()
				temp()
			} else {
				temp()
				humidity()
			}

			f.state("garage1", "garage1/sensor/temperature/state", "20")
			f.advance(time.Second)
			f.state("garage1", "garage1/sensor/humidity/state", "45.5")

			if len(f.container.registrations) != 1 {
				t.Fatalf("humidity must not be a separate entity: %+v", f.container.registrations)
			}

			got := lastThermometerState(t, f.container)
			if !got.SensorData.HasHumidity || got.SensorData.Humidity != 45.5 || got.SensorData.Temperature != 20 ||
				!got.SensorData.LastUpdateTime.Equal(f.clock) {
				t.Fatalf("unexpected state %+v", got)
			}
		})
	}
}

func TestThermometer_HumidityNotAttachedWhenAmbiguous(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "t1", sensorConfig("garage1", "t1", "Inside", "temperature", "°C"))
	f.discover("garage1", "sensor", "t2", sensorConfig("garage1", "t2", "Outside", "temperature", "°C"))
	f.discover("garage1", "sensor", "h1", sensorConfig("garage1", "h1", "Humidity", "humidity", "%"))

	f.state("garage1", "garage1/sensor/h1/state", "50")
	f.state("garage1", "garage1/sensor/t1/state", "20")

	if got := lastThermometerState(t, f.container); got.SensorData.HasHumidity {
		t.Fatalf("humidity attached despite two temperature sensors: %+v", got)
	}

	if len(f.container.registrations) != 2 {
		t.Fatalf("expected both thermometers registered, got %+v", f.container.registrations)
	}
}

func TestThermometer_FahrenheitConvertedToCelsius(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°F"))

	f.state("garage1", "garage1/sensor/temperature/state", "68")

	if got := lastThermometerState(t, f.container).SensorData.Temperature; got != 20 {
		t.Fatalf("expected 20, got %v", got)
	}
}

func TestThermometer_UnusableReadingsIgnored(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))

	for _, payload := range []string{"nan", "NaN", "inf", "", "unavailable", "21,5"} {
		f.state("garage1", "garage1/sensor/temperature/state", payload)
	}

	if len(f.container.broadcasts) != 0 {
		t.Fatalf("expected readings to be ignored, got %+v", f.container.broadcasts)
	}
}

func TestThermometer_UnknownUnitIgnored(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "K"))

	if len(f.container.registrations) != 0 {
		t.Fatalf("unexpected registration %+v", f.container.registrations)
	}
}

func TestThermometer_RenamedOnReannouncement(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))
	f.state("garage1", "garage1/sensor/temperature/state", "20")
	before := len(f.container.broadcasts)

	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Air", "temperature", "°C"))

	if len(f.container.registrations) != 1 {
		t.Fatalf("re-announcement must not register again: %+v", f.container.registrations)
	}

	if len(f.container.broadcasts) != before+1 || lastThermometerState(t, f.container).Name != "Garage 1 Air" {
		t.Fatalf("expected a rename broadcast, got %+v", f.container.broadcasts[before:])
	}
}

func TestDiscovery_ReannouncementWithNoChangeIsSilent(t *testing.T) {
	f := newFixture(t)
	config := sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C")

	f.discover("garage1", "sensor", "temperature", config)
	f.state("garage1", "garage1/sensor/temperature/state", "20")
	before := len(f.container.broadcasts)

	f.discover("garage1", "sensor", "temperature", config)

	if len(f.container.registrations) != 1 || len(f.container.broadcasts) != before {
		t.Fatalf("re-announcement was not silent: %+v / %+v", f.container.registrations, f.container.broadcasts[before:])
	}
}

func TestDoor_FirstReportEstablishesPositionAndLaterOnesChangeIt(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.advance(time.Second)
	f.discover("garage1", "binary_sensor", "door", doorConfig("garage1", "door", "Door"))

	if len(f.container.registrations) != 1 ||
		f.container.registrations[0].entityType != device.IDoorSensorType ||
		f.container.registrations[0].name != "Garage 1 Door" {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	// First report: position established, not a change.
	f.advance(time.Second)
	f.state("garage1", "garage1/binary_sensor/door/state", "OFF")

	first := f.container.lastBroadcast(t).event.(events.EntityStateChangedEvent).NewState.(device.DoorSensor)
	if !first.HasData || first.Position != device.DoorPositionClosed || !first.LastChangeTime.IsZero() ||
		first.Connectivity != environment.ConnectivityOnline {
		t.Fatalf("unexpected first state %+v", first)
	}

	for _, b := range f.container.broadcasts {
		if _, isChange := b.event.(device.DoorPositionChangedEvent); isChange {
			t.Fatal("first report must not be announced as a change")
		}
	}

	// Opens.
	f.advance(time.Minute)
	f.state("garage1", "garage1/binary_sensor/door/state", "ON")

	n := len(f.container.broadcasts)
	state := f.container.broadcasts[n-2].event.(events.EntityStateChangedEvent).NewState.(device.DoorSensor)
	change := f.container.broadcasts[n-1].event.(device.DoorPositionChangedEvent)

	if state.Position != device.DoorPositionOpen || !state.LastChangeTime.Equal(f.clock) {
		t.Fatalf("unexpected state after opening %+v", state)
	}

	if change.NewPosition != device.DoorPositionOpen || change.OldPosition != device.DoorPositionClosed ||
		change.Id != "esphome_garage1/door" || change.EntityType != device.IDoorSensorType {
		t.Fatalf("unexpected change event %+v", change)
	}

	// Same position again: nothing to say, but it was heard.
	f.advance(time.Minute)
	f.state("garage1", "garage1/binary_sensor/door/state", "ON")

	if len(f.container.broadcasts) != n {
		t.Fatalf("an unchanged report was broadcast: %+v", f.container.broadcasts[n:])
	}

	stub, _ := f.container.registrations[0].stubFactory()
	if got := stub.(device.IDoorSensor).GetDoorSensorData(); got.LastUpdateTime != f.clock || got.LastChangeTime.Equal(f.clock) {
		t.Fatalf("expected the unchanged report to refresh only LastUpdateTime, got %+v", got)
	}
}

func TestDoor_UnrecognizedPayloadIgnored(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "binary_sensor", "door", doorConfig("garage1", "door", "Door"))

	f.state("garage1", "garage1/binary_sensor/door/state", "AJAR")

	if len(f.container.broadcasts) != 0 {
		t.Fatalf("unexpected broadcast %+v", f.container.broadcasts)
	}
}

func TestDoor_CustomPayloads(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "binary_sensor", "door",
		`{"name":"Door","state_topic":"garage1/binary_sensor/door/state","device_class":"door","payload_on":"OPEN","payload_off":"CLOSED"}`)

	f.state("garage1", "garage1/binary_sensor/door/state", "OPEN")

	got := f.container.lastBroadcast(t).event.(events.EntityStateChangedEvent).NewState.(device.DoorSensor)
	if got.Position != device.DoorPositionOpen {
		t.Fatalf("unexpected state %+v", got)
	}
}

func TestDoor_ConnectivityFollowsTheDevice(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "binary_sensor", "door", doorConfig("garage1", "door", "Door"))

	door := func() device.DoorSensor {
		return f.container.lastBroadcast(t).event.(events.EntityStateChangedEvent).NewState.(device.DoorSensor)
	}

	f.advance(time.Second)
	f.registry.HandleConnected("garage1", "10.0.0.1")

	if got := door(); got.Connectivity != environment.ConnectivityOnline || !got.OnlineSince.Equal(f.clock) {
		t.Fatalf("expected online, got %+v", got)
	}

	f.state("garage1", "garage1/binary_sensor/door/state", "ON")
	f.advance(time.Minute)
	f.registry.HandleDisconnected("garage1")

	got := door()
	if got.Connectivity != environment.ConnectivityOffline || !got.OfflineSince.Equal(f.clock) ||
		got.Position != device.DoorPositionOpen {
		t.Fatalf("expected offline with the last position kept, got %+v", got)
	}
}

func TestDoor_DiscoveredWhileOfflineStartsOffline(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "binary_sensor", "door", doorConfig("garage1", "door", "Door"))

	stub, _ := f.container.registrations[0].stubFactory()

	if got := stub.(device.IDoorSensor).GetDoorSensorData(); got.Connectivity != environment.ConnectivityOffline {
		t.Fatalf("unexpected connectivity %v", got.Connectivity)
	}
}

func TestRemoval_EmptyDiscoveryPayloadDeregisters(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))
	f.discover("garage1", "binary_sensor", "door", doorConfig("garage1", "door", "Door"))

	f.discover("garage1", "sensor", "temperature", "")
	f.discover("garage1", "binary_sensor", "door", "")

	if want := []string{"esphome_garage1/temperature", "esphome_garage1/door"}; !reflect.DeepEqual(f.container.deregistered, want) {
		t.Fatalf("deregistered %v, want %v", f.container.deregistered, want)
	}

	// Their state topics are no longer anyone's.
	before := len(f.container.broadcasts)
	f.state("garage1", "garage1/sensor/temperature/state", "20")
	f.state("garage1", "garage1/binary_sensor/door/state", "ON")

	if len(f.container.broadcasts) != before {
		t.Fatalf("state for removed entities was processed: %+v", f.container.broadcasts[before:])
	}

	// The stub handed out earlier no longer reaches the entity.
	reg := f.container.registrations[0]
	stub, _ := reg.stubFactory()
	if stub == nil {
		t.Fatal("expected a stub")
	}
}

func TestRemoval_ClosesStub(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))
	stub, _ := f.container.registrations[0].stubFactory()
	f.discover("garage1", "sensor", "temperature", "")

	defer func() {
		if recover() == nil {
			t.Fatal("expected a call through a closed stub to panic, like the other plugins' stubs")
		}
	}()

	stub.(environment.IWirelessThermometer).GetWirelessThermometerData()
}

func TestDiscovery_UnusableEntitiesAreIgnoredAndRemovedIfPreviouslyUsable(t *testing.T) {
	f := newFixture(t)

	// State topic outside the device's own tree.
	f.discover("garage1", "sensor", "temperature",
		`{"name":"T","state_topic":"garage2/sensor/temperature/state","device_class":"temperature"}`)
	// Value template.
	f.discover("garage1", "sensor", "humidity",
		`{"name":"H","state_topic":"garage1/sensor/humidity/state","device_class":"humidity","value_template":"{{ value_json.h }}"}`)
	// No state topic.
	f.discover("garage1", "binary_sensor", "door", `{"name":"Door","device_class":"garage_door"}`)

	if len(f.container.registrations) != 0 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	// Usable, then re-announced with a template: no longer tracked.
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))
	f.discover("garage1", "sensor", "temperature",
		`{"name":"T","state_topic":"garage1/sensor/temperature/state","device_class":"temperature","value_template":"{{ v }}"}`)

	if !reflect.DeepEqual(f.container.deregistered, []string{"esphome_garage1/temperature"}) {
		t.Fatalf("expected the previously usable entity to be removed, got %v", f.container.deregistered)
	}
}

func TestDiscovery_UnsupportedEntitiesIgnored(t *testing.T) {
	f := newFixture(t)

	f.discover("garage1", "fan", "fan", `{"name":"Fan","state_topic":"garage1/fan/fan/state","command_topic":"garage1/fan/fan/command"}`)
	f.discover("garage1", "sensor", "co2", sensorConfig("garage1", "co2", "CO2", "carbon_dioxide", "ppm"))
	f.discover("garage1", "sensor", "wifi", sensorConfig("garage1", "wifi", "WiFi", "signal_strength", "dBm"))
	f.discover("garage1", "binary_sensor", "motion",
		`{"name":"Motion","state_topic":"garage1/binary_sensor/motion/state","device_class":"motion"}`)

	if len(f.container.registrations) != 0 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}
}

func TestDiscovery_ConfigForAnotherNodeIgnored(t *testing.T) {
	f := newFixture(t)

	f.registry.HandleMessage("garage1", "homeassistant/sensor/garage2/temperature/config",
		[]byte(sensorConfig("garage2", "temperature", "Temperature", "temperature", "°C")))

	if len(f.container.registrations) != 0 {
		t.Fatalf("a device registered an entity for another node: %+v", f.container.registrations)
	}
}

func TestDiscovery_StateOfOneDeviceNeverReachesAnother(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))

	f.state("garage2", "garage1/sensor/temperature/state", "99")

	if len(f.container.broadcasts) != 0 {
		t.Fatalf("garage2 influenced garage1's entity: %+v", f.container.broadcasts)
	}
}

func TestRegistration_FailureIsRetriedOnNextAnnouncement(t *testing.T) {
	f := newFixture(t)
	f.container.registerErr = errors.New("nope")
	config := sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C")

	f.discover("garage1", "sensor", "temperature", config)
	f.state("garage1", "garage1/sensor/temperature/state", "20")

	if len(f.container.registrations) != 0 || len(f.container.broadcasts) != 0 {
		t.Fatalf("an unregistered entity was used: %+v / %+v", f.container.registrations, f.container.broadcasts)
	}

	f.container.registerErr = nil
	f.discover("garage1", "sensor", "temperature", config)

	if len(f.container.registrations) != 1 {
		t.Fatalf("expected the registration to be retried, got %+v", f.container.registrations)
	}

	// The reading that arrived while it was unregistered was kept.
	if got := lastThermometerState(t, f.container); !got.SensorData.HasData || got.SensorData.Temperature != 20 {
		t.Fatalf("unexpected state %+v", got)
	}
}

func TestShutdown_DeregistersEverythingAndIgnoresLaterMessages(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "temperature", sensorConfig("garage1", "temperature", "Temperature", "temperature", "°C"))
	f.discover("garage1", "binary_sensor", "door", doorConfig("garage1", "door", "Door"))

	f.registry.Shutdown()

	if len(f.container.deregistered) != 2 {
		t.Fatalf("expected both entities deregistered, got %v", f.container.deregistered)
	}

	f.state("garage1", "garage1/sensor/temperature/state", "20")
	f.discover("garage1", "binary_sensor", "door2", doorConfig("garage1", "door2", "Door 2"))
	f.registry.HandleConnected("garage1", "10.0.0.1")

	if len(f.container.registrations) != 2 || len(f.container.broadcasts) != 0 {
		t.Fatalf("activity after shutdown: %+v / %+v", f.container.registrations, f.container.broadcasts)
	}
}

func TestEntityName(t *testing.T) {
	r := newRegistry(&fakeContainer{}, "homeassistant", map[string]string{"garage1": "Left Garage"})

	for _, c := range []struct {
		device string
		name   string
		disc   string
		want   string
	}{
		{"garage1", "Temperature", "garage1", "Left Garage Temperature"},
		{"garage1", "", "garage1", "Left Garage"},
		{"garage1", "left garage", "garage1", "Left Garage"},
		{"garage1", "Left Garage Door", "garage1", "Left Garage Door"},
		{"fan", "Temperature", "Attic Fan", "Attic Fan Temperature"},
		{"fan", "Temperature", "", "fan Temperature"},
		// A Kauf plug's relay is named for the device, which has the end of its MAC address on the end.
		{"kauf-plug-7981c8", "Kauf Plug", "Kauf Plug 7981c8", "Kauf Plug 7981c8"},
		{"kauf-plug-7981c8", "kauf plug", "Kauf Plug 7981c8", "Kauf Plug 7981c8"},
		{"kauf-plug-7981c8", "Power", "Kauf Plug 7981c8", "Kauf Plug 7981c8 Power"},
		// Named for what the device calls itself, whatever it's been given to be called on the page.
		{"garage1", "Kauf Plug", "Kauf Plug 7981c8", "Left Garage"}, // given the display name Left Garage
		// Older Kauf firmware: no friendly name, and the plug's name on every entity.
		{"kauf-plug-7aad7c", "Kauf Plug", "kauf-plug-7aad7c", "kauf-plug-7aad7c"},
		{"kauf-plug-7aad7c", "Kauf Plug Power", "kauf-plug-7aad7c", "kauf-plug-7aad7c Power"},
		{"kauf-plug-7aad7c", "Kauf Plug Total Daily Energy", "kauf-plug-7aad7c", "kauf-plug-7aad7c Total Daily Energy"},
		{"garage1", "Kauf Plug Current", "kauf-plug-7aad7c", "Left Garage Current"},
		// But only at a word boundary: "Kauf" isn't the start of "Kaufman Plug".
		{"p", "Kauf", "Kaufman Plug", "Kaufman Plug Kauf"},
		{"p", "Kauf Plug 7981c8 Extra", "Kauf Plug 7981c8", "Kauf Plug 7981c8 Extra"},
	} {
		e := entityForName(c.name, c.disc)
		if got := r.entityName(c.device, e); got != c.want {
			t.Errorf("entityName(%q, name=%q, device=%q) = %q, want %q", c.device, c.name, c.disc, got, c.want)
		}
	}
}

func entityForName(name string, deviceName string) hadiscovery.Entity {
	return hadiscovery.Entity{Name: name, Device: hadiscovery.Device{Name: deviceName}}
}

// The binary sensor from a real device, whose YAML didn't set a device_class, and the status LED beside it.
// Neither is tracked, and the log says why and what to change.
func TestDiscovery_TheCapturedDoorSensorWithoutADeviceClassIsIgnoredWithAHint(t *testing.T) {
	f := newFixture(t)

	f.discover("garagedoor1", "binary_sensor", "garage_door_position",
		`{"name":"Garage Door Position","stat_t":"garagedoor1/binary_sensor/garage_door_position/state","avty_t":"garagedoor1/status","uniq_id":"ESPbinary_sensorgarage_door_position","dev":{"ids":"20500de464ec","name":"GarageDoor1","mdl":"esp32","mf":"Espressif"}}`)
	f.discover("garagedoor1", "light", "garage_esp_status_led",
		`{"schema":"json","supported_color_modes":["onoff"],"name":"Garage ESP Status LED","stat_t":"garagedoor1/light/garage_esp_status_led/state","cmd_t":"garagedoor1/light/garage_esp_status_led/command","uniq_id":"ESPlightgarage_esp_status_led","dev":{"ids":"20500de464ec","name":"GarageDoor1"}}`)

	if len(f.container.registrations) != 0 {
		t.Fatalf("unexpected registrations %+v", f.container.registrations)
	}

	if len(f.logs) != 2 {
		t.Fatalf("expected a log line for each ignored entity, got %v", f.logs)
	}

	if !strings.Contains(f.logs[0], "garage_door_position") || !strings.Contains(f.logs[0], "set device_class: garage_door") {
		t.Errorf("the binary sensor's log line doesn't say what to do: %q", f.logs[0])
	}

	if !strings.Contains(f.logs[1], "garage_esp_status_led") || strings.Contains(f.logs[1], "set device_class: garage_door") ||
		!strings.Contains(f.logs[1], "switches, sensors with device_class temperature, humidity, power, voltage, current or energy") {
		t.Errorf("the light's log line should list what is tracked, not suggest a door: %q", f.logs[1])
	}
}

func TestConnectAndDisconnectAreLogged(t *testing.T) {
	f := newFixture(t)

	f.registry.HandleConnected("garagedoor1", "10.47.7.17")
	f.registry.HandleDisconnected("garagedoor1")

	if len(f.logs) != 2 || f.logs[0] != "garagedoor1: connected from 10.47.7.17" || f.logs[1] != "garagedoor1: disconnected" {
		t.Fatalf("unexpected log lines %v", f.logs)
	}

	// After shutdown nothing is processed, and nothing is logged either.
	f.registry.Shutdown()
	f.registry.HandleConnected("garagedoor1", "10.47.7.17")

	if len(f.logs) != 2 {
		t.Fatalf("logged after shutdown: %v", f.logs)
	}
}

// recordingHandler keeps the message of each record, as a person reading the log would see it.
type recordingHandler struct{ messages *[]string }

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.messages = append(*h.messages, r.Message)

	return nil
}

func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// An MQTT device announces the temperature of its own chip (ESPHome's internal_temperature) as a temperature sensor with a category of
// diagnostic. It is on the device's card, as it is for a device on the native API, and it is not a thermometer.
func TestChipTemperature_AnMQTTDevicesIsOnItsCardAndNotAThermometer(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("relay1", "10.0.0.9")

	f.discover("relay1", "sensor", "internal_temperature",
		`{"name":"Internal Temperature","state_topic":"relay1/sensor/internal_temperature/state","device_class":"temperature","unit_of_measurement":"°C","ent_cat":"diagnostic","device":{"name":"relay1"}}`)
	f.discover("relay1", "sensor", "room", sensorConfig("relay1", "room", "Room", "temperature", "°C"))

	if len(f.container.registrations) != 1 || f.container.registrations[0].uniqueData != "relay1/room" {
		t.Fatalf("registrations: %+v", f.container.registrations)
	}

	if chip := f.registry.deviceView("relay1").ChipTemperature; chip == nil || chip.HasData {
		t.Fatalf("%+v", chip)
	}

	f.advance(time.Second)
	f.state("relay1", "relay1/sensor/internal_temperature/state", "nan")

	if chip := f.registry.deviceView("relay1").ChipTemperature; chip.HasData {
		t.Fatalf("not a number was a reading: %+v", chip)
	}

	f.state("relay1", "relay1/sensor/internal_temperature/state", "44.5")
	f.state("relay1", "relay1/sensor/room/state", "20")

	chip := f.registry.deviceView("relay1").ChipTemperature
	if chip == nil || !chip.HasData || chip.Celsius != 44.5 || !chip.LastUpdate.Equal(f.clock) {
		t.Fatalf("%+v", chip)
	}

	if got := lastThermometerState(t, f.container); got.SensorData.Temperature != 20 {
		t.Errorf("the thermometer: %+v", got)
	}

	// The device takes it back.
	f.discover("relay1", "sensor", "internal_temperature", "")

	if chip := f.registry.deviceView("relay1").ChipTemperature; chip != nil {
		t.Errorf("%+v", chip)
	}
}

func TestChipTemperature_ASensorThatBecomesADiagnosticStopsBeingAThermometer(t *testing.T) {
	f := newFixture(t)
	f.discover("garage1", "sensor", "cpu", sensorConfig("garage1", "cpu", "CPU", "temperature", "°C"))

	if len(f.container.registrations) != 1 {
		t.Fatalf("%+v", f.container.registrations)
	}

	f.discover("garage1", "sensor", "cpu",
		`{"name":"CPU","state_topic":"garage1/sensor/cpu/state","device_class":"temperature","unit_of_measurement":"°C","entity_category":"diagnostic","device":{"name":"garage1"}}`)

	if len(f.container.deregistered) != 1 || f.registry.deviceView("garage1").ChipTemperature == nil {
		t.Errorf("deregistered %v, card %+v", f.container.deregistered, f.registry.deviceView("garage1").ChipTemperature)
	}
}

// The device's settings and what is off unless asked for are not entities, as for a device on the native API.
func TestHidden_AnMQTTDevicesSettingsAndDisabledEntitiesAreNotEntities(t *testing.T) {
	f := newFixture(t)

	f.discover("plug1", "switch", "relay", `{"name":"Relay","state_topic":"plug1/switch/relay/state","command_topic":"plug1/switch/relay/command"}`)
	f.discover("plug1", "switch", "blue_led",
		`{"name":"Blue LED","state_topic":"plug1/switch/blue_led/state","command_topic":"plug1/switch/blue_led/command","ent_cat":"config"}`)
	f.discover("plug1", "switch", "quiet",
		`{"name":"Quiet","state_topic":"plug1/switch/quiet/state","command_topic":"plug1/switch/quiet/command","en":false}`)
	f.discover("plug1", "sensor", "wifi",
		`{"name":"WiFi","state_topic":"plug1/sensor/wifi/state","device_class":"voltage","unit_of_measurement":"V","ent_cat":"diagnostic"}`)

	if len(f.container.registrations) != 1 || f.container.registrations[0].uniqueData != "plug1/relay" {
		t.Fatalf("registrations: %+v", f.container.registrations)
	}

	if chip := f.registry.deviceView("plug1").ChipTemperature; chip != nil {
		t.Errorf("a chip's temperature that is not one: %+v", chip)
	}
}
