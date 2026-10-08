package broker

import (
	"strings"

	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
)

// ESPHome's own node discovery, on by default (the mqtt component's discover_ip option), lets tools find a
// node's address over MQTT. Its topics are shared by every ESPHome device on a broker, unlike everything a
// device otherwise uses, and PMAAS has no use for it. A device left at its defaults does it on every connect,
// and a refused QoS 1 publish disconnects it, so rather than refuse it the broker takes it in and throws it
// away: see isNodeDiscoveryPublish and hook.OnPublish.
const (
	nodeDiscoveryTopic       = "esphome/discover"
	nodeDiscoveryFilter      = "esphome/discover/#"
	nodeDiscoveryPingPrefix  = "esphome/ping/"
	nodeDiscoveryAnswerTopic = "esphome/discover/"
)

// isNodeDiscoveryPublish reports whether topic is one ESPHome's node discovery publishes to on behalf of
// device: the discover topic itself, or the device's own answer topic. Nothing published to these is
// delivered to anyone, so that devices can't see each other through them.
func isNodeDiscoveryPublish(device string, topic string) bool {
	return device != "" && (topic == nodeDiscoveryTopic || topic == nodeDiscoveryAnswerTopic+device)
}

// isNodeDiscoverySubscription reports whether filter is one ESPHome's node discovery subscribes device to.
// Granting it costs nothing: since everything published to the discovery topics is thrown away, nothing
// ever arrives on it.
func isNodeDiscoverySubscription(device string, filter string) bool {
	return device != "" &&
		(filter == nodeDiscoveryTopic || filter == nodeDiscoveryFilter || filter == nodeDiscoveryPingPrefix+device)
}

// allowed reports whether the device named device may publish to (write) or subscribe to (!write)
// topic, which for a subscription is the topic filter.
//
// A device owns the topic tree named after it - ESPHome's default topic_prefix is its node name, so
// its state, availability and command topics all live under "<device>/" - and its own discovery
// config topics, where the node segment is its name. The one exception is ESPHome's node discovery,
// described above, which is allowed exactly as ESPHome uses it. Nothing else is allowed: no other
// device's topics, no other discovery configs, and no wildcards reaching outside its own tree (a
// filter must itself start with "<device>/" to be allowed).
func allowed(device string, discoveryPrefix string, topic string, write bool) bool {
	if device == "" {
		return false
	}

	if strings.HasPrefix(topic, device+"/") {
		return true
	}

	if !write {
		return isNodeDiscoverySubscription(device, topic)
	}

	if isNodeDiscoveryPublish(device, topic) {
		return true
	}

	key, ok := hadiscovery.ParseTopic(discoveryPrefix, topic)

	return ok && key.NodeID == device
}

// validDeviceName reports whether name is usable as both an MQTT username and the first topic
// segment of everything the device owns.
func validDeviceName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/+#$ \t\r\n")
}
