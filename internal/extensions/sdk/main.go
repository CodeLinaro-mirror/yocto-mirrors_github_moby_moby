package sdk

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/moby/moby/v2/internal/extensions"
	"github.com/moby/moby/v2/internal/extensions/serverpoint"
)

// Main runs ext as a standalone out-of-process extension and does not return:
// it is the whole body of an extension binary's main. It handles the signals
// the host stops an extension with, registers ext with the server registration
// for each point it provides, serves until stopped, and exits non-zero with the
// reason on failure.
//
//	func main() {
//		sdk.Main(myext.Extension, createspecpb.ServerPoint)
//	}
//
// A binary that needs more than this -- declaring a dependency point with
// [Server.Depends], say -- builds a [Server] itself; Main is only the common
// case, not a required entry point.
func Main(ext extensions.Extension, points ...serverpoint.Registration) {
	if err := serve(ext, points); err != nil {
		// stdout is reserved for the runtime handshake; the host captures stderr
		// and folds it into its own logs.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// serve is Main's body, split out so the signal handler is unregistered on the
// way out rather than skipped by os.Exit.
func serve(ext extensions.Extension, points []serverpoint.Registration) error {
	// The host stops an extension with SIGTERM on unix; cancelling ctx on it
	// lets the SDK shut the extension down gracefully and exit zero. On Windows
	// the host kills the process instead, so nothing is delivered there.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := NewServer()
	if err := srv.Register(ext, points...); err != nil {
		return err
	}
	return srv.Listen(ctx)
}
