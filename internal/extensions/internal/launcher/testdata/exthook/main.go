// Command exthook is a minimal out-of-process extension used by the launcher
// end-to-end test. It provides the echo test point and echoes the request back
// (erroring on an empty message), so the test can assert both the success and
// veto paths across the real stdio handshake and gRPC connection.
package main

import (
	"context"
	"errors"

	"github.com/moby/moby/v2/internal/extensions"
	echov1 "github.com/moby/moby/v2/internal/extensions/internal/launcher/echo/v1"
	echopb "github.com/moby/moby/v2/internal/extensions/internal/launcher/echo/v1/protogen"
	"github.com/moby/moby/v2/internal/extensions/sdk"
)

type echo struct{}

func (echo) Echo(_ context.Context, req *echov1.EchoRequest) (*echov1.EchoResponse, error) {
	if req.Message == "" {
		return nil, errors.New("message must not be empty")
	}
	return &echov1.EchoResponse{Message: req.Message}, nil
}

func main() {
	ext := extensions.New(extensions.Declaration{
		ID:        "org.example.exthook.v1",
		Providers: []extensions.Provider{echov1.Point.Provide(echo{})},
	})
	// [sdk.Main] handles the SIGTERM the launcher stops extensions with on unix,
	// so the process shuts down gracefully and exits zero -- which is what the
	// launcher test asserts. On Windows the launcher kills the process instead.
	sdk.Main(ext, echopb.ServerPoint)
}
