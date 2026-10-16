package nativeapi

import (
	"fmt"
	"testing"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

// The ids are part of the protocol, fixed by ESPHome: these are the ones everything here depends on, so a
// regeneration that moved one would be caught.
func TestMessageIDs_TheOnesTheProtocolDependsOn(t *testing.T) {
	for id, want := range map[uint32]any{
		1:  &pb.HelloRequest{},
		2:  &pb.HelloResponse{},
		5:  &pb.DisconnectRequest{},
		6:  &pb.DisconnectResponse{},
		7:  &pb.PingRequest{},
		8:  &pb.PingResponse{},
		9:  &pb.DeviceInfoRequest{},
		10: &pb.DeviceInfoResponse{},
		11: &pb.ListEntitiesRequest{},
		19: &pb.ListEntitiesDoneResponse{},
		20: &pb.SubscribeStatesRequest{},
		36: &pb.GetTimeRequest{},
		37: &pb.GetTimeResponse{},
	} {
		got, ok := newMessage(id)
		if !ok {
			t.Errorf("id %d is unknown", id)
			continue
		}

		if gotID, err := messageID(got); err != nil || gotID != id {
			t.Errorf("id %d: round trip gave %d, %v", id, gotID, err)
		}

		if typeName(got) != typeName(want) {
			t.Errorf("id %d is %s, want %s", id, typeName(got), typeName(want))
		}
	}
}

func typeName(message any) string {
	return fmt.Sprintf("%T", message)
}

func TestMessageIDs_UnknownIsReportedNotFatal(t *testing.T) {
	if message, ok := newMessage(60000); ok || message != nil {
		t.Fatalf("got %v, %v", message, ok)
	}
}

func TestMessageIDs_EveryMessageWithAnIDIsReachableBothWays(t *testing.T) {
	messageTypesOnce.Do(loadMessageTypes)

	if len(messageTypeByID) < 100 {
		t.Fatalf("only %d messages have ids, which can't be right", len(messageTypeByID))
	}

	for id, messageType := range messageTypeByID {
		got, err := messageID(messageType.New().Interface())
		if err != nil || got != id {
			t.Errorf("%s: id %d, round trip %d, %v", messageType.Descriptor().FullName(), id, got, err)
		}
	}
}
