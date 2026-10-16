package esphome

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/avanha/pmaas-plugin-esphome/data"
)

// fakeChild is a kind of child nothing has heard of, as one introduced later would be.
type fakeChild struct {
	kind string
	key  string
}

func (c fakeChild) Kind() string    { return c.kind }
func (c fakeChild) SortKey() string { return c.key }

func order(children []data.Child) string {
	parts := make([]string, len(children))

	for i, c := range children {
		parts[i] = c.Kind() + ":" + c.SortKey()
	}

	return strings.Join(parts, " ")
}

// withPriorities runs a test with a different table, and restores the real one after.
func withPriorities(t *testing.T, priorities map[string]int) {
	t.Helper()

	original := childPriority
	childPriority = priorities

	t.Cleanup(func() { childPriority = original })
}

func TestSortChildren_TheKindsThereAreSwitchesFirst(t *testing.T) {
	children := []data.Child{
		data.ThermometerView{Name: "T"},
		data.DoorSensorView{Name: "D"},
		data.PowerView{Name: "P"},
		data.SwitchView{Name: "S"},
	}

	sortChildren(children)

	var kinds []string
	for _, c := range children {
		kinds = append(kinds, c.Kind())
	}

	if got := strings.Join(kinds, ","); got != "switch,power,door,thermometer" {
		t.Fatalf("got %s", got)
	}
}

func TestSortChildren_WithinAKindByTheSortKeyAndThatKeepsSameNamedOnesApart(t *testing.T) {
	children := []data.Child{
		data.SwitchView{Name: "Relay", ObjectID: "b"},
		data.SwitchView{Name: "Relay", ObjectID: "a"},
		data.SwitchView{Name: "Lamp", ObjectID: "z"},
	}

	sortChildren(children)

	if got := order(children); got != "switch:Lamp\x00z switch:Relay\x00a switch:Relay\x00b" {
		t.Fatalf("got %q", got)
	}
}

// A kind nobody has listed sorts after everything that is, and among the unlisted by the name of the kind.
func TestSortChildren_AKindThatIsNotListedGoesAfterTheListedOnes(t *testing.T) {
	withPriorities(t, map[string]int{"switch": 100, "power": 90})

	children := []data.Child{
		fakeChild{"zeta", "1"}, fakeChild{"power", "1"}, fakeChild{"alpha", "1"}, fakeChild{"switch", "1"}, fakeChild{"alpha", "0"},
	}

	sortChildren(children)

	if got := order(children); got != "switch:1 power:1 alpha:0 alpha:1 zeta:1" {
		t.Fatalf("got %q", got)
	}
}

// To move a kind up, raise its number: no code changes anywhere else.
func TestSortChildren_RaisingAPriorityMovesAKindUp(t *testing.T) {
	withPriorities(t, map[string]int{"switch": 100, "power": 90, "thermometer": 40})

	children := []data.Child{fakeChild{"switch", "1"}, fakeChild{"power", "1"}, fakeChild{"thermometer", "1"}}

	sortChildren(children)

	if got := order(children); got != "switch:1 power:1 thermometer:1" {
		t.Fatalf("got %q", got)
	}

	childPriority["thermometer"] = 95
	sortChildren(children)

	if got := order(children); got != "switch:1 thermometer:1 power:1" {
		t.Fatalf("after boosting the thermometer: got %q", got)
	}
}

// And a negative one puts a kind below the ones that aren't listed.
func TestSortChildren_ANegativePriorityGoesBelowTheUnlisted(t *testing.T) {
	withPriorities(t, map[string]int{"switch": 100, "diagnostic": -10})

	children := []data.Child{fakeChild{"diagnostic", "1"}, fakeChild{"mystery", "1"}, fakeChild{"switch", "1"}}

	sortChildren(children)

	if got := order(children); got != "switch:1 mystery:1 diagnostic:1" {
		t.Fatalf("got %q", got)
	}
}

// Kinds with the same priority are told apart by the name of the kind, not left to chance.
func TestSortChildren_EqualPrioritiesFallBackToTheNameOfTheKind(t *testing.T) {
	withPriorities(t, map[string]int{"b": 50, "a": 50, "c": 50, "high": 60})

	children := []data.Child{fakeChild{"c", "1"}, fakeChild{"b", "1"}, fakeChild{"high", "1"}, fakeChild{"a", "1"}}

	sortChildren(children)

	if got := order(children); got != "high:1 a:1 b:1 c:1" {
		t.Fatalf("got %q", got)
	}
}

// The children come out of maps, so they arrive in a different order each time, and the card mustn't change.
func TestSortChildren_TheResultDoesNotDependOnTheOrderTheyArriveIn(t *testing.T) {
	template := []data.Child{
		data.SwitchView{Name: "Relay", ObjectID: "relay"}, data.SwitchView{Name: "LED", ObjectID: "led"},
		data.PowerView{Name: "Power"}, data.DoorSensorView{Name: "Door", ObjectID: "door"},
		data.ThermometerView{Name: "A", ObjectID: "a"}, data.ThermometerView{Name: "A", ObjectID: "b"},
		fakeChild{"new", "x"},
	}

	want := ""

	for i := 0; i < 50; i++ {
		children := append([]data.Child(nil), template...)
		rand.Shuffle(len(children), func(a, b int) { children[a], children[b] = children[b], children[a] })

		sortChildren(children)

		if got := order(children); want == "" {
			want = got
		} else if got != want {
			t.Fatalf("round %d: got %q, want %q", i, got, want)
		}
	}
}

func TestChildPriority_TheRealTableLeavesRoomToSlotAKindBetween(t *testing.T) {
	seen := map[int]string{}

	for kind, priority := range childPriority {
		if other, clash := seen[priority]; clash {
			t.Errorf("%s and %s have the same priority %d", kind, other, priority)
		}

		seen[priority] = kind
	}

	for _, kind := range []string{data.KindSwitch, data.KindPowerMonitor, data.KindDoorSensor, data.KindThermometer} {
		if _, ok := childPriority[kind]; !ok {
			t.Errorf("%s isn't in the table", kind)
		}
	}
}

func TestDeviceView_CollectsWhatTheDeviceHasInOrder(t *testing.T) {
	f := newFixture(t)
	f.registry.HandleConnected("plug1", "10.0.0.7")
	f.discover("plug1", "switch", "relay", switchConfig("plug1", "relay", "Relay"))
	f.discover("plug1", "switch", "fan", switchConfig("plug1", "fan", "Fan"))
	f.powerSensor("plug1", "power", "power", "W")
	f.discover("plug1", "sensor", "temp", `{"name":"Temp","dev_cla":"temperature","stat_t":"plug1/sensor/temp/state"}`)
	f.discover("plug1", "binary_sensor", "door", `{"name":"Door","dev_cla":"door","stat_t":"plug1/binary_sensor/door/state"}`)
	f.discover("plug2", "switch", "other", switchConfig("plug2", "other", "Other"))

	view := f.registry.deviceView("plug1")

	if got := fmt.Sprint(kinds(view)); got != "switch,switch,power,door,thermometer" {
		t.Fatalf("got %s", got)
	}

	// Only its own: nothing of plug2's, and the switches by name.
	if first, second := view.Children[0].(data.SwitchView), view.Children[1].(data.SwitchView); first.DeviceName != "plug1" ||
		second.DeviceName != "plug1" || first.ObjectID != "fan" || second.ObjectID != "relay" {
		t.Errorf("unexpected switches %+v %+v", first, second)
	}

	if !view.Connected || view.Name != "plug1" {
		t.Errorf("unexpected status %+v", view.DeviceStatus)
	}
}

func TestDeviceView_ADeviceThatHasNeverConnectedHasNoChildren(t *testing.T) {
	f := newFixture(t)

	view := f.registry.deviceView("nobody")
	if len(view.Children) != 0 || view.Connected || view.Name != "nobody" {
		t.Fatalf("unexpected view %+v", view)
	}
}
