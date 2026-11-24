package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// Who is calling? On a Unix socket the kernel knows: SO_PEERCRED gives the
// UID, GID and PID of the process that connected, and the caller cannot
// fake them. gRPC's hook for "learn something about a new connection" is
// its transport credentials. peerCreds is such a hook that encrypts
// nothing (there is no network to protect against, as for vsock) and only
// reads SO_PEERCRED; every call on that connection then carries a
// PeerInfo.

// PeerInfo is what the kernel says about the process at the other end of
// a connection.
type PeerInfo struct {
	credentials.CommonAuthInfo
	// UID, GID and PID of the process that connected.
	UID, GID uint32
	PID      int32
}

// AuthType names this kind of AuthInfo.
func (PeerInfo) AuthType() string { return "peercred" }

// peerCreds are gRPC transport credentials that read SO_PEERCRED from a
// Unix socket and change nothing else. Connections that are not Unix
// sockets are refused.
type peerCreds struct{}

// ServerHandshake reads the peer's credentials from conn.
func (peerCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, nil, fmt.Errorf("daemon: connection from %T, want a Unix socket", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: %w", err)
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, nil, fmt.Errorf("daemon: %w", err)
	}
	if credErr != nil {
		return nil, nil, fmt.Errorf("daemon: SO_PEERCRED: %w", credErr)
	}
	return conn, PeerInfo{
		CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity},
		UID:            cred.Uid, GID: cred.Gid, PID: cred.Pid,
	}, nil
}

// ClientHandshake is never used: these credentials are for the server.
func (peerCreds) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("daemon: peer credentials are for the server side only")
}

// Info describes the credentials.
func (peerCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "peercred"}
}

// Clone returns a copy (there is nothing to copy).
func (peerCreds) Clone() credentials.TransportCredentials { return peerCreds{} }

// OverrideServerName does nothing; gRPC requires the method.
func (peerCreds) OverrideServerName(string) error { return nil }

// callerOf returns the credentials of the process that made the call in
// ctx.
func callerOf(ctx context.Context) (PeerInfo, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return PeerInfo{}, false
	}
	info, ok := p.AuthInfo.(PeerInfo)
	return info, ok
}
