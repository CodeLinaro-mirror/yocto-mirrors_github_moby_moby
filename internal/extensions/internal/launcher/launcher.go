package launcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/internal/extensions"
	"github.com/moby/moby/v2/internal/extensions/sdk"
	"github.com/moby/moby/v2/internal/extensions/sdk/sdkapi"
	sdkapipb "github.com/moby/moby/v2/internal/extensions/sdk/sdkapi/protogen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Launcher starts out-of-process extensions and describes them. Building
// providers from the connection is the caller's responsibility.
type Launcher struct {
	RuntimeDir      string
	ReadyTimeout    time.Duration
	ShutdownTimeout time.Duration
	// ExtensionConfig is each extension's configuration keyed by id. The config
	// for the launched extension (by its binary name) is delivered to it over the
	// startup handshake, so an out-of-process extension is configured by id just
	// like an in-process one.
	ExtensionConfig map[extensions.ExtensionID]extensions.Config
	// CallbackEndpoint is the unix socket the host serves launched extensions'
	// dependencies on. It is passed to each extension over the handshake so it
	// can resolve its declared dependencies. Empty when the host offers none.
	CallbackEndpoint string
}

// Launched is a started out-of-process extension: its declaration and the
// connection to it. The caller builds providers from Conn and is responsible
// for calling Close to stop the process and close the connection.
type Launched struct {
	ID           extensions.ExtensionID
	Dependencies []extensions.Dependency
	Conflicts    []extensions.ExtensionID
	Points       []LaunchedPoint
	// ProviderServices are the fully-qualified gRPC service names the extension
	// serves for each provider point on its per-extension socket. The host, not
	// the SDK, decides which point's services are also published on the daemon API
	// socket.
	ProviderServices map[extensions.PointID][]string
	Conn             grpc.ClientConnInterface
	shutdown         *processShutdown
}

// LaunchedPoint is one point an extension declared it provides.
type LaunchedPoint struct {
	ID extensions.PointID
}

// Close stops the extension process and closes the connection.
func (l *Launched) Close(ctx context.Context) error {
	return l.shutdown.Close(ctx)
}

// Initialize runs the extension's Init in its process, over the Initialize RPC.
// The host calls it in dependency order (via the broker), so the extension's
// dependencies are already up when it initializes.
func (l *Launched) Initialize(ctx context.Context) error {
	_, err := sdkapipb.NewClient(l.Conn).Initialize(ctx, &sdkapi.InitializeRequest{})
	return err
}

// Binaries lists the executable files directly under dir, each of which is an
// out-of-process extension named after its file (minus any .exe on Windows).
// Extensions live side by side in one directory rather than each in its own.
// A directory that does not exist yields none, so a missing default location is
// not an error.
//
// Discovery is a root-code-execution boundary: the daemon (often root) launches
// each binary it lists. So an entry is refused -- skipped with a warning, rather
// than failing the scan, so one bad file does not block the rest -- when it is
// world-writable (any local user could rewrite it), owned by a user other than
// root or the daemon (that owner could rewrite it), or not named like a valid
// extension id (it is not an extension; the directory is not a dumping ground
// for arbitrary executables). Broader trust (further ownership, group policy,
// symlinks) is the operator's, per the security model in the design docs.
func Binaries(ctx context.Context, dir string) ([]string, error) {
	dirInfo, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat extension dir %q: %w", dir, err)
	}
	if worldWritable(dirInfo) {
		log.G(ctx).Warnf("extensions: ignoring world-writable extension directory %q", dir)
		return nil, nil
	}
	if uid, untrusted := untrustedOwner(dirInfo); untrusted {
		log.G(ctx).Warnf("extensions: ignoring extension directory %q owned by untrusted uid %d", dir, uid)
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read extension dir %q: %w", dir, err)
	}
	var bins []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("stat extension %q: %w", filepath.Join(dir, e.Name()), err)
		}
		if !isExecutable(info) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// The file name is the extension id (see Launch), so a file whose name is
		// not a well-formed id cannot be an extension. Reject it before launching,
		// rather than executing it and failing the handshake: the directory is
		// trusted, but it is not a dumping ground -- a stray executable (a build
		// leftover, a helper tool, a script with the exec bit) must not be run as
		// the daemon just for sharing the directory.
		name := strings.TrimSuffix(e.Name(), ".exe")
		if err := extensions.ValidateExtensionID(extensions.ExtensionID(name)); err != nil {
			log.G(ctx).WithError(err).Warnf("extensions: skipping %q: not a valid extension binary name", path)
			continue
		}
		if worldWritable(info) {
			log.G(ctx).Warnf("extensions: refusing to run world-writable extension binary %q", path)
			continue
		}
		if uid, untrusted := untrustedOwner(info); untrusted {
			log.G(ctx).Warnf("extensions: refusing to run extension binary %q owned by untrusted uid %d", path, uid)
			continue
		}
		bins = append(bins, path)
	}
	return bins, nil
}

// untrustedOwner reports whether info is owned by a uid that is neither the
// superuser (0) nor the daemon's own effective user, returning that uid when so.
// A binary or directory owned by any other user could be rewritten by them and
// then executed as the daemon, so it is not trusted. This complements the
// world-writable check: that catches a file anyone can rewrite, this catches one
// a specific untrusted owner can. Ownership is not determinable on every platform
// (notably Windows, where access is governed by ACLs the mode does not reflect);
// there it is not enforced, and broader owner and group policy remains the
// operator's, per the security model in the design docs.
func untrustedOwner(info fs.FileInfo) (int, bool) {
	uid, ok := fileUID(info)
	if !ok {
		return 0, false
	}
	if uid == 0 || uid == os.Geteuid() {
		return 0, false
	}
	return uid, true
}

func isExecutable(info fs.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(info.Name()), ".exe")
	}
	return info.Mode().Perm()&0o111 != 0
}

// worldWritable reports whether info is writable by others (the o+w bit). A
// world-writable binary or directory on the daemon's exec path lets any local
// user run code as the daemon, so it is not trusted. The bit is only meaningful
// on Unix; on Windows access is governed by ACLs the mode does not reflect, so
// this check does not apply there.
func worldWritable(info fs.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	return info.Mode().Perm()&0o002 != 0
}

// Launch starts the extension binary bin, performs the stdio handshake, and
// describes it. The executable's file name (minus any .exe on Windows) is its
// extension id, which the launched extension must declare to match.
func (l Launcher) Launch(ctx context.Context, bin string) (*Launched, error) {
	readyTimeout := l.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = 5 * time.Second
	}
	shutdownTimeout := l.ShutdownTimeout
	if shutdownTimeout == 0 {
		shutdownTimeout = 5 * time.Second
	}
	if l.RuntimeDir == "" {
		return nil, errors.New("extension runtime dir is required")
	}
	if err := os.MkdirAll(l.RuntimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create extension runtime dir: %w", err)
	}
	name := strings.TrimSuffix(filepath.Base(bin), ".exe")
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("extension %q: %w", name, err)
	}
	endpoint := filepath.Join(l.RuntimeDir, name+".sock")
	if err := os.Remove(endpoint); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale extension socket: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open extension stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open extension stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open extension stderr: %w", err)
	}
	go logOutput(ctx, name, stderr)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start extension %q: %w", name, err)
	}
	// Reap the child from the moment it starts. Without a Wait in flight an
	// extension that exits on its own -- a crash, a panic, an unhandled signal --
	// stays a zombie for the life of the daemon, because nothing else waits for
	// it until shutdown. Every later stop reads this channel instead of calling
	// Wait again, since Wait may only be called once.
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	startup := sdk.StartupConfig{
		Endpoint:         endpoint,
		ProtocolVersion:  sdk.ProtocolVersion,
		Config:           l.ExtensionConfig[extensions.ExtensionID(name)],
		CallbackEndpoint: l.CallbackEndpoint,
	}
	if err := json.NewEncoder(stdin).Encode(startup); err != nil {
		_ = stopProcess(context.Background(), cmd, wait, shutdownTimeout)
		return nil, fmt.Errorf("write startup config for extension %q: %w", name, err)
	}
	_ = stdin.Close()

	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	stdoutBuf := bufio.NewReader(stdout)
	if err := waitReady(readyCtx, stdout, stdoutBuf); err != nil {
		_ = stopProcess(context.Background(), cmd, wait, shutdownTimeout)
		return nil, fmt.Errorf("wait for extension %q readiness: %w", name, err)
	}
	// Keep draining stdout for the rest of the process's life. stdout carries only
	// the one-line readiness ack; anything the extension prints there afterwards
	// is logged like stderr. Without this drain the pipe fills and the extension
	// blocks on its next write to stdout.
	go logOutput(ctx, name, stdoutBuf)

	conn, err := dial(endpoint)
	if err != nil {
		_ = stopProcess(context.Background(), cmd, wait, shutdownTimeout)
		return nil, fmt.Errorf("connect to extension %q: %w", name, err)
	}
	resp, err := sdkapipb.NewClient(conn).Describe(ctx, &sdkapi.DescribeRequest{})
	if err != nil {
		_ = conn.Close()
		_ = stopProcess(context.Background(), cmd, wait, shutdownTimeout)
		return nil, fmt.Errorf("describe extension %q: %w", name, err)
	}
	// The contract types are plain structs, so an extension that answered with no
	// declaration at all has to be rejected before its fields are read.
	decl := resp.Declaration
	if decl == nil || decl.ID == "" {
		_ = conn.Close()
		_ = stopProcess(context.Background(), cmd, wait, shutdownTimeout)
		return nil, fmt.Errorf("extension %q described no extension", name)
	}
	if decl.ID != name {
		_ = conn.Close()
		_ = stopProcess(context.Background(), cmd, wait, shutdownTimeout)
		return nil, fmt.Errorf("extension %q declared id %q, which must match its file name", name, decl.ID)
	}
	launched := &Launched{
		ID:               extensions.ExtensionID(decl.ID),
		Dependencies:     declaredDependencies(decl.Dependencies),
		Conflicts:        declaredConflicts(decl.Conflicts),
		ProviderServices: declaredServices(decl.ProviderServices),
		Conn:             conn,
		shutdown:         &processShutdown{conn: conn, cmd: cmd, wait: wait, timeout: shutdownTimeout},
	}
	for _, p := range decl.Providers {
		launched.Points = append(launched.Points, LaunchedPoint{
			ID: extensions.PointID(p.ID),
		})
	}
	// Services are only ever published for a point the extension actually
	// declared it provides. The handshake carries the two independently, so a
	// runtime that did not use this SDK could otherwise omit service.grpc from
	// its providers, list services under it anyway, and have the host publish
	// them on the API socket -- past the explicit opt-in the point exists to be.
	// Extensions are trusted code, so this is a policy bypass rather than an
	// escalation, but the policy is worth enforcing where the declaration is
	// read rather than where it is consumed.
	if err := validateDeclaredServices(name, launched); err != nil {
		_ = conn.Close()
		_ = stopProcess(context.Background(), cmd, wait, shutdownTimeout)
		return nil, err
	}
	return launched, nil
}

// validateDeclaredServices rejects services listed under a point the extension
// did not declare it provides.
func validateDeclaredServices(name string, launched *Launched) error {
	declared := make(map[extensions.PointID]bool, len(launched.Points))
	for _, p := range launched.Points {
		declared[p.ID] = true
	}
	for point := range launched.ProviderServices {
		if !declared[point] {
			return fmt.Errorf("extension %q serves services for point %q without declaring it", name, point)
		}
	}
	return nil
}

// declaredServices converts the service inventory reported by the SDK:
// service names grouped by the provider point whose ServerPoint registered them.
func declaredServices(in []sdkapi.ProviderServices) map[extensions.PointID][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[extensions.PointID][]string, len(in))
	for _, ps := range in {
		point := extensions.PointID(ps.Point)
		if point == "" || len(ps.Services) == 0 {
			continue
		}
		out[point] = append(out[point], ps.Services...)
	}
	return out
}

type processShutdown struct {
	conn    *grpc.ClientConn
	cmd     *exec.Cmd
	wait    <-chan error
	timeout time.Duration
	once    sync.Once
	err     error
}

// Close stops the extension once, however many times it is called. The broker
// stops a launched extension in reverse dependency order through its
// declaration's Shutdown, and the host stops any leftover process afterwards as
// a backstop, so the second call must be a no-op rather than a double Wait.
func (s *processShutdown) Close(ctx context.Context) error {
	s.once.Do(func() {
		s.err = errors.Join(s.conn.Close(), stopProcess(ctx, s.cmd, s.wait, s.timeout))
	})
	return s.err
}

// stopErr reports the exit of a process we deliberately stopped. An extension
// that takes the default action for the shutdown signal exits with
// "signal: terminated", and one that handles it may still exit non-zero on the
// way out; neither is a failure of the stop we asked for, and reporting them
// makes every ordinary daemon shutdown log an error. Anything else -- a Wait
// that failed for its own reasons -- is still returned.
func stopErr(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

// dial returns a lazy connection to the extension's unix socket. It does not
// block: the connection is established on the first RPC (the Describe call that
// follows), and the readiness ack already guarantees the process is listening,
// so there is nothing to wait for here.
func dial(endpoint string) (*grpc.ClientConn, error) {
	return grpc.NewClient("unix:"+endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", endpoint)
		}),
	)
}

func waitReady(ctx context.Context, stdout io.Closer, r *bufio.Reader) error {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		done <- result{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		// Unblock the reader: closing stdout makes its blocked ReadString
		// return, so the goroutine does not outlive this call.
		_ = stdout.Close()
		return ctx.Err()
	case res := <-done:
		if res.err != nil {
			return res.err
		}
		if res.line != sdk.ReadinessAck {
			return fmt.Errorf("unexpected readiness ack %q", res.line)
		}
		return nil
	}
}

func stopProcess(ctx context.Context, cmd *exec.Cmd, done <-chan error, timeout time.Duration) error {
	if cmd.Process == nil {
		return nil
	}

	// Ask the extension to stop. Signals other than Kill are not supported on
	// every platform (notably Windows), so a failed signal falls back to Kill.
	// A process we stop ourselves -- by the signal it handles, or by Kill -- is
	// a successful stop, so its exit status is not reported as an error.
	if err := cmd.Process.Signal(shutdownSignal()); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// os.ErrProcessDone means it already exited, which is the stop we
		// wanted; any other Kill error means we failed to stop it, so report it.
		if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return fmt.Errorf("kill extension after failed signal %v: %w", err, killErr)
		}
		<-done
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case err := <-done:
		return stopErr(err)
	case <-shutdownCtx.Done():
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-done
		return nil
	}
}

// logOutput logs each line the extension writes to r at info level. It is used
// for stderr, and for stdout after the readiness handshake. It reads with a
// bufio.Reader rather than a bufio.Scanner so an over-long line (a stack trace
// past the scanner's 64KB token limit, say) is still drained and logged instead
// of silently halting the pump -- which would let the pipe fill and block the
// extension's next write.
func logOutput(ctx context.Context, name string, r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			log.G(ctx).WithField("extension", name).Info(strings.TrimRight(line, "\r\n"))
		}
		if err != nil {
			return
		}
	}
}

// declaredDependencies converts declared dependencies to extension dependencies.
func declaredDependencies(deps []sdkapi.Dependency) []extensions.Dependency {
	if len(deps) == 0 {
		return nil
	}
	out := make([]extensions.Dependency, 0, len(deps))
	for _, dep := range deps {
		out = append(out, extensions.Dependency{
			Point:     extensions.PointID(dep.Point),
			Extension: extensions.ExtensionID(dep.Extension),
			Optional:  dep.Optional,
		})
	}
	return out
}

// declaredConflicts converts declared conflict ids to extension ids.
func declaredConflicts(ids []string) []extensions.ExtensionID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]extensions.ExtensionID, 0, len(ids))
	for _, id := range ids {
		out = append(out, extensions.ExtensionID(id))
	}
	return out
}
