// Package agent is the service inside every microVM: the code behind the
// sandbox-agent program (cmd/sandbox-agent). It implements the AgentService
// gRPC API from proto/sandbox/agent/v1/agent.proto: Health, Exec, SyncTime,
// Reseed and MountScratch.
//
// The agent runs as root inside the guest, next to the untrusted code it
// starts. It does its best to keep that code inside the limits (timeout,
// output cap, killing leftover processes), but it is not a security
// boundary: the code it runs can kill it or replace it. The boundary is the
// VM itself, and the host checks every limit again on its own side
// (internal/agentclient).
package agent

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
)

// Config holds what a Server needs from the outside world. The zero value
// is fine for most fields.
type Config struct {
	// Version is reported by Health, for example the output of
	// version.String("sandbox-agent").
	Version string
	// Clock sets the guest clock for SyncTime. nil means SystemClock.
	Clock Clock
	// Entropy feeds the guest RNG for Reseed. nil means KernelEntropy.
	Entropy Entropy
	// Scratch mounts the scratch disk for MountScratch. nil means
	// KernelScratch.
	Scratch Scratch
}

// Server implements agentv1.AgentServiceServer. Create it with NewServer
// and register it on a grpc.Server.
type Server struct {
	// Embedding the generated "Unimplemented" type is required by gRPC: if
	// the .proto file gains a new RPC, the server still compiles and
	// answers that RPC with codes.Unimplemented.
	agentv1.UnimplementedAgentServiceServer

	version string
	clock   Clock
	entropy Entropy
	scratch Scratch
	started time.Time

	mu          sync.Mutex
	scratchSize uint64 // the mounted scratch disk's size, 0 before MountScratch
}

// NewServer returns a Server that uses cfg.
func NewServer(cfg Config) *Server {
	s := &Server{
		version: cfg.Version,
		clock:   cfg.Clock,
		entropy: cfg.Entropy,
		scratch: cfg.Scratch,
		started: time.Now(),
	}
	if s.clock == nil {
		s.clock = SystemClock{}
	}
	if s.entropy == nil {
		s.entropy = KernelEntropy{}
	}
	if s.scratch == nil {
		s.scratch = KernelScratch{}
	}
	return s
}

// Health reports the agent's version and how long it has been running.
func (s *Server) Health(context.Context, *agentv1.HealthRequest) (*agentv1.HealthResponse, error) {
	return &agentv1.HealthResponse{
		Version:  s.version,
		UptimeMs: uint64(max(time.Since(s.started).Milliseconds(), 0)),
	}, nil
}

// SyncTime sets the guest clock to the host's time.
func (s *Server) SyncTime(_ context.Context, req *agentv1.SyncTimeRequest) (*agentv1.SyncTimeResponse, error) {
	if req.GetUnixNanos() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "unix_nanos must be a positive number of nanoseconds since 1970")
	}
	if err := s.clock.SetTime(time.Unix(0, req.GetUnixNanos())); err != nil {
		return nil, status.Errorf(codes.Internal, "set the clock: %v", err)
	}
	return &agentv1.SyncTimeResponse{}, nil
}

// Reseed mixes the host's random bytes into the guest's RNG.
func (s *Server) Reseed(_ context.Context, req *agentv1.ReseedRequest) (*agentv1.ReseedResponse, error) {
	if n := len(req.GetEntropy()); n < MinEntropy || n > MaxEntropy {
		return nil, status.Errorf(codes.InvalidArgument,
			"entropy is %d bytes; it must be %d to %d bytes", n, MinEntropy, MaxEntropy)
	}
	if err := s.entropy.AddEntropy(req.GetEntropy()); err != nil {
		return nil, status.Errorf(codes.Internal, "reseed the RNG: %v", err)
	}
	return &agentv1.ReseedResponse{}, nil
}
