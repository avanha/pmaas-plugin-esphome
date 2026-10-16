package esphome

import (
	"errors"
	"fmt"
	"strings"

	"github.com/avanha/pmaas-plugin-esphome/internal/entities"
	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
	"github.com/avanha/pmaas-spi/device"
	"github.com/avanha/pmaas-spi/events"
)

// switchChannel is a discovered switch and the entity registered for it.
type switchChannel struct {
	disc hadiscovery.Entity
	sw   *entities.Switch

	// native is set for a switch on a device reached over the native API, where a command is a message to the
	// device naming the switch by key, rather than a publish to a topic.
	native    bool
	nativeKey uint32
}

// commandable is whether there's anywhere to send a command for the switch.
func (ch *switchChannel) commandable() bool {
	return ch.native || ch.disc.CommandTopic != ""
}

func (r *registry) upsertSwitch(n *node, e hadiscovery.Entity) {
	ch, ok := n.switches[e.ObjectID]
	if !ok {
		ch = &switchChannel{}
		n.switches[e.ObjectID] = ch
	}

	ch.disc = e

	if ch.sw == nil {
		r.registerSwitch(n, ch)
		return
	}

	// Already registered: only a changed name is worth telling anyone. The topics and payloads are read from
	// ch.disc each time they're used, so a changed one takes effect without anything else.
	if name := r.entityName(n.name, e); name != ch.sw.Data.Name {
		ch.sw.Data.Name = name

		if handle := r.handles[n.name]; handle != nil {
			handle.switchRenamed(ch.sw.PmaasEntityId, name)
		}

		r.broadcastState(ch.sw.PmaasEntityId, device.ISwitchType, name, ch.sw.Data)
	}
}

func (r *registry) registerSwitch(n *node, ch *switchChannel) {
	sw := &entities.Switch{}
	sw.Data.Name = r.entityName(n.name, ch.disc)
	sw.Command = func(state device.SwitchState) error { return r.commandSwitch(n, ch, state) }
	r.applyConnectivity(n, &sw.Data.Connectivity, &sw.Data.OnlineSince, &sw.Data.OfflineSince)

	id, err := r.container.RegisterEntity(
		n.name+"/"+ch.disc.ObjectID,
		device.ISwitchType,
		sw.Data.Name,
		func() (any, error) { return sw.GetStub(r.container), nil })
	if err != nil {
		r.logf("%s: unable to register switch %q: %v", n.name, ch.disc.ObjectID, err)
		return
	}

	sw.PmaasEntityId = id
	ch.sw = sw

	if handle := r.handles[n.name]; handle != nil {
		handle.switchRegistered(r.container, id, sw.Data.Name, sw.GetStub(r.container))
	}
}

func (r *registry) deregisterSwitch(n *node, ch *switchChannel) {
	if ch.sw == nil {
		return
	}

	if handle := r.handles[n.name]; handle != nil {
		handle.switchDeregistered(ch.sw.PmaasEntityId)
	}

	if err := r.container.DeregisterEntity(ch.sw.PmaasEntityId); err != nil {
		r.logf("unable to deregister switch %s: %v", ch.sw.PmaasEntityId, err)
	}

	ch.sw.CloseStubIfPresent()
	ch.sw = nil
}

func (r *registry) updateSwitchConnectivity(n *node, ch *switchChannel) {
	if ch.sw == nil {
		return
	}

	r.applyConnectivity(n, &ch.sw.Data.Connectivity, &ch.sw.Data.OnlineSince, &ch.sw.Data.OfflineSince)
	r.broadcastState(ch.sw.PmaasEntityId, device.ISwitchType, ch.sw.Data.Name, ch.sw.Data)
}

// commandSwitch asks the device to put a switch in state, which has already been checked as On or Off, by
// publishing to its command topic. It returns once the request is queued, and the switch's state changes
// when the device reports it, not before. Runs on the plugin goroutine.
func (r *registry) commandSwitch(n *node, ch *switchChannel, state device.SwitchState) error {
	name := r.entityName(n.name, ch.disc)

	switch {
	case r.controlOf(n.name).Level == ControlReadOnly:
		// Nothing changes a read-only switch, whoever asks: a person at the page or another plugin.
		return fmt.Errorf("unable to set %q to %s: %w", name, state, errReadOnly)
	case r.stopped || (!ch.native && r.publish == nil):
		return fmt.Errorf("unable to set %q to %s: the MQTT broker isn't running", name, state)
	case !n.connected:
		return fmt.Errorf("unable to set %q to %s: the device is offline", name, state)
	case !ch.commandable():
		return fmt.Errorf("unable to set %q to %s: the device didn't announce a command topic for it", name, state)
	}

	if ch.native {
		return r.commandNativeSwitch(n, ch, name, state)
	}

	payload := ch.disc.CommandPayloadOff
	if state == device.SwitchStateOn {
		payload = ch.disc.CommandPayloadOn
	}

	if err := r.publish(ch.disc.CommandTopic, []byte(payload)); err != nil {
		return fmt.Errorf("unable to set %q to %s: %w", name, state, err)
	}

	return nil
}

// errBrokerNotRunning is what the plugin's publish function returns when there's no broker to publish with.
var errBrokerNotRunning = errors.New("the MQTT broker isn't running")

func (r *registry) handleSwitchState(n *node, ch *switchChannel, payload []byte) {
	if ch.sw == nil {
		return
	}

	switch strings.TrimSpace(string(payload)) {
	case ch.disc.StatePayloadOn:
		r.applySwitchState(n, ch, device.SwitchStateOn)
	case ch.disc.StatePayloadOff:
		r.applySwitchState(n, ch, device.SwitchStateOff)
	default:
		r.logf("%s: unrecognized switch state %q on %s", n.name, payload, ch.disc.StateTopic)
	}
}

// applySwitchState takes in the state a switch reported, however it arrived.
func (r *registry) applySwitchState(n *node, ch *switchChannel, state device.SwitchState) {
	if ch.sw == nil {
		return
	}

	s := ch.sw
	old := s.Data
	now := r.now()

	s.Data.LastUpdateTime = now

	switch {
	case !old.HasData:
		// The first report establishes the state; it isn't known to be a change.
		s.Data.HasData = true
		s.Data.State = state
	case old.State != state:
		s.Data.State = state
		s.Data.LastChangeTime = now
	default:
		return
	}

	r.broadcastState(s.PmaasEntityId, device.ISwitchType, s.Data.Name, s.Data)

	if old.HasData {
		r.broadcast(s.PmaasEntityId, device.SwitchStateChangedEvent{
			EntityEvent: events.EntityEvent{
				Id: s.PmaasEntityId, EntityType: device.ISwitchType, Name: s.Data.Name,
			},
			NewState: state,
			OldState: old.State,
		})
	}
}
