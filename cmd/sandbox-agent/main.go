// Command sandbox-agent is the assistant that lives inside each microVM.
// It listens on the vsock "intercom", runs the commands sandboxd sends it,
// and streams their output back. The service itself is in internal/agent.
//
// The guest's init script (guest/init/demo-init) starts it when the kernel
// command line says sandbox_mode=agent. It runs until it is stopped or the
// VM is destroyed.
//
// It is built as a static binary (CGO_ENABLED=0 in the Makefile), so it runs
// inside the tiny guest without needing any other files.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"

	"github.com/redwan-cpp/MicroVM/internal/agent"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/version"
)

// maxConcurrentStreams caps how many calls (Exec streams included) the
// agent serves at the same time over one connection.
const maxConcurrentStreams = 16

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("sandbox-agent"))
		return
	}

	// Log lines go to stderr, which in the guest is the serial console.
	log.SetFlags(0)
	log.SetPrefix("sandbox-agent: ")
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

// run serves the agent API on vsock until SIGINT or SIGTERM arrives.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// AF_VSOCK is the socket family for VM <-> host connections. Firecracker
	// carries them to a Unix socket on the host (see internal/vsockconn).
	lis, err := vsock.Listen(agent.Port, nil)
	if err != nil {
		return fmt.Errorf("listen on vsock port %d: %w", agent.Port, err)
	}

	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(agent.MaxRequestSize),
		grpc.MaxConcurrentStreams(maxConcurrentStreams),
	)
	agentv1.RegisterAgentServiceServer(srv, agent.NewServer(agent.Config{
		Version: version.String("sandbox-agent"),
	}))

	log.Printf("%s listening on vsock port %d", version.String("sandbox-agent"), agent.Port)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }() // Serve closes lis when it returns

	select {
	case err := <-served:
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Print("stopping")
		stopWithin(srv, 2*time.Second)
		return nil
	}
}

// stopWithin lets running calls finish for up to d, then cuts them off.
func stopWithin(srv *grpc.Server, d time.Duration) {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		srv.Stop()
	}
}
