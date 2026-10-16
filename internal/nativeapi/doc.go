// Package nativeapi is a client for the ESPHome native API: the protocol ESPHome devices speak on TCP port 6053,
// which Home Assistant uses to talk to them. Stock firmware, such as a Kauf plug or an Athom relay, has it enabled
// and needs no MQTT broker.
//
// A device is the server and PMAAS connects to it, so there's nothing on the device to configure for it beyond
// what it ships with, and a device is found by its address (or, in another package, by mDNS) rather than by it
// finding us. Messages are protocol buffers, defined by ESPHome's api.proto and generated into package pb (see
// generate.sh), in frames that are either plaintext or Noise-encrypted with a pre-shared key. Encryption is what
// ESPHome offers in place of the API password, which was removed in ESPHome 2026.1.0.
//
// Dial connects, completes the transport handshake and the API hello, and starts a read loop that hands every
// message that isn't connection housekeeping to a handler. What those messages mean, and how a device's entities
// map to PMAAS's, is the caller's business: this package only moves messages. Whatever it doesn't understand is
// skipped rather than treated as an error, since a device running newer firmware than the proto this was
// generated from is normal and mustn't make it unusable.
package nativeapi
