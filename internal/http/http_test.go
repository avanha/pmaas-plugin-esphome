package http

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/data"
	"github.com/avanha/pmaas-plugin-esphome/internal/common"
	spi "github.com/avanha/pmaas-spi"
)

// render renders a template the way PMAAS does. Its template engine plugin uses text/template, which doesn't
// escape anything, so a template has to do that itself: html/template here would escape automatically and hide
// one that doesn't.
func render(t *testing.T, info spi.TemplateInfo, value any) string {
	t.Helper()

	name := path.Base(info.Paths[0])
	tmpl, err := template.New(name).Funcs(info.FuncMap).ParseFS(contentFS, "content/"+info.Paths[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var out bytes.Buffer
	if err := tmpl.ExecuteTemplate(&out, name, value); err != nil {
		t.Fatalf("execute: %v", err)
	}

	return out.String()
}

func checkContains(t *testing.T, out string, wants []string, unwanteds []string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	for _, unwanted := range unwanteds {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpectedly contains %q in:\n%s", unwanted, out)
		}
	}
}

func TestStatusTemplate_ARunningBroker(t *testing.T) {
	now := time.Now()

	out := render(t, statusTemplate, &data.PluginStatus{
		BrokerRunning: true, ListenAddress: "[::]:1883", DiscoveryPrefix: "homeassistant",
		LastMessageTime: now.Add(-2 * time.Minute), DeviceCount: 3, ConnectedDevices: 1, ThermometerCount: 1, DoorSensorCount: 2,
	})

	checkContains(t, out, []string{
		"Listening on", "[::]:1883", "homeassistant", "1 of 3 connected", "1 thermometer,", "2 door sensors",
	}, []string{"Not running", "Capturing messages", "No devices are configured", "device-header"})
}

func TestStatusTemplate_NoLongerListsTheDevicesThemselves(t *testing.T) {
	out := render(t, statusTemplate, &data.PluginStatus{BrokerRunning: true, DeviceCount: 1, ConnectedDevices: 1})

	// Each device has a card of its own, with its status in it.
	checkContains(t, out, []string{"1 of 1 connected"}, []string{"Connected 3h", "device-details", "Firmware"})
}

func TestStatusTemplate_ABrokerThatIsNotRunningSaysWhy(t *testing.T) {
	out := render(t, statusTemplate, &data.PluginStatus{
		BrokerProblem: "unable to listen on :1883: address already in use", DiscoveryPrefix: "homeassistant",
	})

	checkContains(t, out,
		[]string{"Not running", "unable to listen on :1883: address already in use", "0 of 0 connected"},
		[]string{"Listening on"})
}

func TestStatusTemplate_WarnsWhenCapturingMessages(t *testing.T) {
	out := render(t, statusTemplate, &data.PluginStatus{BrokerRunning: true, CaptureMessages: true})

	checkContains(t, out, []string{"Capturing messages"}, nil)

	out = render(t, statusTemplate, &data.PluginStatus{BrokerRunning: true})

	checkContains(t, out, nil, []string{"Capturing messages"})
}

func TestStatusTemplate_Pluralization(t *testing.T) {
	for _, c := range []struct {
		thermometers, doors int
		want                string
	}{
		{1, 1, "1 thermometer, 1 door sensor"},
		{0, 0, "0 thermometers, 0 door sensors"},
		{2, 1, "2 thermometers, 1 door sensor"},
	} {
		out := strings.Join(strings.Fields(render(t, statusTemplate,
			&data.PluginStatus{ThermometerCount: c.thermometers, DoorSensorCount: c.doors})), " ")

		if !strings.Contains(out, c.want) {
			t.Errorf("%+v: missing %q in %q", c, c.want, out)
		}
	}
}

func TestThermometerTemplate(t *testing.T) {
	out := render(t, thermometerTemplate, &data.ThermometerView{
		Name: "Garage Temperature", DeviceName: "garagedoor1", DeviceConnected: true, HasData: true,
		Temperature: 20, HasHumidity: true, Humidity: 39.2, LastUpdateTime: time.Now().Add(-5 * time.Minute),
	})

	checkContains(t, out,
		[]string{"Garage Temperature", "20.0 C", "68.0 F", "39.2%", "5m ago"},
		// The device's name is on its card, and isn't repeated under each thing it has.
		[]string{"offline", "Waiting for data", "garagedoor1"})

	out = render(t, thermometerTemplate, &data.ThermometerView{Name: "T", DeviceConnected: true, HasData: true, Temperature: 21.5})
	checkContains(t, out, []string{"21.5 C", "70.7 F"}, []string{"droplet"})
}

func TestThermometerTemplate_WaitingAndOffline(t *testing.T) {
	out := render(t, thermometerTemplate, &data.ThermometerView{Name: "T", DeviceConnected: true})
	checkContains(t, out, []string{"Waiting for data"}, []string{" C<", "offline"})

	out = render(t, thermometerTemplate, &data.ThermometerView{
		Name: "T", HasData: true, Temperature: 20, LastUpdateTime: time.Now().Add(-time.Hour),
	})
	checkContains(t, out, []string{"entity-esphome-thermometer offline", "bi-wifi-off", "Device offline: last known reading"}, nil)
}

func TestDoorSensorTemplate_EachPosition(t *testing.T) {
	changed := time.Now().Add(-26 * time.Hour)

	for _, c := range []struct {
		position, class, icon string
	}{
		{"Open", "open", "bi-door-open-fill"},
		{"Closed", "closed", "bi-door-closed-fill"},
		{"Unknown", "unknown", "bi-question-circle-fill"},
	} {
		out := render(t, doorSensorTemplate, &data.DoorSensorView{
			Name: "Door", DeviceName: "garagedoor1", DeviceConnected: true, HasData: true,
			Position: c.position, PositionClass: c.class, LastChangeTime: changed, LastUpdateTime: time.Now(),
		})

		checkContains(t, out, []string{
			`class="entity-esphome-door-sensor ` + c.class + `"`,
			c.icon, `class="position ` + c.class + `">` + c.position,
			c.position + " since 1d 2h ago",
		}, []string{"offline", "Waiting for data"})
	}
}

func TestDoorSensorTemplate_BeforeAnyChangeHasBeenSeen(t *testing.T) {
	out := render(t, doorSensorTemplate, &data.DoorSensorView{
		Name: "Door", DeviceConnected: true, HasData: true, Position: "Closed", PositionClass: "closed",
		LastUpdateTime: time.Now().Add(-10 * time.Minute),
	})

	// It can't say how long it has been closed, because it doesn't know.
	checkContains(t, out, []string{"Reported 10m ago"}, []string{"since"})
}

func TestDoorSensorTemplate_WaitingAndOffline(t *testing.T) {
	out := render(t, doorSensorTemplate, &data.DoorSensorView{
		Name: "Door", DeviceConnected: true, Position: "Unknown", PositionClass: "unknown",
	})
	checkContains(t, out, []string{"Waiting for data"}, []string{"position-row", "offline"})

	out = render(t, doorSensorTemplate, &data.DoorSensorView{
		Name: "Door", HasData: true, Position: "Open", PositionClass: "open",
		LastChangeTime: time.Now().Add(-time.Hour), OfflineSince: time.Now().Add(-5 * time.Minute),
	})
	checkContains(t, out, []string{
		"entity-esphome-door-sensor open offline", "bi-wifi-off", "Device offline since 5m ago: last known position",
	}, nil)
}

// What a device calls itself and its entities is whatever it was configured with, and an address or a problem
// is text from the network. None of it may reach the page as markup.
func TestTemplates_EscapeEverythingThatComesFromOutside(t *testing.T) {
	hostile := `<script>alert("x")</script>`
	attack := `"><img src=x onerror=alert(1)>`

	outputs := map[string]string{
		"status": render(t, statusTemplate, &data.PluginStatus{
			BrokerProblem: hostile, ListenAddress: attack, DiscoveryPrefix: hostile,
		}),
		"device": render(t, deviceTemplate, &deviceWithChildren{
			DeviceStatus: data.DeviceStatus{
				Name: attack, Label: hostile, Transport: hostile, Problem: hostile, Address: attack, Firmware: hostile,
				Model: hostile, Manufacturer: attack, MAC: hostile,
			},
		}),
		"status running": render(t, statusTemplate, &data.PluginStatus{
			BrokerRunning: true, ListenAddress: attack, DiscoveryPrefix: hostile,
		}),
		"thermometer": render(t, thermometerTemplate, &data.ThermometerView{
			Name: hostile, DeviceName: attack, HasData: true, DeviceConnected: true,
		}),
		"door": render(t, doorSensorTemplate, &data.DoorSensorView{
			Name: hostile, DeviceName: attack, HasData: true, DeviceConnected: true,
			Position: hostile, PositionClass: attack,
		}),
		"switch": render(t, switchTemplate, &data.SwitchView{
			Name: hostile, DeviceName: attack, ObjectID: attack, HasData: true, DeviceConnected: true, CanControl: true,
			State: hostile, StateClass: attack,
		}),
		"power": render(t, powerTemplate, &data.PowerView{Name: hostile, DeviceName: attack, HasData: true}),
	}

	for name, out := range outputs {
		for _, dangerous := range []string{"<script>", "<img src=x", `onerror=alert(1)>`} {
			if strings.Contains(out, dangerous) {
				t.Errorf("%s: unescaped %q in:\n%s", name, dangerous, out)
			}
		}
	}
}

type fakeStore struct {
	result common.StatusAndEntities
	err    error

	switchCalls []switchCall
	switchErr   error
}

func (s *fakeStore) GetStatusAndEntities() (common.StatusAndEntities, error) { return s.result, s.err }

type switchCall struct {
	device       string
	object       string
	on           bool
	confirmation common.Confirmation
}

func (s *fakeStore) SetSwitch(deviceName string, object string, on bool, confirmation common.Confirmation) error {
	s.switchCalls = append(s.switchCalls, switchCall{deviceName, object, on, confirmation})

	return s.switchErr
}

type renderCall struct {
	options  spi.RenderListOptions
	items    []interface{}
	rendered bool
}

// listContainer records what the page asks the container to render.
type listContainer struct {
	spi.IPMAASContainer
	call renderCall
}

// GetTemplate loads the real templates, as the template engine would, with text/template.
func (c *listContainer) GetTemplate(info *spi.TemplateInfo) (spi.CompiledTemplate, error) {
	name := path.Base(info.Paths[0])

	tmpl, err := template.New(name).Funcs(info.FuncMap).ParseFS(contentFS, "content/"+info.Paths[0])
	if err != nil {
		return spi.CompiledTemplate{}, err
	}

	return spi.CompiledTemplate{Instance: namedTemplate{tmpl, name}, Styles: info.Styles, Scripts: info.Scripts}, nil
}

type namedTemplate struct {
	t    *template.Template
	name string
}

func (n namedTemplate) Execute(w io.Writer, data any) error {
	return n.t.ExecuteTemplate(w, n.name, data)
}

func (c *listContainer) RenderList(_ http.ResponseWriter, _ *http.Request, options spi.RenderListOptions, items []interface{}) {
	c.call = renderCall{options: options, items: items, rendered: true}
}

func TestHandleHttpListRequest_RendersTheStatusAboveTheDeviceCards(t *testing.T) {
	status := data.PluginStatus{BrokerRunning: true}
	entities := []any{
		&data.DeviceView{DeviceStatus: data.DeviceStatus{Name: "plug1"}, Children: []data.Child{data.SwitchView{Name: "S"}, data.PowerView{Name: "P"}}},
		&data.DeviceView{DeviceStatus: data.DeviceStatus{Name: "plug2"}},
	}

	container := &listContainer{}
	h := &Handler{log: slog.New(slog.DiscardHandler), container: container, entityStore: &fakeStore{result: common.StatusAndEntities{Status: status, Entities: entities}}}

	h.handleHttpListRequest(httptest.NewRecorder(), httptest.NewRequest("GET", "/plugins/esphome/", nil))

	call := container.call
	if !call.rendered || call.options.Title != "ESPHome Devices" || len(call.items) != 2 {
		t.Fatalf("unexpected call %+v", call)
	}

	header, ok := call.options.Header.(*data.PluginStatus)
	if !ok || !header.BrokerRunning {
		t.Fatalf("the status should be the header, as a pointer, got %#v", call.options.Header)
	}

	first, ok := call.items[0].(*deviceWithChildren)
	if !ok || first.Name != "plug1" || len(first.Children) != 2 {
		t.Fatalf("unexpected first item %#v", call.items[0])
	}

	// The children stay in the order the device's list had them: the ordering is done before this.
	if first.Children[0].Child.Kind() != data.KindSwitch || first.Children[1].Child.Kind() != data.KindPowerMonitor {
		t.Errorf("unexpected children %+v", first.Children)
	}

	if second, ok := call.items[1].(*deviceWithChildren); !ok || second.Name != "plug2" || len(second.Children) != 0 {
		t.Errorf("unexpected second item %#v", call.items[1])
	}
}

// Something that isn't a device in the list is reported and left out, not rendered as something it isn't.
func TestHandleHttpListRequest_LeavesOutWhatIsNotADevice(t *testing.T) {
	container := &listContainer{}
	h := &Handler{log: slog.New(slog.DiscardHandler), container: container, entityStore: &fakeStore{result: common.StatusAndEntities{
		Entities: []any{"not a device", &data.DeviceView{DeviceStatus: data.DeviceStatus{Name: "plug1"}}},
	}}}

	h.handleHttpListRequest(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if len(container.call.items) != 1 {
		t.Fatalf("unexpected items %#v", container.call.items)
	}
}

// What there is a renderer for, for every kind of child there is.
func TestChildKinds_EveryKindHasARendererThatTakesItsOwnViews(t *testing.T) {
	views := map[string]data.Child{
		data.KindSwitch:       data.SwitchView{},
		data.KindPowerMonitor: data.PowerView{},
		data.KindDoorSensor:   data.DoorSensorView{},
		data.KindThermometer:  data.ThermometerView{},
	}

	for kind, view := range views {
		if view.Kind() != kind {
			t.Errorf("%T says it's a %q, not a %q", view, view.Kind(), kind)
		}

		entry, ok := childKinds[kind]
		if !ok {
			t.Errorf("no renderer for %q", kind)
			continue
		}

		if _, ok := entry.cast(view); !ok {
			t.Errorf("the %q renderer doesn't take a %T", kind, view)
		}

		// Nor does it take another kind's.
		for otherKind, other := range views {
			if otherKind != kind {
				if _, ok := entry.cast(other); ok {
					t.Errorf("the %q renderer takes a %T", kind, other)
				}
			}
		}
	}

	if len(childKinds) != len(views) {
		t.Errorf("there are %d renderers for %d kinds", len(childKinds), len(views))
	}
}

// The device card with its children, rendered by the real templates the way the page does it.
func TestDeviceCard_RendersItsChildrenInsideItself(t *testing.T) {
	container := &listContainer{}
	h := &Handler{log: slog.New(slog.DiscardHandler), container: container}

	device := h.newDeviceWithChildren(&data.DeviceView{
		DeviceStatus: data.DeviceStatus{
			Name: "kauf-plug-1", Label: "Space Heater", Transport: "Native API", Connected: true,
			ConnectedSince: time.Now().Add(-3 * time.Hour), Address: "10.0.0.7:6053", Firmware: "2026.9.1",
			Model: "PLF10", Manufacturer: "Kauf", MAC: "aa:bb:cc:dd:ee:ff", LastMessageTime: time.Now(),
		},
		Children: []data.Child{
			data.SwitchView{Name: "Space Heater Relay", DeviceName: "kauf-plug-1", ObjectID: "relay", DeviceConnected: true,
				CanControl: true, HasData: true, State: "On", StateClass: "on", LastUpdateTime: time.Now()},
			data.PowerView{Name: "Space Heater Power", DeviceName: "kauf-plug-1", DeviceConnected: true, HasData: true, HasPower: true, Power: 27.5},
		},
	})

	out := render(t, deviceTemplate, device)

	checkContains(t, out, []string{
		`entity-esphome-device connected`, "Space Heater", "kauf-plug-1", "Native API", "Connected 3h 0m ago",
		"10.0.0.7:6053", "2026.9.1", "PLF10 (Kauf)", "aa:bb:cc:dd:ee:ff",
		`entity-esphome-switch on`, "Space Heater Relay", `class="switch-toggle on"`, "27.5 W",
	}, []string{"Nothing it has is tracked", "disconnected"})

	// Inside the card, in order, each in a block of its own.
	if strings.Count(out, `class="child"`) != 2 || strings.Index(out, "entity-esphome-switch") > strings.Index(out, "entity-esphome-power") {
		t.Errorf("unexpected children:\n%s", out)
	}

	// The device's name is in its header and not repeated under each child.
	if strings.Count(out, "kauf-plug-1</div>") != 0 && strings.Contains(out, "device-note") {
		t.Errorf("the children still repeat the device's name:\n%s", out)
	}
}

func TestDeviceCard_ADeviceWithNothingTrackedStillHasACard(t *testing.T) {
	out := render(t, deviceTemplate, &deviceWithChildren{
		DeviceStatus: data.DeviceStatus{Name: "plug2", Label: "Lamp", Transport: "Native API", Problem: "dial tcp 10.0.0.9:6053: i/o timeout"},
	})

	checkContains(t, out, []string{
		`entity-esphome-device disconnected`, "Lamp", "Never connected", "dial tcp 10.0.0.9:6053: i/o timeout",
		"Nothing it has is tracked yet",
	}, []string{`class="child"`, "Address", "Firmware"})

	out = render(t, deviceTemplate, &deviceWithChildren{DeviceStatus: data.DeviceStatus{Name: "p", Label: "P", Connected: true}})
	checkContains(t, out, []string{"Nothing it has is tracked."}, []string{"tracked yet", "problem"})
}

func TestDeviceCard_OnlyShowsTheDetailsTheDeviceHas(t *testing.T) {
	out := render(t, deviceTemplate, &deviceWithChildren{
		DeviceStatus: data.DeviceStatus{Name: "d", Label: "d", Connected: true, ConnectedSince: time.Now()},
	})

	checkContains(t, out, nil, []string{"Address", "Firmware", "Model", "MAC", "Last message"})
}

func TestDeviceCard_ADisconnectedDeviceSaysSince(t *testing.T) {
	out := render(t, deviceTemplate, &deviceWithChildren{
		DeviceStatus: data.DeviceStatus{Name: "d", Label: "d", DisconnectedSince: time.Now().Add(-90 * time.Minute)},
	})

	checkContains(t, out, []string{"Disconnected 1h 30m ago", "bi-wifi-off"}, []string{"Never connected"})
}

func TestHandleHttpListRequest_StillRendersAPageWhenTheStatusCannotBeRetrieved(t *testing.T) {
	container := &listContainer{}
	h := &Handler{log: slog.New(slog.DiscardHandler), container: container, entityStore: &fakeStore{err: errors.New("the plugin is stopping")}}

	h.handleHttpListRequest(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if !container.call.rendered || len(container.call.items) != 0 || container.call.options.Header == nil {
		t.Fatalf("expected an empty page, got %+v", container.call)
	}
}

func TestRelativeTime(t *testing.T) {
	for _, c := range []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, "never"},
		{"now", time.Now(), "just now"},
		{"slightly in the future", time.Now().Add(30 * time.Second), "just now"},
		{"minutes", time.Now().Add(-5*time.Minute - 10*time.Second), "5m ago"},
		{"hours", time.Now().Add(-3*time.Hour - 20*time.Minute - 10*time.Second), "3h 20m ago"},
		{"days", time.Now().Add(-(2*24+5)*time.Hour - time.Minute), "2d 5h ago"},
	} {
		if got := relativeTime(c.t); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCelsiusToFahrenheit(t *testing.T) {
	for c, want := range map[float32]float32{0: 32, 100: 212, -40: -40, 20: 68, 37: 98.6} {
		if got := celsiusToFahrenheit(c); got < want-0.01 || got > want+0.01 {
			t.Errorf("celsiusToFahrenheit(%v) = %v, want %v", c, got, want)
		}
	}
}

func TestSwitchTemplate_EachState(t *testing.T) {
	changed := time.Now().Add(-26 * time.Hour)

	for _, c := range []struct {
		state, class, icon, nextOn, title string
	}{
		{"On", "on", "bi-toggle-on", "false", "Turn off"},
		{"Off", "off", "bi-toggle-off", "true", "Turn on"},
		{"Unknown", "unknown", "bi-question-circle-fill", "true", "Turn on"},
	} {
		out := render(t, switchTemplate, &data.SwitchView{
			Name: "Heater", DeviceName: "plug1", ObjectID: "relay", DeviceConnected: true, CanControl: true, HasData: true,
			State: c.state, StateClass: c.class, LastChangeTime: changed, LastUpdateTime: time.Now(),
		})

		checkContains(t, out, []string{
			`class="entity-esphome-switch ` + c.class + `"`, `data-device="plug1"`, `data-object="relay"`,
			c.icon, `class="state ` + c.class + `">` + c.state, c.state + " since 1d 2h ago",
			// The icon is the control, and asks for the other state.
			`<button type="button" class="switch-toggle ` + c.class + `"`, `data-on="` + c.nextOn + `"`,
			`title="` + c.title + `"`, `aria-label="` + c.title + ` Heater"`,
		}, []string{"offline", "Waiting for data", "Turn on</button>", "Turn off</button>", "disabled"})

		// The icon is inside the button, not beside it.
		if strings.Index(out, "<button") > strings.Index(out, c.icon) || strings.Index(out, c.icon) > strings.Index(out, "</button>") {
			t.Errorf("%s: the icon isn't inside the button:\n%s", c.state, out)
		}
	}
}

func TestSwitchTemplate_TheIconIsOnlyAPictureWhenItCannotBeControlled(t *testing.T) {
	out := render(t, switchTemplate, &data.SwitchView{Name: "S", DeviceConnected: true, CanControl: true, HasData: true, StateClass: "on", State: "On"})
	checkContains(t, out, []string{"switch-toggle"}, []string{"disabled"})

	out = render(t, switchTemplate, &data.SwitchView{Name: "S", DeviceConnected: true, HasData: true, StateClass: "on", State: "On"})
	checkContains(t, out, []string{"switch-toggle", " disabled>"}, nil)
}

func TestSwitchTemplate_WaitingAndOffline(t *testing.T) {
	out := render(t, switchTemplate, &data.SwitchView{Name: "S", DeviceConnected: true, StateClass: "unknown", State: "Unknown"})
	checkContains(t, out, []string{"Waiting for data"}, []string{"state-row", "offline"})

	out = render(t, switchTemplate, &data.SwitchView{
		Name: "S", HasData: true, State: "On", StateClass: "on", OfflineSince: time.Now().Add(-5 * time.Minute),
	})
	// Still shown, as a picture: nothing can be asked of a device that's offline.
	checkContains(t, out, []string{
		"entity-esphome-switch on offline", "bi-wifi-off", "Device offline since 5m ago: last known state", "switch-toggle", " disabled>",
	}, nil)
}

func TestPowerTemplate_ShowsOnlyTheReadingsTheDeviceHas(t *testing.T) {
	out := render(t, powerTemplate, &data.PowerView{
		Name: "Heater Power", DeviceName: "plug1", DeviceConnected: true, HasData: true,
		HasPower: true, Power: 1500.25, HasEnergy: true, Energy: 12.3456, LastUpdateTime: time.Now().Add(-2 * time.Minute),
	})

	checkContains(t, out,
		[]string{"Heater Power", "1500.2 W", "12.346 kWh", "2m ago"},
		[]string{" V<", " A<", "offline", "Waiting for data", "plug1"})

	out = render(t, powerTemplate, &data.PowerView{
		Name: "P", DeviceConnected: true, HasData: true, HasVoltage: true, Voltage: 121.34, HasCurrent: true, Current: 0.5,
	})
	checkContains(t, out, []string{"121.3 V", "0.500 A"}, []string{" W<", "kWh"})
}

func TestPowerTemplate_ZeroIsAReadingAndAbsenceIsNot(t *testing.T) {
	out := render(t, powerTemplate, &data.PowerView{Name: "P", DeviceConnected: true, HasData: true, HasPower: true})
	checkContains(t, out, []string{"0.0 W"}, nil)

	out = render(t, powerTemplate, &data.PowerView{Name: "P", DeviceConnected: true, HasData: true, HasVoltage: true, Voltage: 120})
	checkContains(t, out, nil, []string{"0.0 W"})
}

func TestPowerTemplate_WaitingAndOffline(t *testing.T) {
	out := render(t, powerTemplate, &data.PowerView{Name: "P", DeviceConnected: true})
	checkContains(t, out, []string{"Waiting for data"}, []string{"readings", "offline"})

	out = render(t, powerTemplate, &data.PowerView{Name: "P", HasData: true, HasPower: true, Power: 5})
	checkContains(t, out, []string{"entity-esphome-power offline", "bi-wifi-off", "Device offline: last known readings"}, nil)
}

func TestStatusTemplate_CountsSwitchesAndPowerMonitors(t *testing.T) {
	out := strings.Join(strings.Fields(render(t, statusTemplate, &data.PluginStatus{SwitchCount: 1, PowerMonitorCount: 2})), " ")
	checkContains(t, out, []string{"1 switch,", "2 power monitors"}, []string{"1 switches"})

	out = strings.Join(strings.Fields(render(t, statusTemplate, &data.PluginStatus{SwitchCount: 3, PowerMonitorCount: 1})), " ")
	checkContains(t, out, []string{"3 switches,", "1 power monitor"}, []string{"1 power monitors"})
}

func switchRequestFor(method string) *http.Request {
	return httptest.NewRequest(method, "/plugins/esphome/switch", nil)
}

func TestHandleHttpSwitchRequest_PassesTheRequestOn(t *testing.T) {
	store := &fakeStore{}
	h := &Handler{log: slog.New(slog.DiscardHandler), entityStore: store}

	for _, on := range []bool{true, false} {
		response, err := h.handleHttpSwitchRequest(nil, switchRequestFor("POST"), &switchRequest{Device: "plug1", Object: "relay", On: on})
		if err != nil {
			t.Fatal(err)
		}

		if got, ok := response.(switchResponse); !ok || got.Requested != on {
			t.Fatalf("unexpected response %#v", response)
		}
	}

	want := []switchCall{{"plug1", "relay", true, common.Confirmation{}}, {"plug1", "relay", false, common.Confirmation{}}}
	if !reflect.DeepEqual(store.switchCalls, want) {
		t.Fatalf("calls %+v, want %+v", store.switchCalls, want)
	}
}

func TestHandleHttpSwitchRequest_RefusesWhatItShouldNot(t *testing.T) {
	for _, c := range []struct {
		name    string
		method  string
		request any
	}{
		{"a GET", "GET", &switchRequest{Device: "plug1", Object: "relay"}},
		{"no device", "POST", &switchRequest{Object: "relay"}},
		{"no switch", "POST", &switchRequest{Device: "plug1"}},
		{"not the request type", "POST", "something else"},
		{"nothing", "POST", nil},
	} {
		store := &fakeStore{}
		h := &Handler{log: slog.New(slog.DiscardHandler), entityStore: store}

		if _, err := h.handleHttpSwitchRequest(nil, switchRequestFor(c.method), c.request); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}

		if len(store.switchCalls) != 0 {
			t.Errorf("%s: the switch was commanded: %+v", c.name, store.switchCalls)
		}
	}
}

func TestHandleHttpSwitchRequest_ReportsWhyItCouldNotBeSent(t *testing.T) {
	store := &fakeStore{switchErr: errors.New("the device is offline")}
	h := &Handler{log: slog.New(slog.DiscardHandler), entityStore: store}

	_, err := h.handleHttpSwitchRequest(nil, switchRequestFor("POST"), &switchRequest{Device: "plug1", Object: "relay", On: true})
	if err == nil || !strings.Contains(err.Error(), "the device is offline") {
		t.Fatalf("got %v", err)
	}
}

// Inside a device's card the device is already on show, so each child is headed by its own short name, and the full
// one is there for whoever hovers over it.
func TestChildTemplates_AreHeadedByTheShortNameAndTheFullNameIsOnlyATooltip(t *testing.T) {
	for name, out := range map[string]string{
		"thermometer": render(t, thermometerTemplate, &data.ThermometerView{Name: "Garage 1 Temperature", ShortName: "Temperature", HasData: true, DeviceConnected: true}),
		"door":        render(t, doorSensorTemplate, &data.DoorSensorView{Name: "Garage 1 Door", ShortName: "Door", HasData: true, DeviceConnected: true, PositionClass: "open", Position: "Open"}),
		"switch":      render(t, switchTemplate, &data.SwitchView{Name: "Garage 1 Plug", ShortName: "Switch", HasData: true, DeviceConnected: true, StateClass: "on", State: "On"}),
		"power":       render(t, powerTemplate, &data.PowerView{Name: "Garage 1 Power", ShortName: "Power", HasData: true, DeviceConnected: true}),
	} {
		short := map[string]string{"thermometer": "Temperature", "door": "Door", "switch": "Switch", "power": "Power"}[name]
		full := map[string]string{"thermometer": "Garage 1 Temperature", "door": "Garage 1 Door", "switch": "Garage 1 Plug", "power": "Garage 1 Power"}[name]

		checkContains(t, out, []string{`<div class="name" title="` + full + `">` + short + `</div>`}, nil)

		// The full name is in the tooltip, and where an assistive reader or the confirmation dialog needs it: not
		// anywhere that's on show.
		if strings.Count(out, full) > 3 {
			t.Errorf("%s: the full name is repeated:\n%s", name, out)
		}
	}
}

func TestHandleHttpSwitchRequest_PassesOnWhatThePersonDidWhenAsked(t *testing.T) {
	store := &fakeStore{}
	h := &Handler{log: slog.New(slog.DiscardHandler), entityStore: store}

	_, err := h.handleHttpSwitchRequest(nil, switchRequestFor("POST"),
		&switchRequest{Device: "rack", Object: "relay", On: false, Confirmed: true, Password: "typed"})
	if err != nil {
		t.Fatal(err)
	}

	want := switchCall{"rack", "relay", false, common.Confirmation{Confirmed: true, Password: "typed"}}
	if len(store.switchCalls) != 1 || store.switchCalls[0] != want {
		t.Fatalf("calls %+v, want %+v", store.switchCalls, want)
	}
}

// Why a switch wouldn't change reaches the person, so that "wrong password" can be told from "offline".
func TestHandleHttpSwitchRequest_TheReasonIsInTheError(t *testing.T) {
	store := &fakeStore{switchErr: errors.New(`unable to change "Server Rack": wrong password`)}
	h := &Handler{log: slog.New(slog.DiscardHandler), entityStore: store}

	_, err := h.handleHttpSwitchRequest(nil, switchRequestFor("POST"), &switchRequest{Device: "rack", Object: "relay"})
	if err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("got %v", err)
	}
}

func TestSwitchTemplate_SaysHowTheSwitchIsGuarded(t *testing.T) {
	for _, c := range []struct {
		control, icon string
		disabled      bool
	}{
		{"open", "", false},
		{"confirm", "bi-shield-check", false},
		{"password", "bi-key-fill", false},
		{"readonly", "bi-lock-fill", true},
	} {
		out := render(t, switchTemplate, &data.SwitchView{
			Name: "Server Rack", DeviceName: "rack", ObjectID: "relay", DeviceConnected: true, HasData: true,
			StateClass: "on", State: "On", Control: c.control, CanControl: !c.disabled,
		})

		checkContains(t, out, []string{`data-control="` + c.control + `"`, `data-name="Server Rack"`}, nil)

		if c.icon == "" {
			checkContains(t, out, nil, []string{"guard"})
		} else {
			checkContains(t, out, []string{c.icon + " guard"}, nil)
		}

		if strings.Contains(out, " disabled>") != c.disabled {
			t.Errorf("%s: disabled=%v, want %v:\n%s", c.control, strings.Contains(out, " disabled>"), c.disabled, out)
		}
	}
}

// What the page puts in the confirmation is a name that came from a device.
func TestSwitchTemplate_TheNameInTheConfirmationIsEscaped(t *testing.T) {
	out := render(t, switchTemplate, &data.SwitchView{
		Name: `"><script>alert(1)</script>`, DeviceName: "d", ObjectID: "o", DeviceConnected: true, HasData: true,
		StateClass: "on", State: "On", Control: "password", CanControl: true,
	})

	checkContains(t, out, nil, []string{"<script>"})
}
