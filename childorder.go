package esphome

import (
	"sort"

	"github.com/avanha/pmaas-plugin-esphome/data"
)

// childPriority says how early each kind of child comes in a device's card: the higher the number, the earlier.
// What's controlled comes before what's measured, so a plug's switch is the first thing on it. A kind that isn't
// here has priority 0, which puts a new kind after everything listed, in the alphabetical order of its kind, until
// someone decides where it belongs; to move one up, raise its number, and to put one below the unlisted, give it a
// negative one. The numbers are spaced out so that something can be slipped in between.
var childPriority = map[string]int{
	data.KindSwitch:         100,
	data.KindPowerMonitor:   90,
	data.KindDoorSensor:     50,
	data.KindThermometer:    40,
	data.KindBluetoothProxy: 30,
}

// sortChildren puts a device's children in order: by the priority of their kind, highest first, then by the name of
// the kind, then by their own sort key. The result is the same however the children came in, which they don't do
// in a fixed order: they come out of maps.
func sortChildren(children []data.Child) {
	sort.SliceStable(children, func(i, j int) bool {
		a, b := children[i], children[j]

		if pa, pb := childPriority[a.Kind()], childPriority[b.Kind()]; pa != pb {
			return pa > pb
		}

		if a.Kind() != b.Kind() {
			return a.Kind() < b.Kind()
		}

		return a.SortKey() < b.SortKey()
	})
}
