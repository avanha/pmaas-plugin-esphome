package esphome

import (
	"github.com/avanha/pmaas-plugin-esphome/internal/common"
	spi "github.com/avanha/pmaas-spi"
)

// Force implementation of common.EntityStore
var _ common.EntityStore = (*entityStoreAdapter)(nil)

type entityStoreAdapter struct {
	parent *plugin
}

// GetStatusAndEntities implements common.EntityStore. HTTP requests come in on arbitrary goroutines, so
// this runs getStatusAndEntities on the plugin's own goroutine, where all the state is, and waits for it.
func (a *entityStoreAdapter) GetStatusAndEntities() (common.StatusAndEntities, error) {
	return spi.Exec(a.parent.container, a.parent.getStatusAndEntities)
}
