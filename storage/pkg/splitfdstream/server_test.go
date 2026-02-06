//go:build linux

package splitfdstream

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockBaseDriver is a minimal mock that does NOT implement SplitFDStreamDriver.
// Used to test driver compatibility checking via interface type assertions.
type mockBaseDriver struct{}

func (m *mockBaseDriver) String() string { return "mock-base" }

// MockDriver implements SplitFDStreamDriver for testing.
type MockDriver struct{}

func (m *MockDriver) String() string { return "mock" }

func (m *MockDriver) ApplySplitFDStream(options *ApplySplitFDStreamOpts) (int64, error) {
	return 0, nil
}
func (m *MockDriver) GetSplitFDStream(id, parent string, options *GetSplitFDStreamOpts) (io.ReadCloser, []*os.File, error) {
	return nil, nil, nil
}

func TestSplitFDStreamServer_StartStop(t *testing.T) {
	mockDriver := &MockDriver{}
	server := NewJSONRPCServer(mockDriver, nil)

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Start server
	err := server.Start(socketPath)
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	if !server.IsRunning() {
		t.Error("Server should be running")
	}

	if server.GetSocketPath() != socketPath {
		t.Errorf("Expected socket path %s, got %s", socketPath, server.GetSocketPath())
	}

	// Stop server
	err = server.Stop()
	if err != nil {
		t.Fatalf("Failed to stop server: %v", err)
	}

	if server.IsRunning() {
		t.Error("Server should not be running")
	}
}

func TestSplitFDStreamServer_DoubleStart(t *testing.T) {
	mockDriver := &MockDriver{}
	server := NewJSONRPCServer(mockDriver, nil)

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Start server
	err := server.Start(socketPath)
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()

	// Try to start again
	err = server.Start(socketPath)
	if err == nil {
		t.Error("Expected error when starting server twice")
	}
}

func TestSplitFDStreamServer_PingRequest(t *testing.T) {
	mockDriver := &MockDriver{}
	server := NewJSONRPCServer(mockDriver, nil)

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Start server
	err := server.Start(socketPath)
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()

	// Give server time to start listening
	time.Sleep(100 * time.Millisecond)

	// Create client
	client, err := NewJSONRPCClient(socketPath)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	// Send ping request
	resp, err := client.Ping("hello")
	if err != nil {
		t.Fatalf("Failed to ping server: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("Unexpected error in response: %v", resp.Error)
	}

	if resp.Result == nil {
		t.Fatal("Expected result in response")
	}

	if resp.Result.Message != "pong: hello" {
		t.Errorf("Expected 'pong: hello', got %s", resp.Result.Message)
	}
}

func TestSplitFDStreamServer_UnsupportedDriver(t *testing.T) {
	// Use mockBaseDriver which does NOT implement SplitFDStreamDriver
	baseDriver := &mockBaseDriver{}
	server := NewJSONRPCServer(baseDriver, nil)

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Start server
	err := server.Start(socketPath)
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()

	// Give server time to start listening
	time.Sleep(100 * time.Millisecond)

	// Create client
	client, err := NewJSONRPCClient(socketPath)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	// Send ping request - should still succeed even for unsupported driver
	// (ping is a health check and doesn't require driver support)
	resp, err := client.Ping("hello")
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("Ping should succeed even for unsupported driver, got error: %v", resp.Error)
	}

	// Send an apply request - should fail because driver doesn't support splitfdstream
	applyReq := NewRequest(MethodApplySplitFDStream, SplitFDStreamParams{LayerID: "test-layer"}, "apply-1")
	reqBytes, err := json.Marshal(applyReq)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	applyResp, err := client.SendRawRequest(string(reqBytes))
	if err != nil {
		t.Fatalf("Failed to send apply request: %v", err)
	}

	if applyResp.Error == nil {
		t.Error("Expected error response for unsupported driver on apply request")
	} else if applyResp.Error.Code != ErrorCodeDriverNotSupported {
		t.Errorf("Expected error code %d, got %d", ErrorCodeDriverNotSupported, applyResp.Error.Code)
	}
}

func TestSplitFDStreamServer_InvalidRequest(t *testing.T) {
	mockDriver := &MockDriver{}
	server := NewJSONRPCServer(mockDriver, nil)

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Start server
	err := server.Start(socketPath)
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()

	// Give server time to start listening
	time.Sleep(100 * time.Millisecond)

	// Create client connection manually to send invalid JSON
	client, err := NewJSONRPCClient(socketPath)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	// Send invalid JSON request
	err = client.fdPasser.SendMessage([]byte("{invalid json}\n"))
	if err != nil {
		t.Fatalf("Failed to send invalid request: %v", err)
	}

	// Read response
	respData, err := client.fdPasser.ReceiveMessage(1024)
	if err != nil {
		t.Fatalf("Failed to receive response: %v", err)
	}

	// Parse response
	var resp SplitFDStreamResponse
	err = json.Unmarshal(respData, &resp)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Error == nil {
		t.Error("Expected error response for invalid JSON")
	}

	if resp.Error.Code != ErrorCodeParseError {
		t.Errorf("Expected error code %d, got %d", ErrorCodeParseError, resp.Error.Code)
	}
}
