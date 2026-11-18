package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// unixServer runs handler as an HTTP server on a Unix socket, like
// Firecracker's API, and returns a Client for it.
func unixServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	_ = srv.Listener.Close() // replace the default TCP listener
	srv.Listener = lis
	srv.Start()
	t.Cleanup(srv.Close)
	return NewClient(path)
}

type request struct {
	method, path, contentType string
	body                      map[string]any
}

func TestClientSendsTheRightRequests(t *testing.T) {
	var got []request
	c := unixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("%s %s: body is not JSON: %v", r.Method, r.URL.Path, err)
		}
		got = append(got, request{r.Method, r.URL.Path, r.Header.Get("Content-Type"), body})
		w.WriteHeader(http.StatusNoContent) // Firecracker's usual answer
	})
	ctx := t.Context()

	steps := []error{
		c.PutMachineConfig(ctx, MachineConfig{VCPUCount: 1, MemSizeMiB: 256}),
		c.PutBootSource(ctx, BootSource{KernelImagePath: "/k/vmlinux", BootArgs: "console=ttyS0"}),
		c.PutDrive(ctx, Drive{DriveID: "rootfs", PathOnHost: "/k/rootfs.ext4", IsRootDevice: true, IsReadOnly: true}),
		c.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: "/run/v.sock"}),
		c.Start(ctx),
	}
	for i, err := range steps {
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}

	want := []request{
		{"PUT", "/machine-config", "application/json", map[string]any{"vcpu_count": 1.0, "mem_size_mib": 256.0}},
		{"PUT", "/boot-source", "application/json", map[string]any{"kernel_image_path": "/k/vmlinux", "boot_args": "console=ttyS0"}},
		{"PUT", "/drives/rootfs", "application/json", map[string]any{
			"drive_id": "rootfs", "path_on_host": "/k/rootfs.ext4", "is_root_device": true, "is_read_only": true,
		}},
		{"PUT", "/vsock", "application/json", map[string]any{"guest_cid": 3.0, "uds_path": "/run/v.sock"}},
		{"PUT", "/actions", "application/json", map[string]any{"action_type": "InstanceStart"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("requests:\n got  %v\n want %v", got, want)
	}
}

func TestClientDecodesFaultMessage(t *testing.T) {
	c := unixServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"fault_message": "Invalid kernel path"}`)
	})

	err := c.PutBootSource(t.Context(), BootSource{KernelImagePath: "/nope"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if apiErr.StatusCode != 400 || apiErr.FaultMessage != "Invalid kernel path" || apiErr.Path != "/boot-source" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestClientReadsAtMost64KiBOfABody(t *testing.T) {
	c := unixServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", 1<<20)) // 1 MiB, not JSON
	})

	err := c.Start(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if n := len(apiErr.FaultMessage); n != maxResponseBody {
		t.Errorf("FaultMessage is %d bytes, want the first %d", n, maxResponseBody)
	}
}

func TestClientHonoursTheContext(t *testing.T) {
	c := unixServer(t, func(_ http.ResponseWriter, r *http.Request) {
		// Never answer. (The body must be read first: only then does the
		// server notice the client hanging up and cancel r.Context.)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Start(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v, want about 100ms", took)
	}
}

func TestClientRejectsEmptyDriveID(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "unused.sock"))
	if err := c.PutDrive(t.Context(), Drive{PathOnHost: "/x"}); err == nil {
		t.Error("PutDrive with no drive ID succeeded, want an error")
	}
}
