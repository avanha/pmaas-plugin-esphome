package esphome

import (
	"github.com/avanha/pmaas-plugin-esphome/internal/common"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/device"
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

// SetSwitch implements common.EntityStore, on the plugin's goroutine for the same reason. The error is
// either that the request couldn't be handed to that goroutine, or what the switch said about it.
func (a *entityStoreAdapter) SetSwitch(deviceName string, object string, on bool) error {
	state := device.SwitchStateOff
	if on {
		state = device.SwitchStateOn
	}

	commandErr, err := spi.Exec(a.parent.container, func() error {
		return a.parent.registry.setSwitch(deviceName, object, state)
	})
	if err != nil {
		return err
	}

	return commandErr
}
