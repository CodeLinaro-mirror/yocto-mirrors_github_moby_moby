// Command greeter serves the greeter fixture as an out-of-process extension
// that opts into socket exposure, for the integration test.
package main

import (
	servicegrpcv0 "github.com/moby/moby/v2/extpoints/servicegrpc/v0"
	"github.com/moby/moby/v2/integration/extension/testdata/greeter"
	"github.com/moby/moby/v2/internal/extensions/sdk"
)

func main() {
	// The SDK serves all provider gRPC services on the extension socket. Because
	// this provider is service.grpc, the daemon publishes those service names on
	// the API socket.
	sdk.Main(greeter.Extension, servicegrpcv0.ServerPoint)
}
