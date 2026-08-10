//go:generate go run github.com/moby/moby/v2/internal/extensions/cmd/mobyextgen

// Package echov1 is a minimal extension point used only by the launcher
// end-to-end test, to exercise the launcher against a point that is not any
// real one. It is written Go-first like the real points: the [EchoServer]
// interface and its messages in echo.go are the source of truth, and mobyextgen
// generates the .proto and, into the protogen subpackage, the protobuf and
// transport code. Regenerate with
// `go generate ./internal/extensions/internal/launcher/echo/v1/`.
package echov1
