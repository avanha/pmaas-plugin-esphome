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

	// BrokerNotUsed is that the broker isn't running because no MQTT device is configured for it to serve, which
	// isn't a problem: every device is reached over the native API.
	BrokerNotUsed bool

	// DiscoveryPrefix is the topic prefix devices announce their entities under.
	DiscoveryPrefix string

	// CaptureMessages is whether every message a device sends is being logged, which is only meant for
	// working out what a new kind of device sends.
	CaptureMessages bool

	// DeviceCount is how many devices are configured, whether or not any has ever connected, and
	// ConnectedDevices is how many of them are connected right now.
	DeviceCount      int
	ConnectedDevices int

	ThermometerCount  int
	DoorSensorCount   int
	SwitchCount       int
	PowerMonitorCount int

	// LastMessageTime is when any device last sent anything. Zero if none has.
	LastMessageTime time.Time
}

// Kinds of child a device has, which is how the page tells them apart, picks the template for each and orders them
// (see the child priorities in the plugin).
const (
	KindSwitch       = "switch"
	KindPowerMonitor = "power"
	KindDoorSensor   = "door"
	KindThermometer  = "thermometer"
)

// Child is something a device has that the page shows inside the device's card: a switch, a power monitor, a door
// sensor, a thermometer. Each is a registered entity in its own right, as a network interface is in netmon, and the
// device is what groups them for display.
type Child interface {
	// Kind is what sort of child it is: one of the Kind constants.
	Kind() string

	// SortKey orders children of the same kind, and must be different for each: it's the name and what the device
	// calls the entity, since two can have the same name and the order mustn't change from one refresh to the next.
	SortKey() string
}

// DeviceView is a device as the page shows it: its status, and everything of its that's tracked. The children are
// ordered, with the more important kinds first.
type DeviceView struct {
	DeviceStatus

	Children []Child
}

// DeviceStatus describes one configured device.
type DeviceStatus struct {
	// Name is the device's ESPHome name, which is also its MQTT username. Label is what to call it: the
	// display name it was configured with, else the name it announces, else Name.
	Name  string
	Label string

	// Transport is how PMAAS reaches the device: "MQTT" or "Native API".
	Transport string

	// Problem is what's stopping a native API device from connecting, if that's known: the last attempt's
	// error, in terms of what to do about it. Empty for a device that's connected, and for an MQTT device, which
	// connects to PMAAS rather than the other way round, so there's nothing PMAAS could have got wrong.
	Problem string

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

	// LastMessageTime is when the device last sent anything. Zero if it never has. For a Bluetooth proxy that includes its
	// batches of advertisements, so it is when it last heard anything.
	LastMessageTime time.Time

	// Bluetooth is what the device has done as a Bluetooth proxy, or nil if it is not one.
	Bluetooth *BluetoothStatus
}

// BluetoothStatus is a Bluetooth proxy's activity since PMAAS started.
type BluetoothStatus struct {
	// Scanning is "off", "passive" or "active".
	Scanning string

	// Batches is how many messages of advertisements it has sent and LastBatchTime when the last came. Heard is the
	// advertisements in them, Forwarded those the Bluetooth plugin wanted, Sighted the devices told of in sightings, and
	// Dropped those that could not be decoded or broadcast.
	Batches       uint64
	LastBatchTime time.Time
	Heard         uint64
	Forwarded     uint64
	Sighted       uint64
	Dropped       uint64
}

// ThermometerView is a thermometer as the page shows it.
type ThermometerView struct {
	// Name is the entity's full name, as it's registered: the device's, then its own. ShortName is its own alone,
	// for where the device is already on show, as it is for everything inside a device's card.
	Name       string
	ShortName  string
	DeviceName string

	// ObjectID is what the device calls the temperature sensor.
	ObjectID string

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
	ShortName  string
	DeviceName string

	// ObjectID is what the device calls the binary sensor.
	ObjectID string

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
	ShortName  string
	DeviceName string

	// ObjectID is what the device calls the switch, which with DeviceName is how the page says which one to
	// turn on or off.
	ObjectID string

	DeviceConnected bool

	// CanControl is whether the page should offer to turn it on or off: the device is connected, has said where to
	// send the command, and its switches aren't read-only. Control is how they're guarded: "open", "confirm",
	// "password" or "readonly", which is what the page asks of a person before it sends the command. The password
	// itself never leaves the server.
	CanControl bool
	Control    string

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
	ShortName  string
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

func (ThermometerView) Kind() string { return KindThermometer }
func (DoorSensorView) Kind() string  { return KindDoorSensor }
func (SwitchView) Kind() string      { return KindSwitch }
func (PowerView) Kind() string       { return KindPowerMonitor }

func (v ThermometerView) SortKey() string { return v.Name + "\x00" + v.ObjectID }
func (v DoorSensorView) SortKey() string  { return v.Name + "\x00" + v.ObjectID }
func (v SwitchView) SortKey() string      { return v.Name + "\x00" + v.ObjectID }

// SortKey of a PowerView is its name alone, since a device has one power monitor.
func (v PowerView) SortKey() string { return v.Name }
