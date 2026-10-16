package nativeapi

import (
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/avanha/pmaas-plugin-esphome/internal/nativeapi/pb"
)

// Every message in api.proto carries its wire id in an option, (id) = N. They're read from the generated
// descriptors rather than listed here, so regenerating the bindings for a newer ESPHome updates this too.
var (
	messageTypesOnce sync.Once
	messageTypeByID  map[uint32]protoreflect.MessageType
	messageIDByName  map[protoreflect.FullName]uint32
)

func loadMessageTypes() {
	messageTypeByID = make(map[uint32]protoreflect.MessageType)
	messageIDByName = make(map[protoreflect.FullName]uint32)

	messages := pb.File_api_proto.Messages()

	for i := 0; i < messages.Len(); i++ {
		descriptor := messages.Get(i)

		options, ok := descriptor.Options().(*descriptorpb.MessageOptions)
		if !ok || options == nil {
			continue
		}

		id, ok := proto.GetExtension(options, pb.E_Id).(uint32)
		if !ok || id == 0 {
			continue // not a message that goes on the wire
		}

		messageType, err := protoregistry.GlobalTypes.FindMessageByName(descriptor.FullName())
		if err != nil {
			panic(fmt.Sprintf("nativeapi: %s has an id but isn't registered: %v", descriptor.FullName(), err))
		}

		if previous, duplicate := messageTypeByID[id]; duplicate {
			panic(fmt.Sprintf("nativeapi: %s and %s both have id %d", previous.Descriptor().FullName(), descriptor.FullName(), id))
		}

		messageTypeByID[id] = messageType
		messageIDByName[descriptor.FullName()] = id
	}
}

// newMessage returns an empty message of the type with this wire id, or false if the id isn't one this build of
// the API knows, which for a device on newer firmware is expected.
func newMessage(id uint32) (proto.Message, bool) {
	messageTypesOnce.Do(loadMessageTypes)

	messageType, ok := messageTypeByID[id]
	if !ok {
		return nil, false
	}

	return messageType.New().Interface(), true
}

// messageID returns the wire id of a message.
func messageID(message proto.Message) (uint32, error) {
	messageTypesOnce.Do(loadMessageTypes)

	id, ok := messageIDByName[message.ProtoReflect().Descriptor().FullName()]
	if !ok {
		return 0, fmt.Errorf("nativeapi: %T has no wire id", message)
	}

	return id, nil
}
