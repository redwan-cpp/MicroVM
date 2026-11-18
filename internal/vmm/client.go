// Package vmm starts Firecracker and drives it through its REST API: the
// same steps as scripts/boot-vm.sh, written in Go for sandboxd and the
// tests. (VMM stands for "virtual machine monitor", the program that runs
// a VM; Firecracker is one.)
//
// Firecracker listens for HTTP requests on a Unix socket (its "API
// socket"). A handful of PUT requests describe the machine, and a final
// PUT /actions {"action_type": "InstanceStart"} powers it on. The full API
// is in third_party/firecracker/firecracker_spec.yaml after
// `make firecracker`; docs/learn/06-firecracker-api.md explains the calls.
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

// put sends one PUT request with body as JSON and checks the answer.
func (c *Client) put(ctx context.Context, path string, body any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("firecracker: PUT %s: encode body: %w", path, err)
	}
	// The host name in the URL is ignored (the dialer always uses the Unix
	// socket), but HTTP needs one.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("firecracker: PUT %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker: PUT %s: %w", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing useful to do with a close error here

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("firecracker: PUT %s: read response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{
			Method:       http.MethodPut,
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
