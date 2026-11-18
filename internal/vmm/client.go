// Package vmm starts Firecracker and drives it through its REST API: the
// same steps as scripts/boot-vm.sh, written in Go for sandboxd and the
// tests. (VMM stands for "virtual machine monitor", the program that runs
// a VM; Firecracker is one.)
//
// Firecracker listens for HTTP requests on a Unix socket (its "API
// socket"). A handful of PUT requests describe the machine, and a final
// PUT /actions {"action_type": "InstanceStart"} powers it on. A running VM
// can be paused and saved as a snapshot (PUT /snapshot/create), and a
// fresh Firecracker can load that snapshot instead of booting
// (PUT /snapshot/load). The full API is in
// third_party/firecracker/firecracker_spec.yaml after `make firecracker`;
// docs/learn/06-firecracker-api.md explains the boot calls and
// docs/learn/09-snapshots-and-cow.md the snapshot ones.
//
// We write this small client ourselves instead of using firecracker-go-sdk,
// so every call stays visible (ADR-0001).
package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds every API call whose context has no deadline.
	DefaultTimeout = 5 * time.Second

	// maxResponseBody is the most of a response body we read. Firecracker's
	// answers are tiny; this only guards against surprises.
	maxResponseBody = 64 << 10
)

// MachineConfig is the body of PUT /machine-config: the VM's size.
type MachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

// BootSource is the body of PUT /boot-source: the guest kernel and its
// command line.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

// Drive is the body of PUT /drives/{drive_id}: a disk for the guest.
// Firecracker shows the root device to the guest as /dev/vda.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// Vsock is the body of PUT /vsock: the VM's vsock device. GuestCID is the
// guest's vsock address (3 or more; 0-2 are reserved). Firecracker creates
// a Unix socket at UDSPath for host-to-guest connections (see
// internal/vsockconn).
type Vsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// SnapshotCreate is the body of PUT /snapshot/create. Both paths are
// relative to Firecracker's working directory (its run folder), like every
// path we give Firecracker (ADR-0003, ADR-0005).
type SnapshotCreate struct {
	// SnapshotType is "Full" (all of guest memory) or "Diff" (only the
	// pages written since the last snapshot). We always make Full ones.
	SnapshotType string `json:"snapshot_type"`
	// SnapshotPath receives the VM state: vCPU registers, device state and
	// the device configuration, including every path given to Firecracker.
	SnapshotPath string `json:"snapshot_path"`
	// MemFilePath receives a copy of all of guest memory.
	MemFilePath string `json:"mem_file_path"`
}

// SnapshotLoad is the body of PUT /snapshot/load. Firecracker accepts it
// only as the first request to a fresh process: the snapshot replaces
// PutMachineConfig, PutBootSource, PutDrive and PutVsock.
type SnapshotLoad struct {
	// SnapshotPath is the VM state file written by CreateSnapshot.
	SnapshotPath string `json:"snapshot_path"`
	// MemBackend says where guest memory comes from.
	MemBackend MemoryBackend `json:"mem_backend"`
	// ResumeVM starts the vCPUs right after loading. Without it the VM
	// stays paused until Resume.
	ResumeVM bool `json:"resume_vm,omitempty"`
	// ClockRealtime (x86_64 only) moves the guest's kvmclock forward by the
	// wall-clock time since the snapshot was taken. We leave it off: the
	// agent sets the guest clock after every restore (SyncTime), and
	// docs/learn/09-snapshots-and-cow.md explains the difference.
	ClockRealtime bool `json:"clock_realtime,omitempty"`
}

// MemoryBackend is where a restored VM's memory comes from.
type MemoryBackend struct {
	// BackendType is "File": Firecracker maps the memory file privately
	// (copy-on-write), so every clone shares its pages until it writes
	// to them. ("Uffd" hands page faults to another process instead.)
	BackendType string `json:"backend_type"`
	// BackendPath is the memory file, relative to the working directory.
	BackendPath string `json:"backend_path"`
}

// APIError is a non-2xx answer from Firecracker.
type APIError struct {
	Method, Path string
	StatusCode   int
	// FaultMessage is Firecracker's explanation, from the JSON body
	// {"fault_message": "..."}, or the raw body if it was not JSON.
	FaultMessage string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("firecracker: %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.FaultMessage)
}

// Client sends requests to one Firecracker API socket.
type Client struct {
	http *http.Client
}

// NewClient returns a Client for the API socket at socketPath.
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		// Every request goes to the Unix socket, whatever host name the URL
		// has. Proxy is left nil: never send these requests to a proxy.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	return &Client{http: &http.Client{Transport: transport}}
}

// PutMachineConfig sets the number of vCPUs and the memory size.
func (c *Client) PutMachineConfig(ctx context.Context, mc MachineConfig) error {
	return c.put(ctx, "/machine-config", mc)
}

// PutBootSource sets the guest kernel and its command line.
func (c *Client) PutBootSource(ctx context.Context, bs BootSource) error {
	return c.put(ctx, "/boot-source", bs)
}

// PutDrive adds (or replaces) the drive d.DriveID.
func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	if d.DriveID == "" {
		return fmt.Errorf("firecracker: PUT /drives: empty drive ID")
	}
	return c.put(ctx, "/drives/"+url.PathEscape(d.DriveID), d)
}

// PutVsock adds the vsock device.
func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.put(ctx, "/vsock", v)
}

// Start powers the VM on (InstanceStart). The guest kernel starts booting
// right away; Start returns as soon as Firecracker has accepted the
// request, long before the guest is ready.
func (c *Client) Start(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "InstanceStart"})
}

// Pause stops the VM's vCPUs (PATCH /vm {"state": "Paused"}). Guest time
// stands still until Resume. A VM must be paused for CreateSnapshot.
func (c *Client) Pause(ctx context.Context) error {
	return c.send(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

// Resume starts the vCPUs of a paused VM again.
func (c *Client) Resume(ctx context.Context) error {
	return c.send(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// CreateSnapshot saves the paused VM to two files (see SnapshotCreate).
// SnapshotType defaults to "Full". It returns once both files are written
// and synced to disk, which takes about as long as writing guest memory;
// give ctx a deadline that allows for that.
func (c *Client) CreateSnapshot(ctx context.Context, s SnapshotCreate) error {
	if s.SnapshotType == "" {
		s.SnapshotType = "Full"
	}
	return c.put(ctx, "/snapshot/create", s)
}

// LoadSnapshot turns a fresh Firecracker into a copy of the snapshotted VM
// (see SnapshotLoad).
func (c *Client) LoadSnapshot(ctx context.Context, s SnapshotLoad) error {
	return c.put(ctx, "/snapshot/load", s)
}

// put sends one PUT request with body as JSON and checks the answer.
func (c *Client) put(ctx context.Context, path string, body any) error {
	return c.send(ctx, http.MethodPut, path, body)
}

// send sends one request with body as JSON and checks the answer.
func (c *Client) send(ctx context.Context, method, path string, body any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("firecracker: %s %s: encode body: %w", method, path, err)
	}
	// The host name in the URL is ignored (the dialer always uses the Unix
	// socket), but HTTP needs one.
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("firecracker: %s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing useful to do with a close error here

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("firecracker: %s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{
			Method:       method,
			Path:         path,
			StatusCode:   resp.StatusCode,
			FaultMessage: faultMessage(respBody),
		}
	}
	return nil
}

// faultMessage extracts Firecracker's error text from a response body.
func faultMessage(body []byte) string {
	var fault struct {
		FaultMessage string `json:"fault_message"`
	}
	if err := json.Unmarshal(body, &fault); err == nil && fault.FaultMessage != "" {
		return fault.FaultMessage
	}
	return strings.TrimSpace(string(body))
}
