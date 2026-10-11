package common

import "github.com/avanha/pmaas-plugin-esphome/data"

// StatusAndEntities is everything the plugin's page shows: the status block, and the entities below it.
type StatusAndEntities struct {
	Status data.PluginStatus

	// Entities are pointers to *data.ThermometerView and *data.DoorSensorView, ordered by device and then
	// name. They're pointers so the render layer, which looks a renderer up by type, gets the same kind of
	// value it does for the status block.
	Entities []any
}

// EntityStore is how internal/http reaches back into the plugin for what to show. The plugin implements it
// by crossing onto its own goroutine, which is where all the state lives.
type EntityStore interface {
	GetStatusAndEntities() (StatusAndEntities, error)
}
