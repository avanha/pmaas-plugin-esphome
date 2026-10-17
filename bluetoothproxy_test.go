package esphome

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
	"github.com/avanha/pmaas-spi/bluetooth"
	"github.com/avanha/pmaas-spi/clock"
	"github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
)

// Advertisements captured from a Bluetooth proxy, from sensors that are ours.
const (
	proxyGoveeFrame   = "0d09475648353037355f45324630030388ec02010509ff88ec000339a664001aff4c000215494e54454c4c495f524f434b535f48575075f2ff0c"
	proxyGoveeFrame2  = "0d09475648353037355f45324630030388ec02010509ff88ec000339bb64001aff4c000215494e54454c4c495f524f434b535f48575075f2ff0c"
	proxyInkbirdFrame = "0201060302f0ff04097370730aff6607970f0099606408"
	proxyAppleFrame   = "02011a020a0c0aff4c001005031cbf6469"
	proxyGoveeAddress = 0xA4C138CCE2F0
)

// sentMessages records what the proxies ask the devices to do.
type sentMessages struct {
	mu   sync.Mutex
	sent map[string][]proto.Message
	err  error
}

func (s *sentMessages) send(deviceName string, message proto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return s.err
	}

	if s.sent == nil {
		s.sent = make(map[string][]proto.Message)
	}

	s.sent[deviceName] = append(s.sent[deviceName], message)

	return nil
}

func (s *sentMessages) to(deviceName string) []proto.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]proto.Message(nil), s.sent[deviceName]...)
}

type proxyFixture struct {
	t         *testing.T
	container *fakeContainer
	sent      *sentMessages
	proxies   *bluetoothProxies
	clock     time.Time
}

func newProxyFixture(t *testing.T, disabled ...string) *proxyFixture {
	t.Helper()

	f := &proxyFixture{t: t, container: &fakeContainer{}, sent: &sentMessages{}, clock: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}

	disabledSet := map[string]bool{}
	for _, name := range disabled {
		disabledSet[name] = true
	}

	names := map[string]string{"bt-proxy-1": "Garage Proxy"}
	f.proxies = newBluetoothProxies(f.container, f.sent.send, func(deviceName string) string {
		if name, ok := names[deviceName]; ok {
			return name
		}

		return deviceName
	}, disabledSet)
	f.proxies.now = func() time.Time { return f.clock }

	return f
}

func proxyInfo() *pb.DeviceInfoResponse {
	return &pb.DeviceInfoResponse{Name: "bt-proxy-1", BluetoothProxyFeatureFlags: 0xff}
}

func (f *proxyFixture) connect(deviceName string) {
	f.t.Helper()

	f.proxies.deviceInfo(deviceName, &pb.DeviceInfoResponse{Name: deviceName, BluetoothProxyFeatureFlags: 0xff})
}

func (f *proxyFixture) want(rules ...bluetooth.Rule) {
	f.t.Helper()

	f.wanting(false, rules...)
}

func (f *proxyFixture) wanting(active bool, rules ...bluetooth.Rule) {
	f.t.Helper()

	event := bluetooth.InterestEvent{Owner: "bluetooth", Rules: rules, ActiveScan: active}
	if err := f.proxies.onInterest(&events.EventInfo{Event: event}); err != nil {
		f.t.Fatal(err)
	}
}

func goveeRule() bluetooth.Rule {
	return bluetooth.Rule{Name: "govee", ServiceUUID: bluetooth.UUID16(0xec88)}
}

// raw is the advertisements a proxy would send for these frames, each padded to the fixed size the device uses.
func raw(t *testing.T, address uint64, rssi int32, frames ...string) *pb.BluetoothLERawAdvertisementsResponse {
	t.Helper()

	message := &pb.BluetoothLERawAdvertisementsResponse{}

	for _, frame := range frames {
		data, err := hex.DecodeString(frame)
		if err != nil {
			t.Fatal(err)
		}

		message.Advertisements = append(message.Advertisements,
			&pb.BluetoothLERawAdvertisement{Address: address, Rssi: rssi, Data: append(data, make([]byte, 62-len(data))...)})
	}

	return message
}

func (f *proxyFixture) adverts() []bluetooth.AdvertisementEvent {
	var result []bluetooth.AdvertisementEvent

	for _, b := range f.container.broadcastsSoFar() {
		if a, ok := b.event.(bluetooth.AdvertisementEvent); ok {
			result = append(result, a)
		}
	}

	return result
}

func (f *proxyFixture) source(deviceName string) bluetooth.Source {
	f.t.Helper()

	f.proxies.mu.Lock()
	p := f.proxies.proxies[deviceName]
	f.proxies.mu.Unlock()

	if p == nil {
		f.t.Fatalf("%s is not a source", deviceName)
	}

	return p.state.GetSourceData()
}

func TestIsProxy(t *testing.T) {
	for flags, want := range map[uint32]bool{0: false, 1: false, 0x1f: false, 1 << 5: true, 0xff: true, 0x3f: true} {
		if got := isProxy(&pb.DeviceInfoResponse{BluetoothProxyFeatureFlags: flags}); got != want {
			t.Errorf("%#x: %v", flags, got)
		}
	}
}

func TestDeviceInfo_AProxyBecomesASourceNamedAfterTheDeviceAndAnythingElseDoesNot(t *testing.T) {
	f := newProxyFixture(t)

	f.proxies.deviceInfo("plug1", &pb.DeviceInfoResponse{Name: "plug1"})
	f.proxies.deviceInfo("heater", &pb.DeviceInfoResponse{Name: "heater", BluetoothProxyFeatureFlags: 0x1f}) // no raw advertisements

	if len(f.container.registrations) != 0 {
		t.Fatalf("registered %+v", f.container.registrations)
	}

	f.connect("bt-proxy-1")

	if len(f.container.registrations) != 1 {
		t.Fatalf("registered %+v", f.container.registrations)
	}

	r := f.container.registrations[0]
	if r.entityType != bluetooth.ISourceType || r.name != "Garage Proxy Bluetooth" || r.uniqueData != "bt-proxy-1/bluetooth" {
		t.Errorf("registration %+v", r)
	}

	stub, err := r.stubFactory()
	if err != nil {
		t.Fatal(err)
	}

	data := stub.(bluetooth.ISource).GetSourceData()
	if data.Kind != "esphome-proxy" || data.Name != "Garage Proxy Bluetooth" || data.Connectivity != environment.ConnectivityOnline || !data.OnlineSince.Equal(f.clock) {
		t.Errorf("source %+v", data)
	}
}

func TestDeviceInfo_ADeviceTheAssemblyDisabledIsNotASourceEvenIfItIsAProxy(t *testing.T) {
	f := newProxyFixture(t, "bt-proxy-1")
	f.connect("bt-proxy-1")

	if len(f.container.registrations) != 0 {
		t.Errorf("registered %+v", f.container.registrations)
	}
}

func TestDeviceInfo_ReconnectingDoesNotRegisterAgain(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.proxies.disconnected("bt-proxy-1")
	f.connect("bt-proxy-1")

	if len(f.container.registrations) != 1 {
		t.Errorf("registered %d times", len(f.container.registrations))
	}
}

func TestInterest_SubscribesOnceWhenSomethingIsWantedAndNotBefore(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")

	if got := f.sent.to("bt-proxy-1"); len(got) != 0 {
		t.Fatalf("subscribed with nothing wanted: %v", got)
	}

	f.want(goveeRule())
	f.want(goveeRule()) // said again, as it is every minute
	f.proxies.reconcile()

	got := f.sent.to("bt-proxy-1")
	if len(got) != 1 {
		t.Fatalf("sent %v", got)
	}

	if subscribe, ok := got[0].(*pb.SubscribeBluetoothLEAdvertisementsRequest); !ok || subscribe.Flags != 1 {
		t.Errorf("sent %v", got[0])
	}
}

func TestInterest_ADeviceThatConnectsLaterSubscribesAtOnce(t *testing.T) {
	f := newProxyFixture(t)
	f.want(goveeRule())
	f.connect("bt-proxy-1")

	got := f.sent.to("bt-proxy-1")
	if len(got) != 1 {
		t.Fatalf("sent %v", got)
	}

	if _, ok := got[0].(*pb.SubscribeBluetoothLEAdvertisementsRequest); !ok {
		t.Errorf("sent %v", got[0])
	}
}

func TestInterest_AReconnectionSubscribesAgainBecauseTheNewConnectionHasNothing(t *testing.T) {
	f := newProxyFixture(t)
	f.want(goveeRule())
	f.connect("bt-proxy-1")
	f.proxies.disconnected("bt-proxy-1")
	f.connect("bt-proxy-1")

	if got := f.sent.to("bt-proxy-1"); len(got) != 2 {
		t.Errorf("sent %v", got)
	}
}

func TestInterest_UnsubscribesWhenNothingIsWantedAnyMoreAndWhenWhatWasWantedLapses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProxyFixture(t)
		f.connect("bt-proxy-1")
		f.want(goveeRule())

		// Withdrawn.
		f.proxies.onInterest(&events.EventInfo{Event: bluetooth.InterestEvent{Owner: "bluetooth"}})

		got := f.sent.to("bt-proxy-1")
		if len(got) != 2 {
			t.Fatalf("sent %v", got)
		}

		if _, ok := got[1].(*pb.UnsubscribeBluetoothLEAdvertisementsRequest); !ok {
			t.Errorf("sent %v", got[1])
		}

		// Wanted again, and then not said again: it lapses, which the minute notices.
		f.want(goveeRule())
		time.Sleep(bluetooth.InterestTTL + time.Second)
		f.proxies.reconcile()

		got = f.sent.to("bt-proxy-1")
		if len(got) != 4 {
			t.Fatalf("sent %v", got)
		}

		if _, ok := got[3].(*pb.UnsubscribeBluetoothLEAdvertisementsRequest); !ok {
			t.Errorf("sent %v", got[3])
		}
	})
}

func TestInterest_ASubscriptionThatCouldNotBeSentIsTriedAgainAtTheNextOpportunity(t *testing.T) {
	f := newProxyFixture(t)
	f.sent.err = context.DeadlineExceeded
	f.connect("bt-proxy-1")
	f.want(goveeRule())

	if got := f.sent.to("bt-proxy-1"); len(got) != 0 {
		t.Fatalf("sent %v", got)
	}

	f.sent.mu.Lock()
	f.sent.err = nil
	f.sent.mu.Unlock()

	f.proxies.reconcile()

	if got := f.sent.to("bt-proxy-1"); len(got) != 1 {
		t.Errorf("sent %v", got)
	}
}

func TestScannerMode_IsSetToWhatIsWantedOnceAndLeftAloneIfItAlreadyIs(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.wanting(false, goveeRule()) // passive is enough

	running := func(mode pb.BluetoothScannerMode) *pb.BluetoothScannerStateResponse {
		return &pb.BluetoothScannerStateResponse{State: pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_RUNNING, Mode: mode}
	}

	modes := func() (modes []pb.BluetoothScannerMode) {
		for _, m := range f.sent.to("bt-proxy-1") {
			if set, ok := m.(*pb.BluetoothScannerSetModeRequest); ok {
				modes = append(modes, set.Mode)
			}
		}

		return modes
	}

	// Already passive: nothing to do, and the source says so.
	f.proxies.scannerState("bt-proxy-1", running(pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_PASSIVE))

	if len(modes()) != 0 || f.source("bt-proxy-1").Scanning != bluetooth.ScanPassive {
		t.Fatalf("modes %v, scanning %v", modes(), f.source("bt-proxy-1").Scanning)
	}

	// Active scanning wanted: asked for, once, however often it is reconciled.
	f.wanting(true, goveeRule())
	f.proxies.reconcile()
	f.proxies.reconcile()

	if got := modes(); !reflect.DeepEqual(got, []pb.BluetoothScannerMode{pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_ACTIVE}) {
		t.Fatalf("modes %v", got)
	}

	// It does what it was asked.
	f.proxies.scannerState("bt-proxy-1", running(pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_ACTIVE))

	if f.source("bt-proxy-1").Scanning != bluetooth.ScanActive || len(modes()) != 1 {
		t.Errorf("scanning %v, modes %v", f.source("bt-proxy-1").Scanning, modes())
	}

	// And not wanted any more: back to passive.
	f.wanting(false, goveeRule())

	if got := modes(); len(got) != 2 || got[1] != pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_PASSIVE {
		t.Errorf("modes %v", got)
	}
}

func TestScannerMode_IsLeftAloneWhenNothingIsWantedAndAskedForAgainAfterAReconnection(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.wanting(true, goveeRule())

	active := &pb.BluetoothScannerStateResponse{State: pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_RUNNING, Mode: pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_ACTIVE}
	passive := &pb.BluetoothScannerStateResponse{State: pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_RUNNING, Mode: pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_PASSIVE}

	setModes := func() int {
		count := 0

		for _, m := range f.sent.to("bt-proxy-1") {
			if _, ok := m.(*pb.BluetoothScannerSetModeRequest); ok {
				count++
			}
		}

		return count
	}

	f.proxies.scannerState("bt-proxy-1", active)

	// Nobody wants anything: the proxy is not touched, whatever mode it is in.
	f.proxies.onInterest(&events.EventInfo{Event: bluetooth.InterestEvent{Owner: "bluetooth"}})
	f.proxies.scannerState("bt-proxy-1", passive)

	if setModes() != 0 {
		t.Fatalf("a mode was set with nothing wanted: %v", f.sent.to("bt-proxy-1"))
	}

	// Active wanted, the proxy passive: asked once. It reconnects, passive again: asked again.
	f.wanting(true, goveeRule())

	if setModes() != 1 {
		t.Fatalf("%d mode requests", setModes())
	}

	f.proxies.disconnected("bt-proxy-1")
	f.connect("bt-proxy-1")
	f.proxies.scannerState("bt-proxy-1", passive)

	if setModes() != 2 {
		t.Errorf("%d mode requests after a reconnection", setModes())
	}
}

func TestOnClockEvent_MakesTheProxiesDoWhatIsWantedSoALapsedInterestIsNoticed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProxyFixture(t)
		p := &plugin{container: f.container, registry: newRegistry(f.container, "homeassistant", nil), proxies: f.proxies}

		f.connect("bt-proxy-1")
		f.want(goveeRule())

		time.Sleep(bluetooth.InterestTTL + time.Second)

		if err := p.onClockEvent(&events.EventInfo{Event: clock.MinuteChangedEvent{Time: f.clock}}); err != nil {
			t.Fatal(err)
		}

		got := f.sent.to("bt-proxy-1")
		if len(got) != 2 {
			t.Fatalf("sent %v", got)
		}

		if _, ok := got[1].(*pb.UnsubscribeBluetoothLEAdvertisementsRequest); !ok {
			t.Errorf("sent %v", got[1])
		}

		// Another kind of event does nothing.
		_ = p.onClockEvent(&events.EventInfo{Event: "something else"})
	})
}

func TestScannerMode_AScannerThatIsNotRunningIsNotScanning(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")

	for _, state := range []pb.BluetoothScannerState{
		pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_IDLE, pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_FAILED,
		pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_STOPPED,
	} {
		f.proxies.scannerState("bt-proxy-1", &pb.BluetoothScannerStateResponse{State: state, Mode: pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_ACTIVE})

		if got := f.source("bt-proxy-1").Scanning; got != bluetooth.ScanOff {
			t.Errorf("%v: %v", state, got)
		}
	}

	// Nothing is asked of a device that is not a proxy.
	f.proxies.scannerState("plug1", &pb.BluetoothScannerStateResponse{})
}

func TestAdvertisements_OnlyWhatIsWantedIsBroadcastDecodedAndFromTheSource(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.want(goveeRule())

	f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame, proxyAppleFrame, proxyInkbirdFrame))

	got := f.adverts()
	if len(got) != 1 {
		t.Fatalf("broadcast %d", len(got))
	}

	a := got[0]
	if a.Source != f.container.registrations[0].id || a.Address != "A4:C1:38:CC:E2:F0" || a.AddressType != bluetooth.AddressPublic ||
		a.RSSI != -61 || !a.Time.Equal(f.clock) || a.LocalName != "GVH5075_E2F0" || len(a.ServiceUUIDs) != 1 ||
		a.ManufacturerData[0xec88][3] != 0xa6 {
		t.Errorf("event %+v", a)
	}

	// The data as received, without the padding, and the broadcast is from the source's own id.
	if want, _ := hex.DecodeString(proxyGoveeFrame); !reflect.DeepEqual(a.Raw, want) {
		t.Errorf("raw %x", a.Raw)
	}

	if s := f.source("bt-proxy-1"); s.Heard != 3 || s.Forwarded != 1 || s.Dropped != 0 || !s.LastHeard.Equal(f.clock) {
		t.Errorf("source %+v", s)
	}
}

func TestAdvertisements_NothingIsBroadcastUntilSomebodyHasSaidWhatTheyWant(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")

	f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame))

	if len(f.adverts()) != 0 {
		t.Error("broadcast with nothing wanted")
	}

	if s := f.source("bt-proxy-1"); s.Heard != 1 || s.Forwarded != 0 {
		t.Errorf("source %+v", s)
	}
}

func TestAdvertisements_RepeatsAreDroppedAndChangesAreNot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProxyFixture(t)
		f.connect("bt-proxy-1")
		f.want(goveeRule())

		f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame))
		f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -75, proxyGoveeFrame))  // a repeat, quieter
		f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame2)) // a new reading

		if got := f.adverts(); len(got) != 2 || got[1].ManufacturerData[0xec88][3] != 0xbb {
			t.Fatalf("broadcast %d", len(got))
		}

		// A repeat after the window is news again.
		time.Sleep(bluetooth.DefaultWindow + time.Second)
		f.want(goveeRule())
		f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame2))

		if got := f.adverts(); len(got) != 3 {
			t.Errorf("broadcast %d", len(got))
		}
	})
}

func TestAdvertisements_EachProxyDedupesItsOwnStreamSoBothAreHeard(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.connect("bt-proxy-2")
	f.want(goveeRule())

	f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -50, proxyGoveeFrame))
	f.proxies.advertisements("bt-proxy-2", raw(t, proxyGoveeAddress, -80, proxyGoveeFrame))
	f.proxies.advertisements("bt-proxy-2", raw(t, proxyGoveeAddress, -80, proxyGoveeFrame))

	got := f.adverts()
	if len(got) != 2 || got[0].Source == got[1].Source || got[0].RSSI != -50 || got[1].RSSI != -80 {
		t.Errorf("broadcast %+v", got)
	}
}

func TestAdvertisements_AStructureCutOffAtTheEndIsLeftOutAndWhatCameBeforeIsKept(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.want(bluetooth.Rule{Name: "all", NamePrefix: "s"})

	// All 62 bytes used: a name, a long manufacturer record, and then the start of a structure that claims ten bytes
	// with one left.
	data := make([]byte, 62)
	copy(data, []byte{4, 0x09, 's', 'p', 's'})
	data[5], data[6], data[7], data[8] = 54, 0xff, 0x66, 0x07 // 54 bytes: type, company ID, 51 bytes of data
	data[60], data[61] = 10, 0xff

	f.proxies.advertisements("bt-proxy-1", &pb.BluetoothLERawAdvertisementsResponse{
		Advertisements: []*pb.BluetoothLERawAdvertisement{{Address: 1, Rssi: -60, Data: data}},
	})

	got := f.adverts()
	if len(got) != 1 || got[0].LocalName != "sps" || len(got[0].ManufacturerData[0x0766]) != 51 {
		t.Fatalf("broadcast %+v", got)
	}

	if !reflect.DeepEqual(got[0].Raw, data[:60]) {
		t.Errorf("raw %x", got[0].Raw)
	}
}

func TestAdvertisements_ABroadcastThatFailsIsCountedAsDropped(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.want(goveeRule())

	f.container.broadcastErr = context.Canceled
	f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame))

	if s := f.source("bt-proxy-1"); s.Heard != 1 || s.Forwarded != 0 || s.Dropped != 1 {
		t.Errorf("source %+v", s)
	}
}

func TestAdvertisements_ADevicesAddressTypeIsMapped(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.want(goveeRule())

	for kind, want := range map[uint32]bluetooth.AddressType{0: bluetooth.AddressPublic, 1: bluetooth.AddressRandom, 2: bluetooth.AddressRandom, 3: bluetooth.AddressRandom} {
		message := raw(t, proxyGoveeAddress+uint64(kind), -60, proxyGoveeFrame)
		message.Advertisements[0].AddressType = kind
		f.proxies.advertisements("bt-proxy-1", message)

		got := f.adverts()
		if got[len(got)-1].AddressType != want {
			t.Errorf("type %d: %v", kind, got[len(got)-1].AddressType)
		}
	}
}

func TestAdvertisements_FromADeviceThatIsNotASourceAreIgnored(t *testing.T) {
	f := newProxyFixture(t)
	f.want(goveeRule())

	f.proxies.advertisements("plug1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame))

	if len(f.adverts()) != 0 {
		t.Error("broadcast from something that is not a source")
	}
}

func TestDisconnected_TheSourceGoesOfflineAndStopsScanning(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.proxies.scannerState("bt-proxy-1", &pb.BluetoothScannerStateResponse{State: pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_RUNNING})

	f.clock = f.clock.Add(time.Hour)
	f.proxies.disconnected("bt-proxy-1")
	f.proxies.disconnected("plug1")

	s := f.source("bt-proxy-1")
	if s.Connectivity != environment.ConnectivityOffline || !s.OfflineSince.Equal(f.clock) || s.Scanning != bluetooth.ScanOff {
		t.Errorf("source %+v", s)
	}
}

func TestShutdown_DeregistersTheSourcesAndStopsHearing(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")
	f.want(goveeRule())

	f.proxies.shutdown()

	if len(f.container.deregistered) != 1 {
		t.Errorf("deregistered %v", f.container.deregistered)
	}

	f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame))

	if len(f.adverts()) != 0 {
		t.Error("heard after shutdown")
	}
}

func TestOnInterest_AnInterestThatIsNotValidIsIgnoredAndOtherEventsAreNotInterests(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")

	if err := f.proxies.onInterest(&events.EventInfo{Event: bluetooth.InterestEvent{Owner: "bluetooth", Rules: []bluetooth.Rule{{}}}}); err != nil {
		t.Error(err)
	}

	if err := f.proxies.onInterest(&events.EventInfo{Event: "other"}); err != nil {
		t.Error(err)
	}

	if f.proxies.interests.Interested() || len(f.sent.to("bt-proxy-1")) != 0 {
		t.Error("an invalid interest was taken")
	}
}

// aProxyDial connects to a device that is a Bluetooth proxy: it says so, and once subscribed sends the advertisements.
func aProxyDial(conns chan<- *fakeNativeConn, frames ...string) nativeDialFunc {
	return func(ctx context.Context, cfg nativeapi.Config, handler nativeapi.Handler) (nativeConn, error) {
		conn := &proxyConn{fakeNativeConn: newFakeNativeConn(), frames: frames}
		conn.handler = handler
		conns <- conn.fakeNativeConn

		return conn, nil
	}
}

type proxyConn struct {
	*fakeNativeConn
	frames []string
}

func (c *proxyConn) Send(m proto.Message) error {
	if err := c.fakeNativeConn.Send(m); err != nil {
		return err
	}

	switch m.(type) {
	case *pb.DeviceInfoRequest:
		c.handler(&pb.DeviceInfoResponse{Name: "bt-proxy-1", MacAddress: "68:09:47:2E:34:AC", EsphomeVersion: "2026.8.0", BluetoothProxyFeatureFlags: 0xff})
	case *pb.SubscribeBluetoothLEAdvertisementsRequest:
		c.handler(&pb.BluetoothScannerStateResponse{State: pb.BluetoothScannerState_BLUETOOTH_SCANNER_STATE_RUNNING})

		message := &pb.BluetoothLERawAdvertisementsResponse{}

		for _, frame := range c.frames {
			data, _ := hex.DecodeString(frame)
			message.Advertisements = append(message.Advertisements,
				&pb.BluetoothLERawAdvertisement{Address: proxyGoveeAddress, Rssi: -61, Data: append(data, make([]byte, 62-len(data))...)})
		}

		c.handler(message)
	}

	return nil
}

func TestPlugin_AProxyOnTheNativeAPIIsASourceThatSendsWhatTheBluetoothPluginAsksFor(t *testing.T) {
	config := NewPluginConfig()
	config.AddNativeDevice(NativeDeviceConfig{Name: "bt-proxy-1", Address: "10.0.0.18", DisplayName: "Garage Proxy"})

	conns := make(chan *fakeNativeConn, 4)
	p, c := startNativePlugin(t, config, aProxyDial(conns, proxyGoveeFrame, proxyAppleFrame))

	conn := nextConn(t, conns)

	// The device has said it is a proxy, and is registered as a source. Nothing is wanted yet, so nothing is subscribed.
	eventually(t, c, "the proxy to be registered as a source", func() bool {
		for _, r := range c.registrations {
			if r.entityType == bluetooth.ISourceType {
				return true
			}
		}

		return false
	})

	for _, sent := range conn.sentTypes() {
		if sent == "*pb.SubscribeBluetoothLEAdvertisementsRequest" {
			t.Fatal("subscribed with nothing wanted")
		}
	}

	// The bluetooth plugin says what it wants. The plugin hears it where the event manager would tell it.
	c.onPluginGoroutine(func() {})
	c.broadcastMu.Lock()
	predicate, receiver := c.interestPredicate, c.interestReceiver
	c.broadcastMu.Unlock()

	if receiver == nil || !predicate(&events.EventInfo{Event: bluetooth.InterestEvent{}}) {
		t.Fatal("the plugin isn't listening for what is wanted")
	}

	c.onPluginGoroutine(func() {
		_ = receiver(&events.EventInfo{Event: bluetooth.InterestEvent{Owner: "bluetooth", Rules: []bluetooth.Rule{goveeRule()}}})
	})

	// Subscribed; the proxy sends two advertisements, and one is wanted.
	eventually(t, c, "the advertisement to be broadcast", func() bool {
		for _, b := range c.broadcastsSoFar() {
			if a, ok := b.event.(bluetooth.AdvertisementEvent); ok {
				return a.Address == "A4:C1:38:CC:E2:F0" && a.LocalName == "GVH5075_E2F0"
			}
		}

		return false
	})

	adverts := 0

	for _, b := range c.broadcastsSoFar() {
		if _, ok := b.event.(bluetooth.AdvertisementEvent); ok {
			adverts++
		}
	}

	if adverts != 1 {
		t.Errorf("%d advertisements were broadcast", adverts)
	}

	// The source says what it heard, and that the scanner is running, as the proxy reported.
	var source bluetooth.Source

	eventually(t, c, "the scanner state to arrive", func() bool {
		for _, r := range c.registrations {
			if r.entityType == bluetooth.ISourceType {
				stub, _ := r.stubFactory()
				source = stub.(bluetooth.ISource).GetSourceData()
			}
		}

		return source.Scanning == bluetooth.ScanPassive
	})

	if source.Heard != 2 || source.Forwarded != 1 || source.Connectivity != environment.ConnectivityOnline {
		t.Errorf("source %+v", source)
	}

	// The connection drops: the source goes offline.
	conn.end(errors.New("the proxy went away"))

	eventually(t, c, "the source to go offline", func() bool {
		for _, r := range c.registrations {
			if r.entityType == bluetooth.ISourceType {
				stub, _ := r.stubFactory()

				return stub.(bluetooth.ISource).GetSourceData().Connectivity == environment.ConnectivityOffline
			}
		}

		return false
	})

	// And when the plugin stops, the source is deregistered, and it stops listening for what is wanted.
	stopPlugin(t, p, c)

	c.onPluginGoroutine(func() {
		if len(c.deregistered) == 0 || c.interestReceiver != nil {
			t.Errorf("deregistered %v, still listening: %v", c.deregistered, c.interestReceiver != nil)
		}
	})
}

func (f *proxyFixture) sightings() []bluetooth.SightingEvent {
	var result []bluetooth.SightingEvent

	for _, b := range f.container.broadcastsSoFar() {
		if s, ok := b.event.(bluetooth.SightingEvent); ok {
			result = append(result, s)
		}
	}

	return result
}

func TestAdvertisements_ADeviceNobodyWantsIsASightingWhenSightingsAreAskedForAndNothingOtherwise(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProxyFixture(t)
		f.connect("bt-proxy-1")
		f.want(goveeRule())

		// A phone with a rotating address, and the Govee that is wanted.
		f.proxies.advertisements("bt-proxy-1", raw(t, 0x44DCFAB65B00, -70, proxyAppleFrame))

		if len(f.sightings()) != 0 {
			t.Fatal("a sighting nobody asked for")
		}

		event := bluetooth.InterestEvent{Owner: "bluetooth", Rules: []bluetooth.Rule{goveeRule()}, Sightings: time.Minute}
		if err := f.proxies.onInterest(&events.EventInfo{Event: event}); err != nil {
			t.Fatal(err)
		}

		f.proxies.advertisements("bt-proxy-1", raw(t, 0x44DCFAB65B00, -71, proxyAppleFrame))
		f.proxies.advertisements("bt-proxy-1", raw(t, 0x44DCFAB65B00, -72, proxyAppleFrame)) // too soon after
		f.proxies.advertisements("bt-proxy-1", raw(t, proxyGoveeAddress, -61, proxyGoveeFrame))

		sightings := f.sightings()
		if len(sightings) != 1 || sightings[0].Address != "44:DC:FA:B6:5B:00" || sightings[0].RSSI != -71 ||
			sightings[0].Source != f.container.registrations[0].id || !reflect.DeepEqual(sightings[0].ManufacturerIDs, []uint16{0x004c}) {
			t.Fatalf("sightings %+v", sightings)
		}

		// The wanted Govee was forwarded as an advertisement, not as a sighting.
		if adverts := f.adverts(); len(adverts) != 1 || adverts[0].Address != "A4:C1:38:CC:E2:F0" {
			t.Errorf("advertisements %+v", adverts)
		}

		if s := f.source("bt-proxy-1"); s.Heard != 4 || s.Forwarded != 1 || s.Sighted != 1 || s.Dropped != 0 {
			t.Errorf("source %+v", s)
		}

		// Some time later it is told of again.
		time.Sleep(time.Minute)

		if err := f.proxies.onInterest(&events.EventInfo{Event: event}); err != nil {
			t.Fatal(err)
		}

		f.proxies.advertisements("bt-proxy-1", raw(t, 0x44DCFAB65B00, -73, proxyAppleFrame))

		if got := f.sightings(); len(got) != 2 || got[1].RSSI != -73 {
			t.Errorf("sightings %+v", got)
		}
	})
}

func TestInterest_SightingsAloneAreReasonToSubscribe(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")

	if err := f.proxies.onInterest(&events.EventInfo{Event: bluetooth.InterestEvent{Owner: "bluetooth", Sightings: time.Minute}}); err != nil {
		t.Fatal(err)
	}

	if got := f.sent.to("bt-proxy-1"); len(got) != 1 {
		t.Errorf("sent %v", got)
	}
}

func TestAdvertisements_ASightingThatCouldNotBeBroadcastIsCountedAsDropped(t *testing.T) {
	f := newProxyFixture(t)
	f.connect("bt-proxy-1")

	if err := f.proxies.onInterest(&events.EventInfo{Event: bluetooth.InterestEvent{Owner: "bluetooth", Sightings: time.Minute}}); err != nil {
		t.Fatal(err)
	}

	f.container.broadcastErr = context.Canceled
	f.proxies.advertisements("bt-proxy-1", raw(t, 0x44DCFAB65B00, -70, proxyAppleFrame))

	if s := f.source("bt-proxy-1"); s.Heard != 1 || s.Sighted != 0 || s.Dropped != 1 {
		t.Errorf("source %+v", s)
	}
}
