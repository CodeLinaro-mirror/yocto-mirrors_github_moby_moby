package main

import (
	"fmt"

	gengo "google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// emitMessages generates the point's protobuf message code -- what protoc plus
// protoc-gen-go used to produce -- without either. The descriptor protoc would
// have compiled from the emitted .proto is built here directly from the same
// contract model, and handed to protoc-gen-go's generator, which is an ordinary
// vendored Go package.
//
// This is not a shortcut around protoc: it is the same code protoc-gen-go runs,
// on an equivalent descriptor, so the output is the code protoc produced before.
// What it removes is the pinned protoc binary (fetched over the network at
// image-build time), the separately installed plugin whose version drifted from
// the vendored protobuf module, and the requirement that anyone running
// `go generate` have both on their PATH.
func emitMessages(pt point) ([]byte, error) {
	fd, err := fileDescriptor(pt)
	if err != nil {
		return nil, err
	}
	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{pt.protoPath},
		ProtoFile:      []*descriptorpb.FileDescriptorProto{fd},
	}
	gen, err := protogen.Options{}.New(req)
	if err != nil {
		return nil, fmt.Errorf("build protobuf generator: %w", err)
	}
	gen.SupportedFeatures = gengo.SupportedFeatures
	gen.SupportedEditionsMinimum = gengo.SupportedEditionsMinimum
	gen.SupportedEditionsMaximum = gengo.SupportedEditionsMaximum
	// The request deliberately carries no CompilerVersion: there is no protoc in
	// this pipeline, so the header records `protoc (unknown)` rather than
	// claiming a compiler that did not run. The markers stay on for the static
	// check they also emit, which fails the build if the protobuf runtime is
	// older than the code generated against it.
	for _, f := range gen.Files {
		if f.Generate {
			gengo.GenerateFile(gen, f)
		}
	}
	resp := gen.Response()
	if resp.Error != nil {
		return nil, fmt.Errorf("generate protobuf code: %s", resp.GetError())
	}
	if len(resp.File) != 1 {
		return nil, fmt.Errorf("expected 1 generated protobuf file, got %d", len(resp.File))
	}
	return []byte(resp.File[0].GetContent()), nil
}

// fileDescriptor builds the FileDescriptorProto for the point: the compiled form
// of the .proto emitted by [emitProto], from the same model, so the two cannot
// disagree.
func fileDescriptor(pt point) (*descriptorpb.FileDescriptorProto, error) {
	fd := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(pt.protoPath),
		Package: proto.String(pt.id),
		Syntax:  proto.String("proto3"),
		Options: &descriptorpb.FileOptions{GoPackage: proto.String(pt.protogenImport())},
	}

	for _, msg := range pt.messages {
		d, err := messageDescriptor(pt.id, msg)
		if err != nil {
			return nil, err
		}
		fd.MessageType = append(fd.MessageType, d)
	}
	// Empty response messages for the bare-`error` methods, matching emitProto.
	for _, m := range pt.methods {
		if m.bareError {
			fd.MessageType = append(fd.MessageType, &descriptorpb.DescriptorProto{Name: proto.String(m.response)})
		}
	}

	svc := &descriptorpb.ServiceDescriptorProto{Name: proto.String(pt.service)}
	for _, m := range pt.methods {
		svc.Method = append(svc.Method, &descriptorpb.MethodDescriptorProto{
			Name:       proto.String(m.name),
			InputType:  proto.String("." + pt.id + "." + m.request),
			OutputType: proto.String("." + pt.id + "." + m.response),
		})
	}
	fd.Service = []*descriptorpb.ServiceDescriptorProto{svc}
	return fd, nil
}

func messageDescriptor(pkg string, msg message) (*descriptorpb.DescriptorProto, error) {
	d := &descriptorpb.DescriptorProto{Name: proto.String(msg.name)}
	for _, n := range msg.reserved {
		// Ranges are half-open, so a single burned number spans [n, n+1).
		d.ReservedRange = append(d.ReservedRange, &descriptorpb.DescriptorProto_ReservedRange{
			Start: proto.Int32(int32(n)), End: proto.Int32(int32(n) + 1),
		})
	}
	for _, f := range msg.fields {
		fd := &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(f.protoName),
			Number:   proto.Int32(int32(f.number)),
			JsonName: proto.String(jsonName(f.protoName)),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
		switch f.kind {
		case scalarSingle:
			t, err := scalarDescriptorType(f.protoType)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", msg.name, f.protoName, err)
			}
			fd.Type = t.Enum()
		case scalarRepeated:
			t, err := scalarDescriptorType(f.protoType)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", msg.name, f.protoName, err)
			}
			fd.Type = t.Enum()
			fd.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		case messageSingle:
			fd.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
			fd.TypeName = proto.String("." + pkg + "." + f.protoType)
		case messageRepeated:
			fd.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
			fd.TypeName = proto.String("." + pkg + "." + f.protoType)
			fd.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		case scalarMap:
			// A proto map is sugar for a repeated nested entry message; the
			// descriptor has to spell that out, exactly as protoc does when it
			// compiles `map<k, v>`.
			key, err := scalarDescriptorType(f.mapKey)
			if err != nil {
				return nil, fmt.Errorf("%s.%s key: %w", msg.name, f.protoName, err)
			}
			val, err := scalarDescriptorType(f.protoType)
			if err != nil {
				return nil, fmt.Errorf("%s.%s value: %w", msg.name, f.protoName, err)
			}
			entry := goCamelCase(f.protoName) + "Entry"
			d.NestedType = append(d.NestedType, &descriptorpb.DescriptorProto{
				Name:    proto.String(entry),
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name: proto.String("key"), Number: proto.Int32(1), JsonName: proto.String("key"),
						Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: key.Enum(),
					},
					{
						Name: proto.String("value"), Number: proto.Int32(2), JsonName: proto.String("value"),
						Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: val.Enum(),
					},
				},
			})
			fd.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
			fd.TypeName = proto.String("." + pkg + "." + msg.name + "." + entry)
			fd.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		}
		d.Field = append(d.Field, fd)
	}
	return d, nil
}

// scalarDescriptorType maps a proto type token, as emitted into the .proto, to
// its descriptor enum.
func scalarDescriptorType(protoType string) (descriptorpb.FieldDescriptorProto_Type, error) {
	switch protoType {
	case "string":
		return descriptorpb.FieldDescriptorProto_TYPE_STRING, nil
	case "bytes":
		return descriptorpb.FieldDescriptorProto_TYPE_BYTES, nil
	case "bool":
		return descriptorpb.FieldDescriptorProto_TYPE_BOOL, nil
	case "int32":
		return descriptorpb.FieldDescriptorProto_TYPE_INT32, nil
	case "int64":
		return descriptorpb.FieldDescriptorProto_TYPE_INT64, nil
	case "uint32":
		return descriptorpb.FieldDescriptorProto_TYPE_UINT32, nil
	case "uint64":
		return descriptorpb.FieldDescriptorProto_TYPE_UINT64, nil
	case "float":
		return descriptorpb.FieldDescriptorProto_TYPE_FLOAT, nil
	case "double":
		return descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, nil
	}
	return 0, fmt.Errorf("unsupported proto type %q", protoType)
}

// jsonName is protoc's default json_name for a field: the lowerCamelCase of its
// proto name. protoc computes it during compilation, so the descriptor built
// here has to do the same or the generated struct tags differ.
func jsonName(protoName string) string {
	var b []byte
	upper := false
	for i := 0; i < len(protoName); i++ {
		c := protoName[i]
		if c == '_' {
			upper = true
			continue
		}
		if upper && isASCIILower(c) {
			c -= 'a' - 'A'
		}
		upper = false
		b = append(b, c)
	}
	return string(b)
}
