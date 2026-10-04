package broker

import (
	"strings"

	"github.com/avanha/pmaas-plugin-esphome/internal/hadiscovery"
)

// allowed reports whether the device named device may publish to (write) or subscribe to (!write)
// topic, which for a subscription is the topic filter.
//
// A device owns the topic tree named after it - ESPHome's default topic_prefix is its node name, so
// its state, availability and command topics all live under "<device>/" - and its own discovery
// config topics, where the node segment is its name. Nothing else is allowed: no other device's
// topics, no other discovery configs, and no wildcards reaching outside its own tree (a filter must
// itself start with "<device>/" to be allowed).
func allowed(device string, discoveryPrefix string, topic string, write bool) bool {
	if device == "" {
		return false
	}

	if strings.HasPrefix(topic, device+"/") {
		return true
	}

	if !write {
		return false
	}

	key, ok := hadiscovery.ParseTopic(discoveryPrefix, topic)

	return ok && key.NodeID == device
}

// validDeviceName reports whether name is usable as both an MQTT username and the first topic
// segment of everything the device owns.
func validDeviceName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/+#$ \t\r\n")
}
