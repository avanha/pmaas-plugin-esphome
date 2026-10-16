// Package data holds what the plugin's status page shows: a plain description of the plugin, its devices and
// its entities, built on the plugin's goroutine and then handed to the page's templates.
package data

import "time"

// PluginStatus is the status block at the top of the page.
type PluginStatus struct {
	// BrokerRunning is whether the MQTT broker is listening. If it isn't, BrokerProblem says why (no devices
	// are configured, the address is in use, and so on), which is the first thing to look at when no device can
	// connect. ListenAddress is where it's listening, or was meant to.
	BrokerRunning bool
	BrokerProblem string
	ListenAddress string

	// DiscoveryPrefix is the topic prefix devices announce their entities under.
	DiscoveryPrefix string

	// CaptureMessages is whether every message a device sends is being logged, which is only meant for
	// working out what a new kind of device sends.
	CaptureMessages bool

	// Devices has every configured device, whether or not it has ever connected, in the order configured.
	Devices []DeviceStatus

	// ConnectedDevices is how many of Devices are connected right now.
	ConnectedDevices int

	ThermometerCount  int
	DoorSensorCount   int
	SwitchCount       int
	PowerMonitorCount int

	// LastMessageTime is when any device last sent anything. Zero if none has.
	LastMessageTime time.Time
}

// DeviceStatus describes one configured device.
type DeviceStatus struct {
	// Name is the device's ESPHome name, which is also its MQTT username. Label is what to call it: the
	// display name it was configured with, else the name it announces, else Name.
	Name  string
	Label string

	// Connected is whether the device has a session with the broker now. ConnectedSince is when its
	// current session began, and DisconnectedSince is when its last one ended. Either is zero if that
	// hasn't happened.
	Connected         bool
	ConnectedSince    time.Time
	DisconnectedSince time.Time

	// Address is the IP address the device last connected from, as the broker saw it. Empty if it has never
	// connected.
	Address string

	// These come from the device block of what the device announces, so they're empty until it has
	// announced something. Firmware is the ESPHome version (and config hash), and MAC is shown only if the
	// device's identifier is one.
	Firmware     string
	Model        string
	Manufacturer string
	MAC          string

	// EntityCount is how many entities of this device PMAAS is tracking.
	EntityCount int

	// LastMessageTime is when the device last sent anything. Zero if it never has.
	LastMessageTime time.Time
}

// ThermometerView is a thermometer as the page shows it.
type ThermometerView struct {
	Name       string
	DeviceName string

	// DeviceConnected is whether the device it belongs to is connected. A reading from a device that has
	// since dropped off is the last one it took, not the current temperature, and the card says so.
	DeviceConnected bool

	HasData        bool
	Temperature    float32
	HasHumidity    bool
	Humidity       float32
	LastUpdateTime time.Time
}

// DoorSensorView is a door position sensor as the page shows it.
type DoorSensorView struct {
	Name       string
	DeviceName string

	DeviceConnected bool

	// HasData is whether a position has been reported at all. Position is "Open", "Closed" or "Unknown",
	// and PositionClass the same in lower case, which is also the CSS class that colours it.
	HasData       bool
	Position      string
	PositionClass string

	// LastChangeTime is when the position last changed, which is zero until a change has been seen (the
	// first report establishes the position without being a change). LastUpdateTime is when it was last
	// reported, changed or not.
	LastChangeTime time.Time
	LastUpdateTime time.Time

	OfflineSince time.Time
	OnlineSince  time.Time
}

// SwitchView is a switch (a relay, a smart plug) as the page shows it.
type SwitchView struct {
	Name       string
	DeviceName string

	// ObjectID is what the device calls the switch, which with DeviceName is how the page says which one to
	// turn on or off.
	ObjectID string

	DeviceConnected bool

	// CanControl is whether the page should offer to turn it on or off: the device is connected, and has said
	// where to send the command.
	CanControl bool

	// HasData is whether a state has been reported at all. State is "On", "Off" or "Unknown", and StateClass
	// the same in lower case, which is also the CSS class that colours it.
	HasData    bool
	State      string
	StateClass string

	// LastChangeTime is when the state last changed, which is zero until a change has been seen (the first
	// report establishes the state without being a change). LastUpdateTime is when it was last reported,
	// changed or not.
	LastChangeTime time.Time
	LastUpdateTime time.Time

	OfflineSince time.Time
	OnlineSince  time.Time
}

// PowerView is a device's power monitor as the page shows it: whichever of the readings the device has.
type PowerView struct {
	Name       string
	DeviceName string

	DeviceConnected bool

	// HasData is whether any reading has been reported. Each reading is only meaningful if its Has flag is
	// set: a device without a voltage sensor has no voltage, which isn't zero volts.
	HasData    bool
	HasPower   bool
	Power      float64 // watts
	HasVoltage bool
	Voltage    float64 // volts
	HasCurrent bool
	Current    float64 // amps
	HasEnergy  bool
	Energy     float64 // kilowatt-hours

	LastUpdateTime time.Time

	OfflineSince time.Time
	OnlineSince  time.Time
}
