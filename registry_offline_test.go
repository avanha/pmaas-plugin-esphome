package esphome

import (
	"testing"
	"time"

	"github.com/avanha/pmaas-spi/alert"
	"github.com/avanha/pmaas-spi/clock"
	"github.com/avanha/pmaas-spi/events"
)

func offlineFixture(t *testing.T, policy alert.OfflineAlert) *fixture {
	t.Helper()

	f := newFixture(t)
	f.registry.offlineAlerts = map[string]alert.OfflineAlert{"garage1": policy}
	f.registry.startOfflineWatches()

	return f
}

func (f *fixture) raised() (raised []alert.RaisedEvent, cleared []alert.ClearedEvent) {
	for _, b := range f.container.broadcasts {
		switch e := b.event.(type) {
		case alert.RaisedEvent:
			raised = append(raised, e)
		case alert.ClearedEvent:
			cleared = append(cleared, e)
		}
	}

	return raised, cleared
}

func TestOfflineAlert_NoSettingNoAlert(t *testing.T) {
	f := newFixture(t)
	f.registry.startOfflineWatches()
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.registry.HandleDisconnected("garage1")
	f.advance(time.Hour)
	f.registry.checkOfflineAlerts()

	if raised, cleared := f.raised(); len(raised)+len(cleared) != 0 {
		t.Errorf("alerts with none asked for: %+v %+v", raised, cleared)
	}
}

func TestOfflineAlert_RaisedAfterItsTimeAndClearedWhenTheDeviceIsBack(t *testing.T) {
	f := offlineFixture(t, alert.WhenOffline(5*time.Minute, alert.Always(alert.SeverityCritical)))
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.advance(time.Hour)
	f.registry.HandleDisconnected("garage1")

	f.advance(4 * time.Minute)
	f.registry.checkOfflineAlerts()

	if raised, _ := f.raised(); len(raised) != 0 {
		t.Fatalf("too early: %+v", raised)
	}

	f.advance(time.Minute)
	f.registry.checkOfflineAlerts()
	f.registry.checkOfflineAlerts()

	raised, _ := f.raised()
	if len(raised) != 1 || raised[0].Source != "esphome" || raised[0].Key != "offline:garage1" ||
		raised[0].Severity != alert.SeverityCritical || raised[0].Title != "Offline: Garage 1" {
		t.Fatalf("raised %+v", raised)
	}

	f.registry.HandleConnected("garage1", "10.0.0.1")

	if _, cleared := f.raised(); len(cleared) != 1 || cleared[0].Key != "offline:garage1" {
		t.Errorf("cleared %+v", cleared)
	}
}

func TestOfflineAlert_ADeviceThatNeverConnectedIsOfflineSinceThePluginStarted(t *testing.T) {
	f := offlineFixture(t, alert.WhenOffline(10*time.Minute, nil))

	f.advance(9 * time.Minute)
	f.registry.checkOfflineAlerts()

	if raised, _ := f.raised(); len(raised) != 0 {
		t.Fatalf("too early: %+v", raised)
	}

	f.advance(time.Minute)
	f.registry.checkOfflineAlerts()

	raised, _ := f.raised()
	if len(raised) != 1 || raised[0].Severity != alert.SeverityWarning {
		t.Errorf("raised %+v", raised)
	}
}

func TestOfflineAlert_ADeviceThatConnectsInTimeNeverAlerts(t *testing.T) {
	f := offlineFixture(t, alert.WhenOffline(10*time.Minute, nil))

	f.advance(2 * time.Minute)
	f.registry.HandleConnected("garage1", "10.0.0.1")
	f.advance(time.Hour)
	f.registry.checkOfflineAlerts()

	if raised, cleared := f.raised(); len(raised)+len(cleared) != 0 {
		t.Errorf("alerts: %+v %+v", raised, cleared)
	}
}

func TestOfflineAlert_NothingAfterShutdown(t *testing.T) {
	f := offlineFixture(t, alert.WhenOffline(0, nil))
	f.container.broadcasts = nil // what the start raised
	f.registry.Shutdown()
	f.registry.checkOfflineAlerts()

	if raised, _ := f.raised(); len(raised) != 0 {
		t.Errorf("raised after shutdown: %+v", raised)
	}
}

func TestPluginConfig_OfflineAlertsAreThoseSetOnMqttAndNativeDevices(t *testing.T) {
	conf := NewPluginConfig()
	conf.AddDevice(DeviceConfig{Name: "a", OfflineAlert: alert.WhenOffline(time.Minute, nil)})
	conf.AddDevice(DeviceConfig{Name: "b"})
	conf.AddNativeDevice(NativeDeviceConfig{Name: "c", Address: "x", OfflineAlert: alert.WhenOffline(0, nil)})

	got := conf.offlineAlerts()
	if len(got) != 2 || !got["a"].Enabled() || !got["c"].Enabled() || got["a"].After() != time.Minute {
		t.Errorf("got %+v", got)
	}
}

func TestOfflineChecks_AnAlertWaitingForItsTimeComesDueOnTheMinuteEventWithNothingElseHappening(t *testing.T) {
	f := newFixture(t)
	p := &plugin{container: f.container, registry: f.registry}
	f.registry.offlineAlerts = map[string]alert.OfflineAlert{"garage1": alert.WhenOffline(5*time.Minute, nil)}

	p.startOfflineChecks()

	if f.container.receiver == nil {
		t.Fatal("not listening for the clock")
	}

	minute := func() {
		f.advance(time.Minute)

		if err := f.container.receiver(&events.EventInfo{Event: clock.MinuteChangedEvent{Time: f.clock}}); err != nil {
			t.Fatal(err)
		}
	}

	for range 4 {
		minute()
	}

	if raised, _ := f.raised(); len(raised) != 0 {
		t.Fatalf("too early: %+v", raised)
	}

	minute()

	if raised, _ := f.raised(); len(raised) != 1 {
		t.Fatalf("raised %+v", raised)
	}

	// Another kind of event is not a reason to look.
	_ = f.container.receiver(&events.EventInfo{Event: "something else"})
}

func TestOfflineChecks_NoClockListenerWithoutAnAlertAskedFor_AndItIsGivenBackOnStop(t *testing.T) {
	f := newFixture(t)
	p := &plugin{container: f.container, registry: f.registry}

	p.startOfflineChecks()

	if f.container.receiver != nil {
		t.Error("listening with nothing to look at")
	}

	f.registry.offlineAlerts = map[string]alert.OfflineAlert{"garage1": alert.WhenOffline(time.Minute, nil)}
	p.startOfflineChecks()

	if f.container.receiver == nil {
		t.Fatal("not listening")
	}

	<-p.Stop()

	if f.container.receiver != nil || len(f.container.receiversDeregistered) != 1 {
		t.Errorf("still listening: %v", f.container.receiversDeregistered)
	}
}
