package protogen

import (
	"os"
	"sort"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"gotest.tools/v3/assert"
)

// TestWireContractIsUnchanged locks the extension runtime service to the wire
// format protoc compiled from the hand-written runtime.proto, captured in
// testdata before the contract moved to a Go-first definition.
//
// This is the one contract in the framework that cannot be versioned away: an
// extension binary is built separately, possibly from an older tree, and the
// first thing it does is speak this service over its stdio handshake. A change
// here does not fail a build, it breaks every already-deployed extension at
// startup. So the check is against a golden descriptor rather than against
// whatever the generator currently emits.
//
// It compares descriptors, not generated Go: the Go legitimately differs from
// what protoc produced -- different message order, different package, and doc
// comments that now live on the contract types rather than being copied out of
// the .proto. None of that is on the wire. Everything that is -- the proto
// package, the service and its method names, and every message, field name,
// field number, type and cardinality, plus the reserved range that keeps a
// burned field number burned -- has to match exactly.
//
// If this fails and the change is deliberate, it needs a new service version,
// not a new golden file.
func TestWireContractIsUnchanged(t *testing.T) {
	golden, err := os.ReadFile("testdata/runtime_v1.textproto")
	assert.NilError(t, err)

	var want descriptorpb.FileDescriptorProto
	assert.NilError(t, prototext.Unmarshal(golden, &want))

	got := normalize(protodesc.ToFileDescriptorProto(File_internal_extensions_sdk_sdkapi_extension_proto))
	if !proto.Equal(got, &want) {
		t.Fatalf("the extension runtime wire contract changed.\n--- want (protoc, runtime.proto)\n%s\n--- got (mobyextgen, Go-first)\n%s",
			prototext.Format(&want), prototext.Format(got))
	}

	// The two identifiers a deployed extension dials. Comparing descriptors would
	// not catch these moving together with the golden.
	assert.Equal(t, got.GetPackage(), "moby.extension.runtime.v1")
	assert.Equal(t, serviceName, "moby.extension.runtime.v1.Extension")
}

// normalize strips what is Go-level or positional rather than wire-level: the
// file path, its Go package option, and the order things happen to be declared
// in.
func normalize(fd *descriptorpb.FileDescriptorProto) *descriptorpb.FileDescriptorProto {
	fd = proto.Clone(fd).(*descriptorpb.FileDescriptorProto)
	fd.Name = nil
	fd.Options = nil
	sort.Slice(fd.MessageType, func(i, j int) bool { return fd.MessageType[i].GetName() < fd.MessageType[j].GetName() })
	for _, m := range fd.MessageType {
		sort.Slice(m.Field, func(i, j int) bool { return m.Field[i].GetNumber() < m.Field[j].GetNumber() })
	}
	return fd
}
