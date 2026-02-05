//go:build !linux

package splitfdstream

import (
	"net"
	"os"
)

// JSONRPCServer manages the JSON-RPC server for storage operations.
type JSONRPCServer struct{}

// NewJSONRPCServer creates a new JSON-RPC server.
func NewJSONRPCServer(driver any) *JSONRPCServer {
	return &JSONRPCServer{}
}

// HandleConnection handles a single client connection directly.
func (s *JSONRPCServer) HandleConnection(conn *net.UnixConn) {
}

// Start starts the JSON-RPC server on the specified UNIX socket path.
func (s *JSONRPCServer) Start(socketPath string) error {
	return errUnsupported
}

// Stop stops the JSON-RPC server and cleans up resources.
func (s *JSONRPCServer) Stop() error {
	return nil
}

// IsRunning returns true if the server is currently running.
func (s *JSONRPCServer) IsRunning() bool {
	return false
}

// GetSocketPath returns the path to the UNIX socket.
func (s *JSONRPCServer) GetSocketPath() string {
	return ""
}

// JSONRPCClient provides a client for communicating with the JSON-RPC server.
type JSONRPCClient struct{}

// NewJSONRPCClient creates a new client connected to the specified UNIX socket.
func NewJSONRPCClient(socketPath string) (*JSONRPCClient, error) {
	return nil, errUnsupported
}

// Ping sends a ping request to the server.
func (c *JSONRPCClient) Ping(message string) (*SplitFDStreamResponse, error) {
	return nil, errUnsupported
}

// SendRawRequest sends a raw JSON-RPC request string and returns the response.
func (c *JSONRPCClient) SendRawRequest(requestLine string) (*SplitFDStreamResponse, error) {
	return nil, errUnsupported
}

// Close closes the client connection.
func (c *JSONRPCClient) Close() error {
	return nil
}

// ApplySplitFDStream sends an apply_splitfdstream request to the server.
func (c *JSONRPCClient) ApplySplitFDStream(layerID string, streamData []byte, fds []*os.File, ignoreChownErrors bool, mountLabel string) (int64, error) {
	return 0, errUnsupported
}

// GetSplitFDStream sends a get_splitfdstream request to the server and receives the stream data.
func (c *JSONRPCClient) GetSplitFDStream(layerID, parentID string) ([]byte, []*os.File, error) {
	return nil, nil, errUnsupported
}
