package esphome

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
	spi "github.com/avanha/pmaas-spi"
	"github.com/avanha/pmaas-spi/bluetooth"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
)

// A device that runs ESPHome's Bluetooth proxy hears Bluetooth LE advertisements and sends them to whoever subscribes. Here
// it is a source of them (see pmaas-spi/bluetooth): the raw advertisements are decoded, the ones the bluetooth plugin has
// said it wants are broadcast, and the device is a source entity of its own, with the connection's state and counters.
//
// What decides whether a device is one is what it says about itself, so nothing is configured; a device can be told not to
// be one (DisableBluetooth).

// proxyRawAdvertisements is the bit of DeviceInfoResponse.bluetooth_proxy_feature_flags that says the device sends raw
// advertisements, which is all that ESPHome 2025.8 and later send.
const proxyRawAdvertisements = 1 << 5

// subscribeRaw is the flag on the subscription that asks for raw advertisements.
const subscribeRaw = 1

// bluetoothProxies are the Bluetooth proxies among the devices. Methods say which goroutine they run on: the plugin's, or
// the connection's read loop, which has to be quick (a slow handler makes the device look dead).
type bluetoothProxies struct {
	container spi.IPMAASContainer
	interests *bluetooth.Interests

	// send queues a message for a device. now is the time.
	send func(deviceName string, message proto.Message) error
	now  func() time.Time

	// displayName is what the device is called on the page.
	displayName func(deviceName string) string

	// disabled are the devices that are not to be used as sources, by name.
	disabled map[string]bool

	mu      sync.Mutex
	proxies map[string]*proxy
}

// proxy is one device. The fields under "plugin goroutine" belong to it; the rest may be read by the connection's.
type proxy struct {
	deviceName string
	state      *bluetooth.SourceState
	coalescer  *bluetooth.Coalescer
	sightings  *bluetooth.SightingTracker

	// entityId is set once, under the lock, before advertisements are forwarded.
	entityId string

	// Plugin goroutine:
	connected        bool
	subscribed       bool
	mode             pb.BluetoothScannerMode
	modeKnown        bool
	modeRequested    pb.BluetoothScannerMode
	modeWasRequested bool
}

func newBluetoothProxies(container spi.IPMAASContainer, send func(string, proto.Message) error, displayName func(string) string, disabled map[string]bool) *bluetoothProxies {
	return &bluetoothProxies{
		container:   container,
		interests:   bluetooth.NewInterests(),
		send:        send,
		now:         time.Now,
		displayName: displayName,
		disabled:    disabled,
		proxies:     make(map[string]*proxy),
	}
}

// isProxy is whether the device says it sends raw advertisements.
func isProxy(info *pb.DeviceInfoResponse) bool {
	return info.GetBluetoothProxyFeatureFlags()&proxyRawAdvertisements != 0
}

// onInterest takes in what the bluetooth plugin wants. Runs on the plugin goroutine.
func (b *bluetoothProxies) onInterest(eventInfo *events.EventInfo) error {
	event, ok := eventInfo.Event.(bluetooth.InterestEvent)
	if !ok {
		return nil
	}

	if err := b.interests.Set(event); err != nil {
		fmt.Printf("esphome: ignoring an interest from %q: %v\n", event.Owner, err)

		return nil
	}

	b.reconcile()

	return nil
}

// deviceInfo is told what a device says about itself, which it does on each connection. Runs on the plugin goroutine.
func (b *bluetoothProxies) deviceInfo(deviceName string, info *pb.DeviceInfoResponse) {
	if b.disabled[deviceName] || !isProxy(info) {
		return
	}

	b.mu.Lock()
	p := b.proxies[deviceName]
	b.mu.Unlock()

	if p == nil {
		p = b.add(deviceName)
		if p == nil {
			return
		}
	}

	p.connected = true
	p.state.SetConnectivity(environment.ConnectivityOnline, b.now())

	b.reconcileOne(p)
}

// add makes the source for a device, and registers it. Runs on the plugin goroutine.
func (b *bluetoothProxies) add(deviceName string) *proxy {
	name := b.displayName(deviceName) + " Bluetooth"

	p := &proxy{
		deviceName: deviceName,
		state:      bluetooth.NewSourceState(name, "esphome-proxy"),
		coalescer:  bluetooth.NewCoalescerFor(b.interests),
		sightings:  bluetooth.NewSightingTracker(b.interests),
	}

	id, err := b.container.RegisterEntity(deviceName+"/bluetooth", bluetooth.ISourceType, name,
		func() (any, error) { return p.state, nil })
	if err != nil {
		fmt.Printf("esphome: unable to register %s as a Bluetooth source: %v\n", deviceName, err)

		return nil
	}

	p.entityId = id

	b.mu.Lock()
	b.proxies[deviceName] = p
	b.mu.Unlock()

	fmt.Printf("esphome: %s is a Bluetooth proxy\n", deviceName)

	return p
}

// disconnected is told a device's connection ended. Runs on the plugin goroutine.
func (b *bluetoothProxies) disconnected(deviceName string) {
	b.mu.Lock()
	p := b.proxies[deviceName]
	b.mu.Unlock()

	if p == nil {
		return
	}

	// A new connection has nothing subscribed, and may not be in the mode it was left in.
	p.connected, p.subscribed, p.modeKnown, p.modeWasRequested = false, false, false, false
	p.state.SetConnectivity(environment.ConnectivityOffline, b.now())
	p.state.SetScanning(bluetooth.ScanOff)
}

// scannerState is told what a proxy's scanner is doing. Runs on the plugin goroutine.
func (b *bluetoothProxies) scannerState(deviceName string, state *pb.BluetoothScannerStateResponse) {
	b.mu.Lock()
	p := b.proxies[deviceName]
	b.mu.Unlock()

	if p == nil {
		return
	}

	p.mode, p.modeKnown = state.GetMode(), true

	switch {
	case state.GetState() != pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_RUNNING:
		p.state.SetScanning(bluetooth.ScanOff)
	case state.GetMode() == pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_ACTIVE:
		p.state.SetScanning(bluetooth.ScanActive)
	default:
		p.state.SetScanning(bluetooth.ScanPassive)
	}

	b.reconcileOne(p)
}

// reconcile has every connected proxy do what is wanted: listen if anybody wants anything and not otherwise, and scan the way
// that is wanted. Runs on the plugin goroutine, when what is wanted changes and every minute, which is what notices an
// interest that has lapsed.
func (b *bluetoothProxies) reconcile() {
	b.mu.Lock()
	proxies := make([]*proxy, 0, len(b.proxies))
	for _, p := range b.proxies {
		proxies = append(proxies, p)
	}
	b.mu.Unlock()

	for _, p := range proxies {
		b.reconcileOne(p)
	}
}

func (b *bluetoothProxies) reconcileOne(p *proxy) {
	if !p.connected {
		return
	}

	wanted := b.interests.Interested()

	if wanted && !p.subscribed {
		if err := b.send(p.deviceName, &pb.SubscribeBluetoothLEAdvertisementsRequest{Flags: subscribeRaw}); err != nil {
			fmt.Printf("esphome: unable to subscribe to %s's advertisements: %v\n", p.deviceName, err)

			return
		}

		p.subscribed = true
	} else if !wanted && p.subscribed {
		if err := b.send(p.deviceName, &pb.UnsubscribeBluetoothLEAdvertisementsRequest{}); err != nil {
			fmt.Printf("esphome: unable to unsubscribe from %s's advertisements: %v\n", p.deviceName, err)

			return
		}

		p.subscribed = false
	}

	if !wanted || !p.modeKnown {
		return
	}

	// Scan actively if anybody needs scan responses, and passively if not. Asked for once; a proxy that won't is left alone.
	desired := pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_PASSIVE
	if b.interests.ActiveScan() {
		desired = pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_ACTIVE
	}

	if desired == p.mode || (p.modeWasRequested && p.modeRequested == desired) {
		return
	}

	if err := b.send(p.deviceName, &pb.BluetoothScannerSetModeRequest{Mode: desired}); err != nil {
		fmt.Printf("esphome: unable to set %s's scanning mode: %v\n", p.deviceName, err)

		return
	}

	p.modeRequested, p.modeWasRequested = desired, true
}

// advertisements takes in the advertisements a proxy has heard and broadcasts those that are wanted. Runs on the connection's
// read loop.
func (b *bluetoothProxies) advertisements(deviceName string, message *pb.BluetoothLERawAdvertisementsResponse) {
	b.mu.Lock()
	p := b.proxies[deviceName]
	b.mu.Unlock()

	if p == nil || p.entityId == "" {
		return
	}

	now := b.now()

	for _, advertised := range message.GetAdvertisements() {
		p.state.Heard(now)

		raw := bluetooth.TrimAdvertisingData(advertised.GetData())

		// What is left after trimming is whole structures, so there is no error to speak of: a structure cut off at the
		// end of the data was trimmed away, and what came before it is good.
		fields, _ := bluetooth.ParseAdvertisingData(raw)

		event := bluetooth.AdvertisementEvent{
			Source:      p.entityId,
			Address:     bluetooth.FormatAddress(advertised.GetAddress()),
			AddressType: addressTypeOf(advertised.GetAddressType()),
			RSSI:        int(advertised.GetRssi()),
			Time:        now,
			Fields:      fields,
		}

		verdict, _ := p.coalescer.Admit(&event)

		if verdict == bluetooth.NotWanted {
			// Nobody wants what it said, but somebody may want to know it is around.
			if sighting, due := p.sightings.Observe(&event); due {
				if err := bluetooth.PublishSighting(b.container, sighting); err != nil {
					p.state.Dropped()
					fmt.Printf("esphome: unable to broadcast a sighting heard by %s: %v\n", deviceName, err)
				} else {
					p.state.Sighted()
				}
			}

			continue
		}

		if verdict != bluetooth.Forward {
			continue
		}

		event.Raw = slices.Clone(raw)

		if err := bluetooth.PublishAdvertisement(b.container, event); err != nil {
			p.state.Dropped()
			fmt.Printf("esphome: unable to broadcast an advertisement heard by %s: %v\n", deviceName, err)

			continue
		}

		p.state.Forwarded()
	}
}

// addressTypeOf is the kind of address ESPHome says a device used: 0 is public. The others are random, or private addresses
// made from a key, which change.
func addressTypeOf(kind uint32) bluetooth.AddressType {
	if kind == 0 {
		return bluetooth.AddressPublic
	}

	return bluetooth.AddressRandom
}

// shutdown deregisters the sources. Runs on the plugin goroutine.
func (b *bluetoothProxies) shutdown() {
	b.mu.Lock()
	proxies := b.proxies
	b.proxies = make(map[string]*proxy)
	b.mu.Unlock()

	for _, p := range proxies {
		if err := b.container.DeregisterEntity(p.entityId); err != nil {
			fmt.Printf("esphome: unable to deregister %s's Bluetooth source: %v\n", p.deviceName, err)
		}
	}
}
