package http

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
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

func TestStatusTemplate_ARunningBrokerWithDevices(t *testing.T) {
	now := time.Now()

	out := render(t, statusTemplate, &data.PluginStatus{
		BrokerRunning: true, ListenAddress: "[::]:1883", DiscoveryPrefix: "homeassistant",
		LastMessageTime: now.Add(-2 * time.Minute), ConnectedDevices: 1, ThermometerCount: 1, DoorSensorCount: 2,
		Devices: []data.DeviceStatus{
			{
				Name: "garagedoor1", Label: "Double Garage Door", Connected: true, ConnectedSince: now.Add(-3 * time.Hour),
				Address: "10.47.7.17", Firmware: "2026.8.0", Model: "esp32", Manufacturer: "Espressif",
				MAC: "20:50:0d:e4:64:ec", EntityCount: 3, LastMessageTime: now.Add(-30 * time.Second),
			},
			{Name: "garagedoor2", Label: "garagedoor2", DisconnectedSince: now.Add(-90 * time.Minute), EntityCount: 1},
			{Name: "never", Label: "never"},
		},
	})

	checkContains(t, out, []string{
		"Listening on", "[::]:1883", "homeassistant",
		"(1 of 3 connected)",
		"1 thermometer,", "2 door sensors",
		"Double Garage Door", "garagedoor1", "Connected 3h 0m ago",
		"10.47.7.17", "2026.8.0", "esp32 (Espressif)", "20:50:0d:e4:64:ec",
		"Disconnected 1h 30m ago", "Never connected",
		`device connected`, `device disconnected`,
	}, []string{"Not running", "Capturing messages", "No devices are configured"})
}

func TestStatusTemplate_ABrokerThatIsNotRunningSaysWhy(t *testing.T) {
	out := render(t, statusTemplate, &data.PluginStatus{
		BrokerProblem: "unable to listen on :1883: address already in use", DiscoveryPrefix: "homeassistant",
	})

	checkContains(t, out,
		[]string{"Not running", "unable to listen on :1883: address already in use", "No devices are configured", "(0 of 0 connected)"},
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

func TestStatusTemplate_OnlyShowsTheDetailsAdeviceHas(t *testing.T) {
	out := render(t, statusTemplate, &data.PluginStatus{
		Devices: []data.DeviceStatus{{Name: "d", Label: "d", Connected: true, ConnectedSince: time.Now()}},
	})

	checkContains(t, out, []string{"Entities"}, []string{"Address", "Firmware", "Model", "MAC", "Last message</span> just"})
}

func TestThermometerTemplate(t *testing.T) {
	out := render(t, thermometerTemplate, &data.ThermometerView{
		Name: "Garage Temperature", DeviceName: "garagedoor1", DeviceConnected: true, HasData: true,
		Temperature: 20, HasHumidity: true, Humidity: 39.2, LastUpdateTime: time.Now().Add(-5 * time.Minute),
	})

	checkContains(t, out,
		[]string{"Garage Temperature", "20.0 C", "68.0 F", "39.2%", "5m ago", "garagedoor1"},
		[]string{"offline", "Waiting for data"})

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
			Devices: []data.DeviceStatus{{
				Name: attack, Label: hostile, Address: attack, Firmware: hostile, Model: hostile,
				Manufacturer: attack, MAC: hostile,
			}},
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
}

func (s *fakeStore) GetStatusAndEntities() (common.StatusAndEntities, error) { return s.result, s.err }

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

func (c *listContainer) RenderList(_ http.ResponseWriter, _ *http.Request, options spi.RenderListOptions, items []interface{}) {
	c.call = renderCall{options: options, items: items, rendered: true}
}

func TestHandleHttpListRequest_RendersTheStatusAboveTheEntities(t *testing.T) {
	status := data.PluginStatus{BrokerRunning: true}
	entities := []any{&data.ThermometerView{Name: "T"}, &data.DoorSensorView{Name: "D"}}

	container := &listContainer{}
	h := &Handler{container: container, entityStore: &fakeStore{result: common.StatusAndEntities{Status: status, Entities: entities}}}

	h.handleHttpListRequest(httptest.NewRecorder(), httptest.NewRequest("GET", "/plugins/esphome/", nil))

	call := container.call
	if !call.rendered || call.options.Title != "ESPHome Devices" || len(call.items) != 2 {
		t.Fatalf("unexpected call %+v", call)
	}

	header, ok := call.options.Header.(*data.PluginStatus)
	if !ok || !header.BrokerRunning {
		t.Fatalf("the status should be the header, as a pointer, got %#v", call.options.Header)
	}

	if _, ok := call.items[0].(*data.ThermometerView); !ok {
		t.Errorf("unexpected first item %T", call.items[0])
	}

	if _, ok := call.items[1].(*data.DoorSensorView); !ok {
		t.Errorf("unexpected second item %T", call.items[1])
	}
}

func TestHandleHttpListRequest_StillRendersAPageWhenTheStatusCannotBeRetrieved(t *testing.T) {
	container := &listContainer{}
	h := &Handler{container: container, entityStore: &fakeStore{err: errors.New("the plugin is stopping")}}

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
