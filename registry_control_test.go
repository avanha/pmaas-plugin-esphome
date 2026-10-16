package esphome

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-esphome/internal/common"
	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
	"github.com/avanha/pmaas-spi/device"
)

// guardedPlug is a plug connected over the native API, guarded as control says, with everything sent to it recorded.
func guardedPlug(t *testing.T, control SwitchControl) (*fixture, *[]nativeSend) {
	t.Helper()

	f := newFixture(t)
	f.registry.displayNames = map[string]string{}
	f.registry.control = map[string]SwitchControl{"plug1": control}
	sent := f.recordNativeSends(nil)
	f.nativeKaufPlug("plug1")
	f.native("plug1", &pb.SwitchStateResponse{Key: 11, State: true})

	return f, sent
}

func fromThePage(f *fixture, state device.SwitchState, c common.Confirmation) error {
	return f.registry.setSwitch("plug1", "relay", state, c)
}

func TestControl_OpenNeedsNothing(t *testing.T) {
	f, sent := guardedPlug(t, SwitchControl{})

	if err := fromThePage(f, device.SwitchStateOff, common.Confirmation{}); err != nil || len(*sent) != 1 {
		t.Fatalf("got %v, sent %d", err, len(*sent))
	}
}

func TestControl_ConfirmNeedsYes(t *testing.T) {
	f, sent := guardedPlug(t, SwitchControl{Level: ControlConfirm})

	err := fromThePage(f, device.SwitchStateOff, common.Confirmation{})
	if !errors.Is(err, errConfirmationRequired) || len(*sent) != 0 {
		t.Fatalf("got %v, sent %d", err, len(*sent))
	}

	// A password means nothing to it.
	if err := fromThePage(f, device.SwitchStateOff, common.Confirmation{Password: "anything"}); !errors.Is(err, errConfirmationRequired) {
		t.Fatalf("got %v", err)
	}

	if err := fromThePage(f, device.SwitchStateOff, common.Confirmation{Confirmed: true}); err != nil || len(*sent) != 1 {
		t.Fatalf("got %v, sent %d", err, len(*sent))
	}
}

func TestControl_PasswordNeedsTheRightOne(t *testing.T) {
	f, sent := guardedPlug(t, SwitchControl{Level: ControlPassword, Password: "hovno"})

	for name, c := range map[string]common.Confirmation{
		"nothing":                  {},
		"only saying yes":          {Confirmed: true},
		"the wrong password":       {Confirmed: true, Password: "wrong"},
		"the password in capitals": {Confirmed: true, Password: "HOVNO"},
		"a prefix":                 {Confirmed: true, Password: "hovn"},
		"a longer one":             {Confirmed: true, Password: "hovno "},
		"nothing but a password":   {Password: "wrong"},
	} {
		if err := fromThePage(f, device.SwitchStateOff, c); !errors.Is(err, errWrongPassword) {
			t.Errorf("%s: got %v", name, err)
		}

		// Spaced out, so that these don't lock it: this is about what's accepted.
		f.advance(time.Minute)
	}

	if len(*sent) != 0 {
		t.Fatalf("something was sent without the password: %+v", *sent)
	}

	// The right one, with or without saying yes as well: the password is the confirmation.
	for _, c := range []common.Confirmation{{Password: "hovno"}, {Confirmed: true, Password: "hovno"}} {
		if err := fromThePage(f, device.SwitchStateOff, c); err != nil {
			t.Fatalf("got %v", err)
		}
	}

	if len(*sent) != 2 {
		t.Fatalf("sent %d", len(*sent))
	}
}

func TestControl_AnEmptyPasswordIsNeverTheRightOne(t *testing.T) {
	f, sent := guardedPlug(t, SwitchControl{Level: ControlPassword, Password: "hovno"})

	if err := fromThePage(f, device.SwitchStateOff, common.Confirmation{Password: ""}); !errors.Is(err, errWrongPassword) || len(*sent) != 0 {
		t.Fatalf("got %v, sent %d", err, len(*sent))
	}
}

func TestControl_WrongPasswordsLockTheDeviceForAWhile(t *testing.T) {
	f, sent := guardedPlug(t, SwitchControl{Level: ControlPassword, Password: "hovno"})
	wrong, right := common.Confirmation{Password: "guess"}, common.Confirmation{Password: "hovno"}

	for i := 0; i < maxPasswordFailures; i++ {
		if err := fromThePage(f, device.SwitchStateOff, wrong); !errors.Is(err, errWrongPassword) {
			t.Fatalf("try %d: got %v", i+1, err)
		}
	}

	// Locked: even the right password isn't taken, or it would only be a matter of trying.
	err := fromThePage(f, device.SwitchStateOff, right)
	if err == nil || !strings.Contains(err.Error(), "too many wrong passwords") || len(*sent) != 0 {
		t.Fatalf("got %v, sent %d", err, len(*sent))
	}

	f.advance(passwordLockout - time.Second)

	if err := fromThePage(f, device.SwitchStateOff, right); err == nil || !strings.Contains(err.Error(), "try again in 1 seconds") {
		t.Fatalf("still locked, and it should say for how long: %v", err)
	}

	f.advance(2 * time.Second)

	if err := fromThePage(f, device.SwitchStateOff, right); err != nil || len(*sent) != 1 {
		t.Fatalf("after the lockout: got %v, sent %d", err, len(*sent))
	}
}

func TestControl_TheLockoutIsPerDeviceAndNotPerSwitch(t *testing.T) {
	f := newFixture(t)
	f.registry.control = map[string]SwitchControl{
		"plug1": {Level: ControlPassword, Password: "hovno"}, "plug2": {Level: ControlPassword, Password: "hovno"},
	}
	f.recordNativeSends(nil)
	f.registry.HandleConnected("plug1", "10.0.0.1")
	f.registry.HandleConnected("plug2", "10.0.0.2")

	for _, name := range []string{"plug1", "plug2"} {
		f.native(name,
			&pb.ListEntitiesSwitchResponse{ObjectId: "a", Key: 1, Name: "A"},
			&pb.ListEntitiesSwitchResponse{ObjectId: "b", Key: 2, Name: "B"},
			&pb.ListEntitiesDoneResponse{})
	}

	// Tries spread over both of a device's switches count together.
	for i := 0; i < maxPasswordFailures; i++ {
		object := []string{"a", "b"}[i%2]
		_ = f.registry.setSwitch("plug1", object, device.SwitchStateOn, common.Confirmation{Password: "guess"})
	}

	for _, object := range []string{"a", "b"} {
		err := f.registry.setSwitch("plug1", object, device.SwitchStateOn, common.Confirmation{Password: "hovno"})
		if err == nil || !strings.Contains(err.Error(), "too many wrong passwords") {
			t.Fatalf("%s: got %v", object, err)
		}
	}

	// Another device is another matter.
	if err := f.registry.setSwitch("plug2", "a", device.SwitchStateOn, common.Confirmation{Password: "hovno"}); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestControl_ARightPasswordStartsTheCountAgain(t *testing.T) {
	f, _ := guardedPlug(t, SwitchControl{Level: ControlPassword, Password: "hovno"})

	// Nearly locked, then right, then nearly locked again: never locked.
	for round := 0; round < 3; round++ {
		for i := 0; i < maxPasswordFailures-1; i++ {
			_ = fromThePage(f, device.SwitchStateOff, common.Confirmation{Password: "guess"})
		}

		if err := fromThePage(f, device.SwitchStateOff, common.Confirmation{Password: "hovno"}); err != nil {
			t.Fatalf("round %d: got %v", round, err)
		}
	}
}

func TestControl_TheLockoutEndsAndThenTakesAFullSetOfWrongOnesToStartAgain(t *testing.T) {
	f, _ := guardedPlug(t, SwitchControl{Level: ControlPassword, Password: "hovno"})
	wrong := common.Confirmation{Password: "guess"}

	for i := 0; i < maxPasswordFailures; i++ {
		_ = fromThePage(f, device.SwitchStateOff, wrong)
	}

	f.advance(passwordLockout + time.Second)

	// Not locked again by one wrong one.
	if err := fromThePage(f, device.SwitchStateOff, wrong); !errors.Is(err, errWrongPassword) {
		t.Fatalf("got %v", err)
	}

	if err := fromThePage(f, device.SwitchStateOff, common.Confirmation{Password: "hovno"}); err != nil {
		t.Fatalf("got %v", err)
	}
}

// What's typed, and what the password is, appear nowhere: not in the log, an error, or what the page is given.
func TestControl_ThePasswordIsNeverLoggedOrShown(t *testing.T) {
	f, _ := guardedPlug(t, SwitchControl{Level: ControlPassword, Password: "hovno"})

	var texts []string

	for i := 0; i < maxPasswordFailures+1; i++ {
		if err := fromThePage(f, device.SwitchStateOff, common.Confirmation{Password: "typed-secret"}); err != nil {
			texts = append(texts, err.Error())
		}
	}

	texts = append(texts, f.logs...)

	view, err := json.Marshal(f.registry.deviceView("plug1"))
	if err != nil {
		t.Fatal(err)
	}

	texts = append(texts, string(view))

	for _, text := range texts {
		for _, secret := range []string{"typed-secret", "hovno"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q gives away %q", text, secret)
			}
		}
	}

	if len(f.logs) < 2 {
		t.Fatalf("wrong passwords should be logged, as that they happened: %v", f.logs)
	}
}

func TestControl_ReadOnlyIsReadOnlyForEveryone(t *testing.T) {
	f, sent := guardedPlug(t, SwitchControl{Level: ControlReadOnly})

	// A person at the page, however they ask.
	for _, c := range []common.Confirmation{{}, {Confirmed: true}, {Confirmed: true, Password: "anything"}} {
		if err := fromThePage(f, device.SwitchStateOff, c); !errors.Is(err, errReadOnly) {
			t.Errorf("%+v: got %v", c, err)
		}
	}

	// Another plugin, through the switch's own interface.
	err := f.switchStub().SetState(device.SwitchStateOff)
	if !errors.Is(err, errReadOnly) {
		t.Fatalf("got %v", err)
	}

	if len(*sent) != 0 {
		t.Fatalf("something was sent to a read-only switch: %+v", *sent)
	}

	// It still reports what the device says.
	if got := f.switchStub().GetSwitchData().State; got != device.SwitchStateOn {
		t.Fatalf("unexpected state %v", got)
	}

	f.native("plug1", &pb.SwitchStateResponse{Key: 11, State: false})

	if got := f.switchStub().GetSwitchData().State; got != device.SwitchStateOff {
		t.Fatalf("a read-only switch must still follow the device: %v", got)
	}
}

// Confirming and the password are for a person at the page. Another plugin has nobody to ask, so they don't apply:
// only read-only stops it.
func TestControl_ConfirmAndPasswordAreForThePageNotForOtherPlugins(t *testing.T) {
	for _, control := range []SwitchControl{{Level: ControlConfirm}, {Level: ControlPassword, Password: "hovno"}} {
		f, sent := guardedPlug(t, control)

		if err := f.switchStub().SetState(device.SwitchStateOff); err != nil || len(*sent) != 1 {
			t.Errorf("%v: got %v, sent %d", control.Level, err, len(*sent))
		}
	}
}

func TestControl_OnlyTheGuardedDeviceIsGuarded(t *testing.T) {
	f := newFixture(t)
	f.registry.control = map[string]SwitchControl{"rack": {Level: ControlReadOnly}}
	f.recordNativeSends(nil)
	f.registry.HandleConnected("lamp", "10.0.0.2")
	f.native("lamp", &pb.ListEntitiesSwitchResponse{ObjectId: "relay", Key: 1, Name: "Relay"}, &pb.ListEntitiesDoneResponse{})

	if err := f.registry.setSwitch("lamp", "relay", device.SwitchStateOn, common.Confirmation{}); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestControl_TheViewSaysHowAndWhetherTheSwitchCanBeChanged(t *testing.T) {
	for _, c := range []struct {
		control    SwitchControl
		want       string
		canControl bool
	}{
		{SwitchControl{}, "open", true},
		{SwitchControl{Level: ControlConfirm}, "confirm", true},
		{SwitchControl{Level: ControlPassword, Password: "p"}, "password", true},
		{SwitchControl{Level: ControlReadOnly}, "readonly", false},
	} {
		f, _ := guardedPlug(t, c.control)

		view := f.registry.switchViews()[0]
		if view.Control != c.want || view.CanControl != c.canControl {
			t.Errorf("%s: control=%q canControl=%v", c.want, view.Control, view.CanControl)
		}
	}
}

func TestControl_AnOfflineDeviceIsStillNotControllable(t *testing.T) {
	f, _ := guardedPlug(t, SwitchControl{Level: ControlConfirm})
	f.registry.HandleDisconnected("plug1")

	if view := f.registry.switchViews()[0]; view.CanControl || view.Control != "confirm" {
		t.Fatalf("unexpected view %+v", view)
	}
}

func TestControl_AdapterPassesWhatThePersonDidThrough(t *testing.T) {
	config := NewPluginConfig()
	config.ListenAddress = freeAddress(t)
	config.AddDevice(DeviceConfig{Name: "plug1", Password: "secret1"})
	config.Devices[0].Control = SwitchControl{Level: ControlPassword, Password: "hovno"}

	p, c := startPlugin(t, config)
	adapter := &entityStoreAdapter{parent: p}

	var sent []published

	c.onPluginGoroutine(func() {
		p.registry.publish = func(topic string, payload []byte) error {
			sent = append(sent, published{topic, string(payload)})
			return nil
		}

		p.registry.HandleConnected("plug1", "10.0.0.5")
		p.registry.HandleMessage("plug1", "homeassistant/switch/plug1/relay/config", []byte(switchConfig("plug1", "relay", "Relay")))
	})

	if err := adapter.SetSwitch("plug1", "relay", true, common.Confirmation{Password: "wrong"}); !errors.Is(err, errWrongPassword) {
		t.Fatalf("got %v", err)
	}

	if err := adapter.SetSwitch("plug1", "relay", true, common.Confirmation{Password: "hovno"}); err != nil {
		t.Fatalf("got %v", err)
	}

	c.onPluginGoroutine(func() {
		if len(sent) != 1 || sent[0].payload != "ON" {
			t.Errorf("sent %+v", sent)
		}
	})

	var done chan func()
	c.onPluginGoroutine(func() { done = p.Stop() })
	<-done
}
