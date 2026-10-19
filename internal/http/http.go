package http

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"github.com/avanha/pmaas-plugin-esphome/data"
	"github.com/avanha/pmaas-plugin-esphome/internal/common"
	"github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/format"
	"github.com/avanha/pmaas-spi/logging"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"reflect"
)

//go:embed content/static content/templates
var contentFS embed.FS

// The templates are rendered by PMAAS's template engine plugin, which uses text/template: it does no HTML
// escaping, so every string a template prints has to go through the html function itself.

var funcMap = template.FuncMap{
	"RelativeTime":        format.Ago,
	"CelsiusToFahrenheit": celsiusToFahrenheit,
}

// deviceFuncMap is for the device card, which renders its children with RenderChild.
var deviceFuncMap = template.FuncMap{
	"RelativeTime": format.Ago,
	"RenderChild":  renderChild,
}

// The device card carries the styles and scripts of everything that can be inside it, since the page only collects
// those of what it renders itself.
var deviceTemplate = spi.TemplateInfo{
	Name:    "esphome_device",
	FuncMap: deviceFuncMap,
	Paths:   []string{"templates/esphome_device.htmlt"},
	Styles:  []string{"css/esphome_device.css", "css/esphome_entity.css"},
	Scripts: []string{"js/esphome_switch.js"},
}

var statusTemplate = spi.TemplateInfo{
	Name:    "esphome_status",
	FuncMap: funcMap,
	Paths:   []string{"templates/esphome_status.htmlt"},
	Styles:  []string{"css/esphome_status.css"},
}

var thermometerTemplate = spi.TemplateInfo{
	Name:    "esphome_thermometer",
	FuncMap: funcMap,
	Paths:   []string{"templates/esphome_thermometer.htmlt"},
}

var switchTemplate = spi.TemplateInfo{
	Name:    "esphome_switch",
	FuncMap: funcMap,
	Paths:   []string{"templates/esphome_switch.htmlt"},
}

var powerTemplate = spi.TemplateInfo{
	Name:    "esphome_power",
	FuncMap: funcMap,
	Paths:   []string{"templates/esphome_power.htmlt"},
}

var bluetoothProxyTemplate = spi.TemplateInfo{
	Name:    "esphome_bluetooth",
	FuncMap: funcMap,
	Paths:   []string{"templates/esphome_bluetooth.htmlt"},
}

var doorSensorTemplate = spi.TemplateInfo{
	Name:    "esphome_door_sensor",
	FuncMap: funcMap,
	Paths:   []string{"templates/esphome_door_sensor.htmlt"},
}

// Handler owns the plugin's HTTP surface: the page, and the renderers for what's on it. It reaches back into
// the plugin only through entityStore.
type Handler struct {
	log         *slog.Logger
	container   spi.IPMAASContainer
	entityStore common.EntityStore
}

func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) Init(container spi.IPMAASContainer, entityStore common.EntityStore) {
	h.container = container
	h.log = logging.For(container, "http")
	h.entityStore = entityStore

	container.ProvideContentFS(&contentFS, "content")
	container.EnableStaticContent("static")
	container.AddRouteWithOptions(
		"",
		h.handleHttpListRequest,
		&spi.HttpHandlerOptions{SupportsXsrfValidation: true, MenuLabel: "ESPHome"})
	container.RegisterEntityRenderer(
		reflect.TypeOf((*data.PluginStatus)(nil)).Elem(),
		h.rendererFactory(&statusTemplate, "esphome_status", func(entity any) (any, bool) {
			status, ok := entity.(*data.PluginStatus)
			return status, ok
		}))
	container.RegisterEntityRenderer(
		reflect.TypeOf((*deviceWithChildren)(nil)).Elem(),
		h.rendererFactory(&deviceTemplate, "esphome_device", func(entity any) (any, bool) {
			device, ok := entity.(*deviceWithChildren)
			return device, ok
		}))
	container.AddJsonRoute(
		"switch",
		func() any { return &switchRequest{} },
		h.handleHttpSwitchRequest)
}

// childKind is how one kind of child is rendered: which template, and how to get what the template wants from the
// child. The children are values, as they come in a device's list, and the templates take them as they are.
type childKind struct {
	template *spi.TemplateInfo
	name     string
	cast     func(child data.Child) (any, bool)
}

// childKinds has a renderer for each kind of child there is. The device card renders each child by its Kind.
var childKinds = map[string]childKind{
	data.KindSwitch: {&switchTemplate, "esphome_switch", func(c data.Child) (any, bool) {
		v, ok := c.(data.SwitchView)
		return v, ok
	}},
	data.KindPowerMonitor: {&powerTemplate, "esphome_power", func(c data.Child) (any, bool) {
		v, ok := c.(data.PowerView)
		return v, ok
	}},
	data.KindDoorSensor: {&doorSensorTemplate, "esphome_door_sensor", func(c data.Child) (any, bool) {
		v, ok := c.(data.DoorSensorView)
		return v, ok
	}},
	data.KindBluetoothProxy: {&bluetoothProxyTemplate, "esphome_bluetooth", func(c data.Child) (any, bool) {
		v, ok := c.(data.BluetoothProxyView)
		return v, ok
	}},
	data.KindThermometer: {&thermometerTemplate, "esphome_thermometer", func(c data.Child) (any, bool) {
		v, ok := c.(data.ThermometerView)
		return v, ok
	}},
}

// deviceWithChildren is a device and what's inside its card, each child with what renders it, the way netmon's host
// carries its interfaces. The core never sees it: it's what the page's own renderer is given.
type deviceWithChildren struct {
	data.DeviceStatus
	Children []*childWithRenderer
}

type childWithRenderer struct {
	Child  data.Child
	Render func(w io.Writer, child data.Child) error
}

// renderChild is what the device card calls for each child. A child that can't be rendered says so in the card, and
// doesn't stop the rest of the page.
func renderChild(child *childWithRenderer) (string, error) {
	var out bytes.Buffer

	if err := child.Render(&out, child.Child); err != nil {
		return "", err
	}

	return out.String(), nil
}

// newDeviceWithChildren readies a device for rendering by finding the renderer for each of its children. A child
// of a kind there's no renderer for is left out, and said so.
func (h *Handler) newDeviceWithChildren(device *data.DeviceView) *deviceWithChildren {
	result := &deviceWithChildren{DeviceStatus: device.DeviceStatus, Children: make([]*childWithRenderer, 0, len(device.Children))}

	for _, child := range device.Children {
		kind, known := childKinds[child.Kind()]
		if !known {
			h.log.Debug("no renderer for a child", "kind", child.Kind(), "name", device.Name)
			continue
		}

		renderer, err := h.rendererFactory(kind.template, kind.name, func(entity any) (any, bool) { return entity, true })()
		if err != nil {
			h.log.Error("unable to render a child", "kind", child.Kind(), "name", device.Name, "error", err)
			continue
		}

		cast := kind.cast

		result.Children = append(result.Children, &childWithRenderer{
			Child: child,
			Render: func(w io.Writer, c data.Child) error {
				value, ok := cast(c)
				if !ok {
					return fmt.Errorf("a %q child that isn't one: %T", c.Kind(), c)
				}

				return renderer.StreamingRenderFunc(w, value)
			},
		})
	}

	return result
}

func (h *Handler) handleHttpListRequest(w http.ResponseWriter, r *http.Request) {
	result, err := h.entityStore.GetStatusAndEntities()

	if err != nil {
		h.log.Error("unable to retrieve the status", "error", err)
		result = common.StatusAndEntities{}
	}

	entities := make([]any, 0, len(result.Entities))

	for _, entity := range result.Entities {
		device, ok := entity.(*data.DeviceView)
		if !ok {
			h.log.Debug("unexpected entity in the list", "type", fmt.Sprintf("%T", entity))
			continue
		}

		entities = append(entities, h.newDeviceWithChildren(device))
	}

	h.container.RenderList(
		w,
		r,
		spi.RenderListOptions{Title: "ESPHome Devices", Header: &result.Status, Masonry: true},
		entities)
}

// switchRequest is what the switch card's button sends: which switch, and whether to turn it on or off.
type switchRequest struct {
	Device string
	Object string
	On     bool

	// What the person did when asked, for a switch that's guarded: said yes, and typed the password. The server
	// checks them against how the device's switches are guarded, and ignores what that doesn't call for.
	Confirmed bool
	Password  string
}

type switchResponse struct {
	// Requested says what was asked for. The switch itself changes when the device reports it has, which
	// the page shows the next time it loads.
	Requested bool
}

// handleHttpSwitchRequest turns a switch on or off. Like every JSON route it needs the XSRF cookie, which is
// what keeps another site from making a visitor's browser flip a relay.
func (h *Handler) handleHttpSwitchRequest(_ http.ResponseWriter, r *http.Request, request any) (any, error) {
	if r.Method != http.MethodPost {
		return nil, errors.New("method not allowed")
	}

	body, ok := request.(*switchRequest)

	if !ok || body.Device == "" || body.Object == "" {
		return nil, errors.New("a device and a switch are required")
	}

	if err := h.entityStore.SetSwitch(body.Device, body.Object, body.On, common.Confirmation{Confirmed: body.Confirmed, Password: body.Password}); err != nil {
		return nil, err
	}

	return switchResponse{Requested: body.On}, nil
}

// rendererFactory builds the factory for the renderer of one kind of item: it loads the template, and
// returns a function that checks an item is the kind expected and executes the template against it.
func (h *Handler) rendererFactory(
	templateInfo *spi.TemplateInfo, name string, cast func(entity any) (any, bool)) spi.EntityRendererFactory {
	return func() (spi.EntityRenderer, error) {
		compiledTemplate, err := h.container.GetTemplate(templateInfo)

		if err != nil {
			return spi.EntityRenderer{}, fmt.Errorf("unable to load %s template: %v", name, err)
		}

		renderer := func(w io.Writer, entity any) error {
			value, ok := cast(entity)

			if !ok {
				return fmt.Errorf("item is not the kind of thing the %s template renders: %T", name, entity)
			}

			if err := compiledTemplate.Instance.Execute(w, value); err != nil {
				return fmt.Errorf("unable to execute %s template: %w", name, err)
			}

			return nil
		}

		return spi.EntityRenderer{
			StreamingRenderFunc: renderer,
			Styles:              compiledTemplate.Styles,
			Scripts:             compiledTemplate.Scripts,
		}, nil
	}
}

func celsiusToFahrenheit(celsius float32) float32 {
	return celsius*9/5 + 32
}
