// Command mobyextgen generates an extension point's wire contract and all of its
// transport code from a Go-first contract: a Go interface (the point's provider
// interface, named in its extensions.DefinePoint call) plus message structs
// whose fields carry pb:"N" tags giving their proto field numbers.
//
// Run with no arguments from the point's own package -- `go generate` does that
// -- it emits, into that package:
//
//   - <service>.proto:       the .proto, derived from the Go interface and
//     structs. It is the reviewable lock on the wire contract: a renamed
//     service or message shows up in it as a diff.
//   - protogen/<service>.pb.go: the protobuf message code.
//   - protogen/wire.gen.go:  the point's gRPC service, its client, the adapters
//     that satisfy the point's Go interface across the
//     boundary, and the Go<->proto conversions.
//
// The Go interface and structs are the source of truth; everything else is
// generated. It supports the narrow shapes points use -- scalars, strings,
// bytes, repeated scalars, string-keyed maps, and repeated messages -- and
// errors on anything else rather than emitting something subtly wrong.
//
// It is the whole pipeline: no protoc, no separately installed protoc plugins,
// and nothing to have on your PATH beyond the Go toolchain. See descriptor.go
// for how the protobuf code is produced.
package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

func main() {
	dir := "."
	if args := os.Args[1:]; len(args) == 1 {
		dir = args[0]
	} else if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: mobyextgen [dir]")
		os.Exit(2)
	}
	if err := run(dir); err != nil {
		fmt.Fprintln(os.Stderr, "mobyextgen:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	pt, err := parsePoint(dir)
	if err != nil {
		return err
	}
	// Everything the generator used to be told on the command line is derivable
	// from where the contract lives, so a point's go:generate line carries no
	// paths to keep in sync. The one thing that is not derivable is the gRPC
	// service name -- it is part of the wire contract, and the point's interface
	// name is not always it -- so the contract states it in a pragma.
	importPath, relDir, err := locate(dir)
	if err != nil {
		return err
	}
	pt.importPath = importPath
	protoName := camelToSnake(pt.service) + ".proto"
	// The .proto is named by its module-relative path: that is how it is spelled
	// in the generated file's source comment and in the descriptor.
	pt.protoPath = path.Join(relDir, protoName)

	protoFile, err := emitProto(pt)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, protoName), protoFile, 0o644); err != nil {
		return err
	}

	messages, err := emitMessages(pt)
	if err != nil {
		return err
	}
	wire, err := emitWire(pt)
	if err != nil {
		return err
	}
	// The generated code lives in the protogen package so the contract package
	// stays free of any protobuf/gRPC imports.
	protogenDir := filepath.Join(dir, "protogen")
	if err := os.MkdirAll(protogenDir, 0o755); err != nil {
		return err
	}
	pbName := strings.TrimSuffix(protoName, ".proto") + ".pb.go"
	if err := os.WriteFile(filepath.Join(protogenDir, pbName), messages, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(protogenDir, "wire.gen.go"), wire, 0o644)
}

// locate returns the import path of the package in dir and its module-relative
// directory, from the module path declared by the enclosing go.mod and dir's
// position under it. Both used to be passed on the command line, where they were
// four hand-maintained facts per point that nothing checked.
func locate(dir string) (importPath, relDir string, _ error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	for root := abs; ; {
		gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
		if err == nil {
			module, err := modulePath(gomod)
			if err != nil {
				return "", "", err
			}
			rel, err := filepath.Rel(root, abs)
			if err != nil {
				return "", "", err
			}
			relDir = filepath.ToSlash(rel)
			return path.Join(module, relDir), relDir, nil
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", "", fmt.Errorf("no go.mod found above %s", abs)
		}
		root = parent
	}
}

// modulePath returns the module path declared by a go.mod file.
func modulePath(gomod []byte) (string, error) {
	for line := range strings.Lines(string(gomod)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", errors.New("no module directive in go.mod")
}

// model

type point struct {
	pkgName    string // Go package name
	importPath string // package import path
	protoPath  string // module-relative path of the .proto
	id         string // proto package: the point id, or the pragma's package in service mode
	service    string // gRPC service name (from the mobyextgen:service pragma)
	iface      string // Go service interface name
	isPoint    bool   // whether the contract declares an extensions.Point
	isSingle   bool   // whether the point was declared with DefineSinglePoint
	methods    []method
	messages   []message
}

// grpcService is the fully-qualified gRPC service name: proto package plus
// service. It is what a caller names on the wire and what the SDK reports to the
// daemon as the service a provider serves.
func (p point) grpcService() string { return p.id + "." + p.service }

func (p point) protogenImport() string { return p.importPath + "/protogen" }

type method struct {
	name      string // method/rpc name
	request   string // request message name
	response  string // response message name
	bareError bool   // true for `M(ctx, *Req) error`; response is a generated empty message
}

type message struct {
	name     string
	fields   []field
	reserved []int // field numbers burned by a removed field
}

type fieldKind int

const (
	scalarSingle fieldKind = iota
	scalarRepeated
	scalarMap
	messageSingle
	messageRepeated
)

type field struct {
	goName      string // Go field name on the contract struct (e.g. ContainerID)
	protoName   string // proto3 field name (e.g. container_id)
	protoGoName string // Go field name protoc-gen-go emits for protoName (e.g. ContainerId)
	number      int
	protoType   string // proto type token, or the message name for messageSingle/messageRepeated
	mapKey      string // proto key type for scalarMap
	kind        fieldKind
}

// parsing

func parsePoint(dir string) (point, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return point{}, err
	}

	var files []*ast.File
	var pkgName string
	for name, pkg := range pkgs {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		pkgName = name
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}
	if pkgName == "" {
		return point{}, fmt.Errorf("no package found in %s", dir)
	}

	pt := point{pkgName: pkgName}

	// A contract is either an extension point -- whose DefinePoint call names the
	// provider interface and the point id -- or a plain gRPC service the framework
	// itself speaks, such as the SDK runtime handshake, which has no point. The
	// service pragma covers the second case by naming the service outright.
	svc, err := findServicePragma(files)
	if err != nil {
		return point{}, err
	}
	pt.service = svc.service
	if svc.iface != "" {
		// Service mode: the pragma documents the service interface and carries the
		// proto package, because there is no point id to take it from.
		pt.iface, pt.id = svc.iface, svc.pkg
	} else {
		iface, id, single, err := findDefinePoint(files)
		if err != nil {
			return point{}, err
		}
		pt.iface, pt.id, pt.isPoint, pt.isSingle = iface, id, true, single
	}

	// Collect message structs (those whose fields carry pb tags) and the
	// provider interface methods.
	msgNames := messageNames(files)
	ifaceType := findInterface(files, pt.iface)
	if ifaceType == nil {
		return point{}, fmt.Errorf("interface %q not found", pt.iface)
	}
	pt.methods, err = parseMethods(ifaceType)
	if err != nil {
		return point{}, err
	}
	// A method's request -- and a non-bare response -- must be emitted even when
	// it has no fields (e.g. an empty marker query), so add them to the set of
	// messages to emit alongside the pb-tagged structs.
	for _, m := range pt.methods {
		msgNames[m.request] = true
		if !m.bareError {
			msgNames[m.response] = true
		}
	}
	pt.messages, err = parseMessages(files, msgNames)
	if err != nil {
		return point{}, err
	}
	return pt, nil
}

// findDefinePoint returns the type argument (the provider interface) and the
// string argument (point id) of the contract's extensions.DefinePoint[T]("id")
// or extensions.DefineSinglePoint[T]("id") call, and which of the two it was:
// the constructor is where a contract declares its cardinality, and the
// generator carries it into the point's client wiring.
func findDefinePoint(files []*ast.File) (iface, id string, single bool, err error) {
	for _, f := range files {
		for _, decl := range f.Decls {
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				idx, ok := call.Fun.(*ast.IndexExpr)
				if !ok {
					return true
				}
				sel, ok := idx.X.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "DefinePoint" && sel.Sel.Name != "DefineSinglePoint") {
					return true
				}
				single = sel.Sel.Name == "DefineSinglePoint"
				if t, ok := idx.Index.(*ast.Ident); ok {
					iface = t.Name
				}
				if len(call.Args) == 1 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, e := strconv.Unquote(lit.Value); e == nil {
							id = v
						}
					}
				}
				return false
			})
		}
	}
	if iface == "" || id == "" {
		return "", "", false, errors.New("no extensions.DefinePoint[T](\"id\") call found")
	}
	return iface, id, single, nil
}

// servicePragma introduces a contract's gRPC service name, and reservedPragma a
// message field number that a removed field burned:
//
//	//mobyextgen:service=CreateSpecHook
//	var Point = extensions.DefinePoint[Hook]("...create_spec.v0")
//
//	//mobyextgen:reserved=2
//	type PointDeclaration struct{ ... }
const (
	servicePragma  = "//mobyextgen:service="
	reservedPragma = "//mobyextgen:reserved="
)

// serviceDecl is what the service pragma resolved to. In point mode only the
// service name is given and iface is empty, because the point supplies the
// interface and the proto package. In service mode the pragma documents the
// interface itself and names the service fully qualified, so it supplies all
// three.
type serviceDecl struct {
	service string
	iface   string
	pkg     string
}

// findServicePragma returns the gRPC service the contract declares.
//
// The service name is required rather than defaulted from the provider
// interface name, because the two legitimately differ -- create-spec's interface
// is Hook but its service is CreateSpecHook -- and a default would silently
// rename a service on the wire when an interface is renamed. It lives in the
// source, next to what it names, rather than in a generator flag: it is part of
// the wire contract, so it belongs where the contract is reviewed.
//
// Where it sits decides how it reads. On an interface it names the service fully
// qualified, `<proto.package>.<Service>`, and that interface is the service --
// the shape a contract with no extensions.Point uses. Anywhere else (in
// practice, on the Point) it is the bare service name and the point supplies the
// rest.
func findServicePragma(files []*ast.File) (serviceDecl, error) {
	var found serviceDecl
	var seen string
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				values := declPragmas(gd, spec, servicePragma)
				if len(values) == 0 {
					continue
				}
				for _, value := range values {
					if seen != "" && seen != value {
						return serviceDecl{}, fmt.Errorf("conflicting %s pragmas: %q and %q", servicePragma, seen, value)
					}
					seen = value
				}
				value := values[0]

				ts, isType := spec.(*ast.TypeSpec)
				if !isType {
					found = serviceDecl{service: value}
					continue
				}
				if _, isIface := ts.Type.(*ast.InterfaceType); !isIface {
					return serviceDecl{}, fmt.Errorf("%s on %s: the pragma may only document an interface or the point", servicePragma, ts.Name.Name)
				}
				pkg, service, ok := strings.Cut(reverse(value), ".")
				if !ok {
					return serviceDecl{}, fmt.Errorf("%s on interface %s: want a fully-qualified name like my.proto.package.%s", servicePragma, ts.Name.Name, ts.Name.Name)
				}
				found = serviceDecl{service: reverse(pkg), iface: ts.Name.Name, pkg: reverse(service)}
			}
		}
	}
	if seen == "" {
		return serviceDecl{}, fmt.Errorf("no service pragma found; declare the contract's gRPC service name as `%s<Name>` next to its DefinePoint call, or fully qualified on its service interface", servicePragma)
	}
	return found, nil
}

// reverse returns s with its bytes reversed, so strings.Cut can split on the
// *last* separator.
func reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// declPragmas returns every value of the named pragma in the doc comment of
// spec, plus the enclosing declaration's doc comment, which is where the comment
// lands for a single-spec declaration such as `// Doc\ntype T struct{}`. All of
// them are returned, not just the first, so a contract that states the same
// pragma twice is reported rather than silently resolved to whichever came
// first.
func declPragmas(gd *ast.GenDecl, spec ast.Spec, pragma string) []string {
	docs := []*ast.CommentGroup{gd.Doc}
	switch sp := spec.(type) {
	case *ast.TypeSpec:
		docs = append(docs, sp.Doc)
	case *ast.ValueSpec:
		docs = append(docs, sp.Doc)
	}
	var values []string
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for _, c := range doc.List {
			if value, ok := strings.CutPrefix(c.Text, pragma); ok {
				values = append(values, strings.TrimSpace(value))
			}
		}
	}
	return values
}

func findInterface(files []*ast.File, name string) *ast.InterfaceType {
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if ts.Name.Name != name {
					continue
				}
				if it, ok := ts.Type.(*ast.InterfaceType); ok {
					return it
				}
			}
		}
	}
	return nil
}

// messageNames returns the set of struct types that have at least one pb-tagged
// field -- the message types of the contract.
func messageNames(files []*ast.File) map[string]bool {
	names := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				if structHasPBTag(st) {
					names[ts.Name.Name] = true
				}
			}
		}
	}
	return names
}

func structHasPBTag(st *ast.StructType) bool {
	for _, f := range st.Fields.List {
		// A malformed tag counts as present, so the struct is still parsed as a
		// message and parseMessage reports the tag rather than the struct being
		// silently treated as a plain type.
		if _, present, _ := pbNumber(f); present {
			return true
		}
	}
	return false
}

func parseMethods(iface *ast.InterfaceType) ([]method, error) {
	var methods []method
	for _, m := range iface.Methods.List {
		ft, ok := m.Type.(*ast.FuncType)
		if !ok || len(m.Names) == 0 {
			continue
		}
		name := m.Names[0].Name
		if ft.Params == nil || len(ft.Params.List) == 0 {
			return nil, fmt.Errorf("method %q: expected a request parameter", name)
		}
		// The request is the last parameter (after ctx).
		reqType := ft.Params.List[len(ft.Params.List)-1].Type
		req, err := pointerIdent(reqType)
		if err != nil {
			return nil, fmt.Errorf("method %q request: %w", name, err)
		}
		// Supported result shapes: `error` (bare; an empty response message is
		// generated) or `(*Resp, error)` (typed; Resp is a message).
		res := results(ft)
		switch {
		case len(res) == 1 && isIdent(res[0], "error"):
			methods = append(methods, method{name: name, request: req, response: name + "Response", bareError: true})
		case len(res) == 2 && isIdent(res[1], "error"):
			resp, err := pointerIdent(res[0])
			if err != nil {
				return nil, fmt.Errorf("method %q response: %w", name, err)
			}
			methods = append(methods, method{name: name, request: req, response: resp})
		default:
			return nil, fmt.Errorf("method %q: result must be `error` or `(*Resp, error)`", name)
		}
	}
	return methods, nil
}

func parseMessages(files []*ast.File, msgNames map[string]bool) ([]message, error) {
	var messages []message
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok || !msgNames[ts.Name.Name] {
					continue
				}
				msg, err := parseMessage(ts.Name.Name, st, msgNames)
				if err != nil {
					return nil, err
				}
				if msg.reserved, err = parseReserved(gd, spec); err != nil {
					return nil, fmt.Errorf("%s: %w", ts.Name.Name, err)
				}
				messages = append(messages, msg)
			}
		}
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].name < messages[j].name })
	return messages, nil
}

// parseReserved returns the field numbers a message declares as burned. Proto
// forbids reusing the number of a deleted field: an old peer still on the wire
// would decode the new field's bytes as the old one. The contract records that
// with a pragma, since a number with no field has nowhere else to live in Go:
//
//	//mobyextgen:reserved=2 // was 'exclusive'
//	type PointDeclaration struct{ ... }
func parseReserved(gd *ast.GenDecl, spec ast.Spec) ([]int, error) {
	var numbers []int
	for _, value := range declPragmas(gd, spec, reservedPragma) {
		// Trailing text after the numbers is the note saying what used to be there.
		list, _, _ := strings.Cut(value, " ")
		for _, f := range strings.Split(list, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				return nil, fmt.Errorf("%s%s: want a comma-separated list of field numbers", reservedPragma, value)
			}
			if n < 1 {
				return nil, fmt.Errorf("reserved field number must be >= 1, got %d", n)
			}
			numbers = append(numbers, n)
		}
	}
	sort.Ints(numbers)
	return slices.Compact(numbers), nil
}

func parseMessage(name string, st *ast.StructType, msgNames map[string]bool) (message, error) {
	msg := message{name: name}
	byNumber := map[int]string{} // field number -> Go field name, to catch reuse
	for _, f := range st.Fields.List {
		num, present, err := pbNumber(f)
		if err != nil {
			field := "field"
			if len(f.Names) > 0 {
				field = f.Names[0].Name
			}
			return message{}, fmt.Errorf("%s.%s: %w", name, field, err)
		}
		if !present || len(f.Names) == 0 {
			continue
		}
		// A grouped declaration (A, B string `pb:"1"`) would give every name the
		// same field number, which is invalid. Reject it rather than silently
		// keeping only the first name and dropping the rest from the wire.
		if len(f.Names) > 1 {
			names := make([]string, len(f.Names))
			for i, n := range f.Names {
				names[i] = n.Name
			}
			return message{}, fmt.Errorf("%s: fields %s share a single pb tag (field number %d); declare each field separately with its own number", name, strings.Join(names, ", "), num)
		}
		goName := f.Names[0].Name
		// Proto field numbers start at 1, and each must be unique within the
		// message -- a duplicate or a zero would produce a broken wire contract.
		if num < 1 {
			return message{}, fmt.Errorf("%s.%s: pb field number must be >= 1, got %d", name, goName, num)
		}
		if prev, dup := byNumber[num]; dup {
			return message{}, fmt.Errorf("%s: pb field number %d is used by both %s and %s", name, num, prev, goName)
		}
		byNumber[num] = goName
		protoName := camelToSnake(goName)
		fl := field{goName: goName, protoName: protoName, protoGoName: goCamelCase(protoName), number: num}
		if err := classify(f.Type, msgNames, &fl); err != nil {
			return message{}, fmt.Errorf("%s.%s: %w", name, goName, err)
		}
		msg.fields = append(msg.fields, fl)
	}
	sort.Slice(msg.fields, func(i, j int) bool { return msg.fields[i].number < msg.fields[j].number })
	return msg, nil
}

// classify fills in fl.kind, fl.protoType and fl.mapKey from a Go field type.
func classify(expr ast.Expr, msgNames map[string]bool, fl *field) error {
	switch t := expr.(type) {
	case *ast.Ident:
		if s, ok := scalarProtoType(t.Name); ok {
			fl.kind, fl.protoType = scalarSingle, s
			return nil
		}
		if err := rejectAmbiguousInt(t.Name); err != nil {
			return err
		}
		if msgNames[t.Name] {
			return fmt.Errorf("embed a message by pointer (*%s), not by value", t.Name)
		}
		return fmt.Errorf("unsupported field type %q", t.Name)
	case *ast.StarExpr:
		// A single embedded message: *SomeMessage. Proto message fields are
		// pointers in generated Go, so the contract uses a pointer too, which
		// also lets the field be nil / absent.
		id, ok := t.X.(*ast.Ident)
		if !ok || !msgNames[id.Name] {
			return errors.New("unsupported pointer field type (only *Message is allowed)")
		}
		fl.kind, fl.protoType = messageSingle, id.Name
		return nil
	case *ast.ArrayType:
		if id, ok := t.Elt.(*ast.Ident); ok && id.Name == "byte" {
			fl.kind, fl.protoType = scalarSingle, "bytes"
			return nil
		}
		elt, ok := t.Elt.(*ast.Ident)
		if !ok {
			return errors.New("unsupported slice element type")
		}
		if s, ok := scalarProtoType(elt.Name); ok {
			fl.kind, fl.protoType = scalarRepeated, s
			return nil
		}
		if err := rejectAmbiguousInt(elt.Name); err != nil {
			return err
		}
		if msgNames[elt.Name] {
			fl.kind, fl.protoType = messageRepeated, elt.Name
			return nil
		}
		return fmt.Errorf("unsupported slice element type %q", elt.Name)
	case *ast.MapType:
		// Proto3 forbids float, bytes, and message map keys; the framework
		// narrows this to string keys, the only kind points use and the shape
		// the docs promise. Rejecting other keys here keeps the generator from
		// emitting a .proto that protoc would reject (or, for floats, silently
		// mis-generate).
		key, ok := t.Key.(*ast.Ident)
		if !ok || key.Name != "string" {
			return errors.New("map keys must be strings")
		}
		val, ok := t.Value.(*ast.Ident)
		if !ok {
			return errors.New("unsupported map value type")
		}
		vs, ok := scalarProtoType(val.Name)
		if !ok {
			if err := rejectAmbiguousInt(val.Name); err != nil {
				return err
			}
			return fmt.Errorf("only scalar map values are supported (value type %q)", val.Name)
		}
		fl.kind, fl.protoType, fl.mapKey = scalarMap, vs, "string"
		return nil
	default:
		return errors.New("unsupported field type")
	}
}

// emit: proto

func emitProto(pt point) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintln(&b, "// Code generated by mobyextgen. DO NOT EDIT.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, `syntax = "proto3";`)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "package %s;\n\n", pt.id)
	fmt.Fprintf(&b, "option go_package = %q;\n\n", pt.protogenImport())

	fmt.Fprintf(&b, "service %s {\n", pt.service)
	for _, m := range pt.methods {
		fmt.Fprintf(&b, "  rpc %s(%s) returns (%s);\n", m.name, m.request, m.response)
	}
	fmt.Fprintln(&b, "}")

	for _, msg := range pt.messages {
		fmt.Fprintf(&b, "\nmessage %s {\n", msg.name)
		for _, f := range msg.fields {
			if slices.Contains(msg.reserved, f.number) {
				return nil, fmt.Errorf("%s.%s: field number %d is reserved", msg.name, f.protoName, f.number)
			}
			switch f.kind {
			case scalarSingle, messageSingle:
				fmt.Fprintf(&b, "  %s %s = %d;\n", f.protoType, f.protoName, f.number)
			case scalarRepeated, messageRepeated:
				fmt.Fprintf(&b, "  repeated %s %s = %d;\n", f.protoType, f.protoName, f.number)
			case scalarMap:
				fmt.Fprintf(&b, "  map<%s, %s> %s = %d;\n", f.mapKey, f.protoType, f.protoName, f.number)
			}
		}
		for _, n := range msg.reserved {
			fmt.Fprintf(&b, "  reserved %d;\n", n)
		}
		fmt.Fprintln(&b, "}")
	}

	// Empty response messages for the bare-`error` methods.
	for _, m := range pt.methods {
		if m.bareError {
			fmt.Fprintf(&b, "\nmessage %s {}\n", m.response)
		}
	}
	return []byte(b.String()), nil
}

// emit: wire.gen.go

func emitWire(pt point) ([]byte, error) {
	var b strings.Builder
	cpkg := pt.pkgName
	svc, iface := pt.service, pt.iface
	fmt.Fprintln(&b, "// Code generated by mobyextgen. DO NOT EDIT.")
	fmt.Fprintf(&b, "package %s\n\n", path.Base(pt.protogenImport()))
	fmt.Fprintln(&b, "import (")
	fmt.Fprintln(&b, `	context "context"`)
	if pt.isPoint {
		fmt.Fprintln(&b, `	extensions "github.com/moby/moby/v2/internal/extensions"`)
		fmt.Fprintln(&b, `	clientpoint "github.com/moby/moby/v2/internal/extensions/clientpoint"`)
		fmt.Fprintln(&b, `	serverpoint "github.com/moby/moby/v2/internal/extensions/serverpoint"`)
	}
	fmt.Fprintf(&b, "	%s %q\n", cpkg, pt.importPath)
	fmt.Fprintln(&b, `	grpc "google.golang.org/grpc"`)
	fmt.Fprintln(&b, ")")

	// The service itself: its name, its handlers, and the descriptor gRPC
	// dispatches on. This is what protoc-gen-go-grpc used to emit into a separate
	// _grpc.pb.go, minus the parts nothing here uses -- the Unimplemented and
	// Unsafe embeds, whose forward-compatibility story is for services with
	// independently evolving implementations, not for a service whose only
	// implementation is generated from the same contract in the same pass.
	fmt.Fprintf(&b, `
// serviceName is the point's fully-qualified gRPC service name.
const serviceName = %q
`, pt.grpcService())

	fmt.Fprintln(&b, "\nconst (")
	for _, m := range pt.methods {
		fmt.Fprintf(&b, "\t%s = \"/\" + serviceName + %q\n", methodConst(m), "/"+m.name)
	}
	fmt.Fprintln(&b, ")")

	// The proto/gRPC types are local to this (protogen) package; the contract
	// types are reached through the contract package, cpkg.
	fmt.Fprintf(&b, `
// %[1]sServer is the server side of the point's gRPC service. It is the
// proto-level shape of the point, not the point's Go interface: a contract
// method returning a bare error returns an empty response message here.
type %[1]sServer interface {
`, svc)
	for _, m := range pt.methods {
		fmt.Fprintf(&b, "\t%s(context.Context, *%s) (*%s, error)\n", m.name, m.request, m.response)
	}
	fmt.Fprintln(&b, "}")

	fmt.Fprintf(&b, `
// serviceDesc describes the point's gRPC service to a server. HandlerType is
// what a registrar type-checks an implementation against, so registering the
// wrong provider for this point is caught at registration.
var serviceDesc = grpc.ServiceDesc{
	ServiceName: serviceName,
	HandlerType: (*%[1]sServer)(nil),
	Metadata:    %[2]q,
	Methods: []grpc.MethodDesc{
`, svc, pt.protoPath)
	for _, m := range pt.methods {
		fmt.Fprintf(&b, "\t\t{MethodName: %q, Handler: %s},\n", m.name, handlerName(m))
	}
	fmt.Fprintln(&b, "\t},\n}")

	for _, m := range pt.methods {
		fmt.Fprintf(&b, `
func %[1]s(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(%[2]s)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(%[3]sServer).%[4]s(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: %[5]s}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(%[3]sServer).%[4]s(ctx, req.(*%[2]s))
	})
}
`, handlerName(m), m.request, svc, m.name, methodConst(m))
	}

	fmt.Fprintf(&b, `
// %[1]sClient calls the point's gRPC service. It is exported so a client
// outside the framework -- one calling a service an extension publishes on the
// API socket -- can reach it with a plain gRPC client.
type %[1]sClient interface {
`, svc)
	for _, m := range pt.methods {
		fmt.Fprintf(&b, "\t%s(ctx context.Context, in *%s, opts ...grpc.CallOption) (*%s, error)\n", m.name, m.request, m.response)
	}
	fmt.Fprintln(&b, "}")

	fmt.Fprintf(&b, `
// New%[1]sClient returns a client for the point's gRPC service on cc.
func New%[1]sClient(cc grpc.ClientConnInterface) %[1]sClient { return &serviceClient{cc: cc} }

type serviceClient struct{ cc grpc.ClientConnInterface }
`, svc)

	for _, m := range pt.methods {
		fmt.Fprintf(&b, `
func (c *serviceClient) %[1]s(ctx context.Context, in *%[2]s, opts ...grpc.CallOption) (*%[3]s, error) {
	out := new(%[3]s)
	if err := c.cc.Invoke(ctx, %[4]s, in, out, append([]grpc.CallOption{grpc.StaticMethod()}, opts...)...); err != nil {
		return nil, err
	}
	return out, nil
}
`, m.name, m.request, m.response, methodConst(m))
	}

	if pt.isPoint {
		fmt.Fprintf(&b, `
// ServerPoint serves the %[1]s point: it registers the point's gRPC service for
// a provider with an SDK server. A binary passes it to (*sdk.Server).Register.
var ServerPoint = serverpoint.Registration{
	Point: %[2]s.Point.ID(),
	Register: func(r grpc.ServiceRegistrar, impl any) {
		r.RegisterService(&serviceDesc, &grpcServer{impl: impl.(%[2]s.%[3]s)})
	},
}

// ClientProvider builds a broker provider for the %[1]s point from an
// out-of-process gRPC connection.
func ClientProvider(conn grpc.ClientConnInterface) extensions.Provider {
	return %[2]s.Point.Provide(&grpcClient{client: New%[1]sClient(conn)})
}

// ClientPoint registers ClientProvider for the %[1]s point with a host.%[4]s
var ClientPoint = clientpoint.Registration{Point: %[2]s.Point.ID(), Provider: ClientProvider%[5]s}
`, svc, cpkg, iface, singleDoc(pt), singleField(pt))
	} else {
		// No point, so there is no broker to hand the adapters to: the two sides
		// are constructed directly by whoever serves and whoever calls.
		fmt.Fprintf(&b, `
// RegisterServer serves impl as the %[1]s service on r.
func RegisterServer(r grpc.ServiceRegistrar, impl %[2]s.%[3]s) {
	r.RegisterService(&serviceDesc, &grpcServer{impl: impl})
}

// NewClient returns a %[2]s.%[3]s that calls the %[1]s service over conn.
func NewClient(conn grpc.ClientConnInterface) %[2]s.%[3]s {
	return &grpcClient{client: New%[1]sClient(conn)}
}
`, svc, cpkg, iface)
	}

	fmt.Fprintf(&b, `
// grpcServer serves an implementation of the contract's Go interface.
type grpcServer struct {
	impl %[1]s.%[2]s
}
`, cpkg, iface)

	for _, m := range pt.methods {
		if m.bareError {
			fmt.Fprintf(&b, `
func (s *grpcServer) %[1]s(ctx context.Context, req *%[2]s) (*%[3]s, error) {
	if err := s.impl.%[1]s(ctx, %[4]sFromProto(req)); err != nil {
		return nil, err
	}
	return &%[3]s{}, nil
}
`, m.name, m.request, m.response, lowerFirst(m.request))
		} else {
			fmt.Fprintf(&b, `
func (s *grpcServer) %[1]s(ctx context.Context, req *%[2]s) (*%[3]s, error) {
	resp, err := s.impl.%[1]s(ctx, %[4]sFromProto(req))
	if err != nil {
		return nil, err
	}
	return %[5]sToProto(resp), nil
}
`, m.name, m.request, m.response, lowerFirst(m.request), lowerFirst(m.response))
		}
	}

	fmt.Fprintf(&b, `
type grpcClient struct {
	client %sClient
}
`, svc)

	for _, m := range pt.methods {
		if m.bareError {
			fmt.Fprintf(&b, `
func (c *grpcClient) %[1]s(ctx context.Context, req *%[2]s.%[3]s) error {
	_, err := c.client.%[1]s(ctx, %[4]sToProto(req))
	return err
}
`, m.name, cpkg, m.request, lowerFirst(m.request))
		} else {
			fmt.Fprintf(&b, `
func (c *grpcClient) %[1]s(ctx context.Context, req *%[2]s.%[3]s) (*%[2]s.%[5]s, error) {
	resp, err := c.client.%[1]s(ctx, %[4]sToProto(req))
	if err != nil {
		return nil, err
	}
	return %[6]sFromProto(resp), nil
}
`, m.name, cpkg, m.request, lowerFirst(m.request), m.response, lowerFirst(m.response))
		}
	}

	for _, msg := range pt.messages {
		emitConversions(&b, cpkg, msg)
	}

	return format.Source([]byte(b.String()))
}

func emitConversions(b *strings.Builder, cpkg string, msg message) {
	conv := lowerFirst(msg.name)

	fmt.Fprintf(b, "\nfunc %sToProto(in *%s.%s) *%s {\n", conv, cpkg, msg.name, msg.name)
	fmt.Fprintln(b, "\tif in == nil {\n\t\treturn nil\n\t}")
	fmt.Fprintf(b, "\tout := &%s{}\n", msg.name)
	for _, f := range msg.fields {
		switch f.kind {
		case scalarSingle, scalarRepeated, scalarMap:
			fmt.Fprintf(b, "\tout.%s = in.%s\n", f.protoGoName, f.goName)
		case messageSingle:
			fmt.Fprintf(b, "\tout.%s = %sToProto(in.%s)\n", f.protoGoName, lowerFirst(f.protoType), f.goName)
		case messageRepeated:
			fmt.Fprintf(b, "\tfor i := range in.%s {\n\t\tout.%s = append(out.%s, %sToProto(&in.%s[i]))\n\t}\n",
				f.goName, f.protoGoName, f.protoGoName, lowerFirst(f.protoType), f.goName)
		}
	}
	fmt.Fprintln(b, "\treturn out\n}")

	fmt.Fprintf(b, "\nfunc %sFromProto(in *%s) *%s.%s {\n", conv, msg.name, cpkg, msg.name)
	fmt.Fprintln(b, "\tif in == nil {\n\t\treturn nil\n\t}")
	fmt.Fprintf(b, "\tout := &%s.%s{}\n", cpkg, msg.name)
	for _, f := range msg.fields {
		switch f.kind {
		case scalarSingle, scalarRepeated, scalarMap:
			fmt.Fprintf(b, "\tout.%s = in.Get%s()\n", f.goName, f.protoGoName)
		case messageSingle:
			fmt.Fprintf(b, "\tout.%s = %sFromProto(in.Get%s())\n", f.goName, lowerFirst(f.protoType), f.protoGoName)
		case messageRepeated:
			fmt.Fprintf(b, "\tfor _, e := range in.Get%s() {\n\t\tout.%s = append(out.%s, *%sFromProto(e))\n\t}\n",
				f.protoGoName, f.goName, f.goName, lowerFirst(f.protoType))
		}
	}
	fmt.Fprintln(b, "\treturn out\n}")
}

// helpers

// pbNumber returns the field's proto field number. present reports whether the
// field carries a pb tag at all, and is false only when there is none: a tag
// that is present but not a number is present with an error, never absent.
// Conflating the two would drop the field from the wire silently, so a typo
// like `pb:"one"` would generate a contract that compiles, runs, and quietly
// loses that field on every call.
func pbNumber(f *ast.Field) (n int, present bool, err error) {
	if f.Tag == nil {
		return 0, false, nil
	}
	tag, uerr := strconv.Unquote(f.Tag.Value)
	if uerr != nil {
		return 0, false, nil
	}
	v, ok := reflect.StructTag(tag).Lookup("pb")
	if !ok {
		return 0, false, nil
	}
	n, aerr := strconv.Atoi(v)
	if aerr != nil {
		return 0, true, fmt.Errorf("pb tag %q is not a field number", v)
	}
	return n, true, nil
}

func scalarProtoType(goType string) (string, bool) {
	switch goType {
	case "string":
		return "string", true
	case "bool":
		return "bool", true
	case "int32":
		return "int32", true
	case "int64":
		return "int64", true
	case "uint32":
		return "uint32", true
	case "uint64":
		return "uint64", true
	case "float32":
		return "float", true
	case "float64":
		return "double", true
	}
	return "", false
}

// rejectAmbiguousInt reports an error for the width-ambiguous Go integer types
// int and uint. Proto has no such type: protoc-gen-go emits int64/uint64 for
// them, so the wire conversions -- which assign the contract field to the
// generated field directly -- would not even compile (int is not int64). Rather
// than pick a width silently, the contract must name an explicit one.
func rejectAmbiguousInt(goType string) error {
	if goType == "int" || goType == "uint" {
		return fmt.Errorf("%s has no fixed width on the wire; use a sized integer such as int32, int64, uint32, or uint64", goType)
	}
	return nil
}

func pointerIdent(expr ast.Expr) (string, error) {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return "", errors.New("expected a pointer type")
	}
	id, ok := star.X.(*ast.Ident)
	if !ok {
		return "", errors.New("expected a named type")
	}
	return id.Name, nil
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

// results flattens a function's result list into one expr per result.
func results(ft *ast.FuncType) []ast.Expr {
	if ft.Results == nil {
		return nil
	}
	var out []ast.Expr
	for _, f := range ft.Results.List {
		if len(f.Names) == 0 {
			out = append(out, f.Type)
			continue
		}
		for range f.Names {
			out = append(out, f.Type)
		}
	}
	return out
}

// camelToSnake converts a Go field name to a proto3 snake_case field name,
// treating an initialism run as a single word: ContainerID -> container_id,
// HTTPServer -> http_server, APIKey -> api_key. A word boundary (underscore) is
// inserted before an uppercase letter that either follows a lowercase or digit,
// or begins a new word after an acronym (i.e. it is itself followed by a
// lowercase). The old rule -- an underscore before every uppercase -- produced
// container_i_d; it round-tripped back to ContainerID through protoc's casing,
// but leaked broken field names into the wire contract every non-Go author reads.
//
// A lone trailing lowercase "s" is treated as a plural suffix on the acronym,
// not the start of a new word, so ContainerIDs -> container_ids and CPUs ->
// cpus rather than container_i_ds / cp_us.
func camelToSnake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if i > 0 && c >= 'A' && c <= 'Z' {
			prev := r[i-1]
			prevIsLowerOrDigit := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
			nextIsLower := i+1 < len(r) && r[i+1] >= 'a' && r[i+1] <= 'z'
			// An uppercase that ends an acronym and is followed only by a plural
			// "s" (end of name, or the "s" then another word) is not a new word:
			// the "s" pluralizes the acronym (IDs, CPUs), so no boundary here.
			pluralS := nextIsLower && r[i+1] == 's' && (i+2 == len(r) || (r[i+2] >= 'A' && r[i+2] <= 'Z'))
			if prevIsLowerOrDigit || (prev >= 'A' && prev <= 'Z' && nextIsLower && !pluralS) {
				b.WriteByte('_')
			}
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return b.String()
}

// goCamelCase converts a proto field name to the Go identifier protoc-gen-go
// generates for it, so the wire conversions can address the generated struct and
// its getters by their real names. It mirrors the algorithm in
// google.golang.org/protobuf/internal/strs.GoCamelCase: protoc-gen-go is not
// initialism-aware (container_id -> ContainerId, url -> Url), so reproducing it
// exactly is what lets a clean snake_case proto name coexist with a Go-idiomatic
// contract name (ContainerID) -- the conversions bridge the two.
func goCamelCase(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.' && i+1 < len(s) && isASCIILower(s[i+1]):
			// Skip over '.' in ".{{lowercase}}".
		case c == '.':
			b = append(b, '_') // convert '.' to '_'
		case c == '_' && (i == 0 || s[i-1] == '.'):
			// Convert initial '_' to ensure we start with a capital letter.
			b = append(b, 'X')
		case c == '_' && i+1 < len(s) && isASCIILower(s[i+1]):
			// Skip over '_' in "_{{lowercase}}".
		case isASCIIDigit(c):
			b = append(b, c)
		default:
			// The next word is a sequence starting with an upper-case letter,
			// followed by any lower-case letters (an acronym stays upper-case).
			if isASCIILower(c) {
				c -= 'a' - 'A'
			}
			b = append(b, c)
			for ; i+1 < len(s) && isASCIILower(s[i+1]); i++ {
				b = append(b, s[i+1])
			}
		}
	}
	return string(b)
}

func isASCIILower(c byte) bool { return 'a' <= c && c <= 'z' }
func isASCIIDigit(c byte) bool { return '0' <= c && c <= '9' }

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// singleDoc and singleField carry a DefineSinglePoint contract's cardinality
// into the generated registration, from which a host learns to reject an
// installation with two providers at startup.
func singleDoc(pt point) string {
	if !pt.isSingle {
		return ""
	}
	return "\n// Single carries the contract's cardinality: the point admits one provider."
}

func singleField(pt point) string {
	if !pt.isSingle {
		return ""
	}
	return ", Single: true"
}

// methodConst is the name of the generated constant holding a method's full
// gRPC path, and handlerName the name of its generated server handler.
func methodConst(m method) string { return "method" + m.name }
func handlerName(m method) string { return "handle" + m.name }
