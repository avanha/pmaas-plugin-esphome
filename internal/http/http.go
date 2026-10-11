package http

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/data"
	"github.com/avanha/pmaas-plugin-esphome/internal/common"
	"github.com/avanha/pmaas-spi"
)

//go:embed content/static content/templates
var contentFS embed.FS

// The templates are rendered by PMAAS's template engine plugin, which uses text/template: it does no HTML
// escaping, so every string a template prints has to go through the html function itself.

var funcMap = template.FuncMap{
	"RelativeTime":        relativeTime,
	"CelsiusToFahrenheit": celsiusToFahrenheit,
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
	Styles:  []string{"css/esphome_entity.css"},
}

var doorSensorTemplate = spi.TemplateInfo{
	Name:    "esphome_door_sensor",
	FuncMap: funcMap,
	Paths:   []string{"templates/esphome_door_sensor.htmlt"},
	Styles:  []string{"css/esphome_entity.css"},
}

// Handler owns the plugin's HTTP surface: the page, and the renderers for what's on it. It reaches back into
// the plugin only through entityStore.
type Handler struct {
	container   spi.IPMAASContainer
	entityStore common.EntityStore
}

func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) Init(container spi.IPMAASContainer, entityStore common.EntityStore) {
	h.container = container
	h.entityStore = entityStore

	container.ProvideContentFS(&contentFS, "content")
	container.EnableStaticContent("static")
	container.AddRouteWithOptions(
		"",
		h.handleHttpListRequest,
		&spi.HttpHandlerOptions{SupportsXsrfValidation: true, MenuLabel: "ESPHome", MenuIcon: "cpu"})
	container.RegisterEntityRenderer(
		reflect.TypeOf((*data.PluginStatus)(nil)).Elem(),
		h.rendererFactory(&statusTemplate, "esphome_status", func(entity any) (any, bool) {
			status, ok := entity.(*data.PluginStatus)
			return status, ok
		}))
	container.RegisterEntityRenderer(
		reflect.TypeOf((*data.ThermometerView)(nil)).Elem(),
		h.rendererFactory(&thermometerTemplate, "esphome_thermometer", func(entity any) (any, bool) {
			view, ok := entity.(*data.ThermometerView)
			return view, ok
		}))
	container.RegisterEntityRenderer(
		reflect.TypeOf((*data.DoorSensorView)(nil)).Elem(),
		h.rendererFactory(&doorSensorTemplate, "esphome_door_sensor", func(entity any) (any, bool) {
			view, ok := entity.(*data.DoorSensorView)
			return view, ok
		}))
}

func (h *Handler) handleHttpListRequest(w http.ResponseWriter, r *http.Request) {
	result, err := h.entityStore.GetStatusAndEntities()

	if err != nil {
		fmt.Printf("esphome.http handleHttpListRequest: unable to retrieve the status: %v\n", err)
		result = common.StatusAndEntities{}
	}

	h.container.RenderList(
		w,
		r,
		spi.RenderListOptions{Title: "ESPHome Devices", Header: &result.Status},
		result.Entities)
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

// relativeTime says how long ago a time was, in its two most significant units: "5m ago", "2h 5m ago",
// "3d 4h ago". A time that hasn't happened, or is still in the future by a little (the clocks of a device and
// this server needn't agree), reads as "just now", and one that never did as "never".
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	elapsed := time.Since(t).Truncate(time.Minute)

	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(elapsed.Hours()), int(elapsed.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh ago", int(elapsed.Hours())/24, int(elapsed.Hours())%24)
	}
}

func celsiusToFahrenheit(celsius float32) float32 {
	return celsius*9/5 + 32
}
