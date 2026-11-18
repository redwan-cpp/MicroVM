package agent

// Port is the vsock port sandbox-agent listens on inside the guest. The
// host connects to it through Firecracker's vsock Unix socket with
// "CONNECT 1024" (see internal/vsockconn). Both sides use this one
// constant, so they can never disagree.
//
// Ports below 1024 are "privileged" on vsock as on TCP: only root may
// listen on them. 1024 is the first ordinary port. That does not make the
// port safe: guest code runs as root and can stop the agent and listen on
// 1024 itself. This is why the host never trusts what it hears on this
// port (docs/threat-model.md, trust boundary 3).
const Port uint32 = 1024
