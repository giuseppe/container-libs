//go:build linux

package splitfdstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// limitedSocketReader reads a fixed amount of data from a socket,
// optionally starting with some initial data already received.
type limitedSocketReader struct {
	fdPasser    *FDPasser
	remaining   int64
	initialData []byte
	initialPos  int
}

// newLimitedSocketReader creates a new reader that reads exactly 'size' bytes
// from the socket, using initialData first if provided.
func newLimitedSocketReader(fdPasser *FDPasser, size int64, initialData []byte) *limitedSocketReader {
	return &limitedSocketReader{
		fdPasser:    fdPasser,
		remaining:   size,
		initialData: initialData,
		initialPos:  0,
	}
}

// Read implements io.Reader for limitedSocketReader.
func (r *limitedSocketReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}

	// First, consume any remaining initial data
	if r.initialPos < len(r.initialData) {
		n := copy(p, r.initialData[r.initialPos:])
		r.initialPos += n
		r.remaining -= int64(n)
		return n, nil
	}

	// Read from socket
	toRead := len(p)
	if int64(toRead) > r.remaining {
		toRead = int(r.remaining)
	}

	data, err := r.fdPasser.ReceiveMessage(toRead)
	if err != nil {
		return 0, err
	}

	n := copy(p, data)
	r.remaining -= int64(n)

	if r.remaining <= 0 {
		return n, io.EOF
	}
	return n, nil
}

// JSONRPCServer manages the JSON-RPC server for storage operations.
type JSONRPCServer struct {
	driver      any
	listener    net.Listener
	socketPath  string
	shutdown    chan struct{}
	connections sync.WaitGroup
	mu          sync.RWMutex
	running     bool
}

// NewJSONRPCServer creates a new JSON-RPC server.
func NewJSONRPCServer(driver any) *JSONRPCServer {
	return &JSONRPCServer{
		driver:   driver,
		shutdown: make(chan struct{}),
	}
}

// HandleConnection handles a single client connection directly.
// This method is used when a socket pair is created and one end
// is passed to the server for handling.
func (s *JSONRPCServer) HandleConnection(conn *net.UnixConn) {
	s.connections.Add(1)
	go s.handleConnectionInternal(conn)
}

// Start starts the JSON-RPC server on the specified UNIX socket path.
func (s *JSONRPCServer) Start(socketPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("server is already running")
	}

	// Remove existing socket file if it exists
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to create UNIX socket listener: %w", err)
	}

	// Create new shutdown channel for this start session
	s.shutdown = make(chan struct{})
	s.listener = listener
	s.socketPath = socketPath
	s.running = true

	logrus.Debugf("splitfdstream: JSON-RPC server listening on %s", socketPath)

	go s.serve()
	return nil
}

// Stop stops the JSON-RPC server and cleans up resources.
func (s *JSONRPCServer) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	logrus.Debug("splitfdstream: stopping JSON-RPC server")

	// Signal shutdown
	close(s.shutdown)

	// Close listener
	if s.listener != nil {
		s.listener.Close()
	}

	// Wait for all connections to finish
	s.connections.Wait()

	// Clean up socket file
	if s.socketPath != "" {
		os.Remove(s.socketPath)
	}

	s.running = false
	logrus.Debug("splitfdstream: JSON-RPC server stopped")

	return nil
}

// serve is the main server loop that accepts connections.
func (s *JSONRPCServer) serve() {
	defer s.connections.Wait()

	for {
		select {
		case <-s.shutdown:
			return
		default:
		}

		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return
			default:
				logrus.Errorf("splitfdstream: failed to accept connection: %v", err)
				continue
			}
		}

		s.connections.Add(1)
		go s.handleConnectionInternal(conn)
	}
}

// handleConnectionInternal handles a single client connection.
func (s *JSONRPCServer) handleConnectionInternal(conn net.Conn) {
	defer s.connections.Done()
	defer conn.Close()

	logrus.Debug("splitfdstream: new client connection")

	// Convert to unix connection for file descriptor passing
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		logrus.Error("splitfdstream: connection is not a unix socket")
		return
	}

	fdPasser := NewFDPasser(unixConn)
	defer fdPasser.Close()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		select {
		case <-s.shutdown:
			return
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		s.handleRequest(fdPasser, line)
	}

	if err := scanner.Err(); err != nil {
		logrus.Errorf("splitfdstream: error reading from connection: %v", err)
	}

	logrus.Debug("splitfdstream: client connection closed")
}

// handleRequest processes a single JSON-RPC request.
func (s *JSONRPCServer) handleRequest(fdPasser *FDPasser, requestLine string) {
	// Parse the JSON-RPC request
	req, err := ParseRequest([]byte(requestLine))
	if err != nil {
		// Send error response
		resp := NewErrorResponse(ErrorCodeParseError, err.Error(), nil, nil)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Validate the request
	if err := req.IsValid(); err != nil {
		resp := NewErrorResponse(ErrorCodeInvalidRequest, err.Error(), nil, req.ID)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Check driver support using interface type assertion
	if req.Method == MethodApplySplitFDStream || req.Method == MethodGetSplitFDStream {
		if _, ok := s.driver.(SplitFDStreamDriver); !ok {
			resp := NewErrorResponse(
				ErrorCodeDriverNotSupported,
				fmt.Sprintf("driver %s does not support splitfdstream operations", fmt.Sprint(s.driver)),
				nil,
				req.ID,
			)
			s.sendResponse(fdPasser, resp)
			return
		}
	}

	// Dispatch the request based on method
	switch req.Method {
	case MethodPing:
		s.handlePing(fdPasser, req)
	case MethodApplySplitFDStream:
		s.handleApplySplitFDStream(fdPasser, req)
	case MethodGetSplitFDStream:
		s.handleGetSplitFDStream(fdPasser, req)
	default:
		resp := NewErrorResponse(ErrorCodeMethodNotFound, "Method not found", req.Method, req.ID)
		s.sendResponse(fdPasser, resp)
	}
}

// handlePing handles ping requests.
func (s *JSONRPCServer) handlePing(fdPasser *FDPasser, req *SplitFDStreamRequest) {
	pingParams, err := ParsePingParams(req.Params)
	if err != nil {
		resp := NewErrorResponse(ErrorCodeInvalidParams, err.Error(), nil, req.ID)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Simple ping response
	result := &SplitFDStreamResult{
		Message: fmt.Sprintf("pong: %s", pingParams.Message),
	}

	resp := NewSuccessResponse(result, req.ID)
	s.sendResponse(fdPasser, resp)
}

// handleApplySplitFDStream handles apply_splitfdstream requests.
// Protocol:
// 1. Client sends JSON-RPC request with layerId, numFileDescriptors, streamSize
// 2. Server sends ACK response indicating ready to receive
// 3. Client sends FDs (if numFileDescriptors > 0) along with first chunk of data
// 4. Client sends remaining stream data
// 5. Server applies the stream and sends final response with size
func (s *JSONRPCServer) handleApplySplitFDStream(fdPasser *FDPasser, req *SplitFDStreamRequest) {
	// Parse the apply-specific parameters
	applyParams, err := ParseApplySplitFDStreamParams(req.Params)
	if err != nil {
		resp := NewErrorResponse(ErrorCodeInvalidParams, err.Error(), nil, req.ID)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Validate stream size
	if applyParams.StreamSize <= 0 {
		resp := NewErrorResponse(ErrorCodeInvalidParams, "streamSize must be positive", nil, req.ID)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Get the driver that supports splitfdstream
	driver, ok := s.driver.(SplitFDStreamDriver)
	if !ok {
		resp := NewErrorResponse(
			ErrorCodeDriverNotSupported,
			fmt.Sprintf("driver %s does not support splitfdstream", fmt.Sprint(s.driver)),
			nil,
			req.ID,
		)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Send ACK to indicate we're ready to receive data
	ackResult := &SplitFDStreamResult{Message: "ready"}
	ackResp := NewSuccessResponse(ackResult, req.ID)
	s.sendResponse(fdPasser, ackResp)

	// Receive file descriptors if expected
	var fds []*os.File
	var initialData []byte

	if applyParams.NumFileDescriptors > 0 {
		// Receive FDs along with first chunk of data
		data, receivedFDs, err := fdPasser.ReceiveFileDescriptors(32 * 1024)
		if err != nil {
			resp := NewErrorResponse(ErrorCodeFileDescriptorError, fmt.Sprintf("failed to receive file descriptors: %v", err), nil, req.ID)
			s.sendResponse(fdPasser, resp)
			return
		}
		fds = receivedFDs
		initialData = data

		// Close FDs when done
		defer func() {
			for _, fd := range fds {
				fd.Close()
			}
		}()
	}

	// Create a reader that reads the stream data
	streamReader := newLimitedSocketReader(fdPasser, applyParams.StreamSize, initialData)

	// Build options for the driver
	opts := &ApplySplitFDStreamOpts{
		Stream:            streamReader,
		FileDescriptors:   fds,
		IgnoreChownErrors: applyParams.IgnoreChownErrors,
		MountLabel:        applyParams.MountLabel,
	}

	// Apply the splitfdstream
	opts.LayerID = applyParams.LayerID
	size, err := driver.ApplySplitFDStream(opts)
	if err != nil {
		resp := NewErrorResponse(ErrorCodeInternalError, fmt.Sprintf("failed to apply splitfdstream: %v", err), nil, req.ID)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Send success response
	result := &SplitFDStreamResult{Size: &size}
	resp := NewSuccessResponse(result, req.ID)
	s.sendResponse(fdPasser, resp)
}

// Batch size for sending file descriptors.
// The kernel limits file descriptors per sendmsg call to SCM_MAX_FD = 253.
// We use 200 as a safe limit well under that.
const fdBatchSize = 200

// handleGetSplitFDStream handles get_splitfdstream requests.
// Protocol:
// 1. Client sends JSON-RPC request with layerId, parentId
// 2. Server creates the stream and sends response with numFDs, numBatches, and streamSize
// 3. Server sends FDs in batches of 200, waiting for "continue\n" between batches
// 4. Server streams remaining data
// 5. Client receives all data
func (s *JSONRPCServer) handleGetSplitFDStream(fdPasser *FDPasser, req *SplitFDStreamRequest) {
	// Parse the get-specific parameters
	getParams, err := ParseGetSplitFDStreamParams(req.Params)
	if err != nil {
		resp := NewErrorResponse(ErrorCodeInvalidParams, err.Error(), nil, req.ID)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Get the driver that supports splitfdstream
	driver, ok := s.driver.(SplitFDStreamDriver)
	if !ok {
		resp := NewErrorResponse(
			ErrorCodeDriverNotSupported,
			fmt.Sprintf("driver %s does not support splitfdstream", fmt.Sprint(s.driver)),
			nil,
			req.ID,
		)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Build options for the driver - no FD limit, batching handles unlimited FDs
	opts := &GetSplitFDStreamOpts{
		MountLabel: getParams.MountLabel,
	}

	// Get the splitfdstream
	streamReader, fds, err := driver.GetSplitFDStream(getParams.LayerID, getParams.ParentID, opts)
	if err != nil {
		resp := NewErrorResponse(ErrorCodeInternalError, fmt.Sprintf("failed to get splitfdstream: %v", err), nil, req.ID)
		s.sendResponse(fdPasser, resp)
		return
	}

	// Ensure we close everything when done
	defer func() {
		if streamReader != nil {
			streamReader.Close()
		}
		for _, fd := range fds {
			fd.Close()
		}
	}()

	// Read all the stream data into memory to get the size
	// Note: For large layers, this could be memory-intensive. A production
	// implementation might want to stream in chunks or use a temp file.
	streamData, err := io.ReadAll(streamReader)
	if err != nil {
		resp := NewErrorResponse(ErrorCodeInternalError, fmt.Sprintf("failed to read stream data: %v", err), nil, req.ID)
		s.sendResponse(fdPasser, resp)
		return
	}

	numFDs := len(fds)
	streamSize := int64(len(streamData))

	// Calculate number of batches
	numBatches := (numFDs + fdBatchSize - 1) / fdBatchSize
	if numBatches == 0 {
		numBatches = 1 // At least 1 batch even with 0 FDs
	}

	logrus.Debugf("splitfdstream: GetSplitFDStream numFDs=%d, numBatches=%d, streamSize=%d", numFDs, numBatches, streamSize)

	// Send initial response with metadata
	result := &SplitFDStreamResult{
		Size:            &streamSize,
		FileDescriptors: &numFDs,
		BatchSize:       fdBatchSize,
		NumBatches:      numBatches,
		Message:         "stream_ready",
	}
	resp := NewSuccessResponse(result, req.ID)
	s.sendResponse(fdPasser, resp)

	// Send file descriptors in batches
	streamDataOffset := 0
	for batch := 0; batch < numBatches; batch++ {
		start := batch * fdBatchSize
		end := start + fdBatchSize
		if end > numFDs {
			end = numFDs
		}
		batchFDs := fds[start:end]

		// Calculate data chunk for this batch
		chunkSize := 32 * 1024
		if streamDataOffset+chunkSize > len(streamData) {
			chunkSize = len(streamData) - streamDataOffset
		}
		var chunkData []byte
		if chunkSize > 0 {
			chunkData = streamData[streamDataOffset : streamDataOffset+chunkSize]
			streamDataOffset += chunkSize
		}

		// Send this batch's FDs with data chunk
		if len(batchFDs) > 0 {
			if err := fdPasser.SendFileDescriptors(batchFDs, chunkData); err != nil {
				logrus.Errorf("splitfdstream: failed to send batch %d: %v", batch, err)
				return
			}
		} else if len(chunkData) > 0 {
			// No FDs in this batch but we have data
			if err := fdPasser.SendMessage(chunkData); err != nil {
				logrus.Errorf("splitfdstream: failed to send data chunk in batch %d: %v", batch, err)
				return
			}
		}

		// Wait for "continue" from client (except for last batch)
		if batch < numBatches-1 {
			if err := s.waitForContinue(fdPasser); err != nil {
				logrus.Errorf("splitfdstream: failed to receive continue after batch %d: %v", batch, err)
				return
			}
		}
	}

	// Send any remaining stream data after all batches
	if streamDataOffset < len(streamData) {
		remaining := streamData[streamDataOffset:]
		if err := fdPasser.SendMessage(remaining); err != nil {
			logrus.Errorf("splitfdstream: failed to send remaining stream data: %v", err)
			return
		}
	}
}

// waitForContinue reads "continue\n" from the client.
func (s *JSONRPCServer) waitForContinue(fdPasser *FDPasser) error {
	msg, err := fdPasser.ReadLine()
	if err != nil {
		return fmt.Errorf("failed to read continue message: %w", err)
	}
	if string(msg) != "continue" {
		return fmt.Errorf("expected 'continue', got '%s'", string(msg))
	}
	return nil
}

// sendResponse sends a JSON-RPC response back to the client.
func (s *JSONRPCServer) sendResponse(fdPasser *FDPasser, resp *SplitFDStreamResponse) {
	data, err := resp.Marshal()
	if err != nil {
		logrus.Errorf("splitfdstream: failed to marshal response: %v", err)
		return
	}

	// Add newline to follow line-delimited protocol
	data = append(data, '\n')

	if err := fdPasser.SendMessage(data); err != nil {
		logrus.Errorf("splitfdstream: failed to send response: %v", err)
	}
}

// IsRunning returns true if the server is currently running.
func (s *JSONRPCServer) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// GetSocketPath returns the path to the UNIX socket.
func (s *JSONRPCServer) GetSocketPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.socketPath
}

// JSONRPCClient provides a client for communicating with the JSON-RPC server.
type JSONRPCClient struct {
	fdPasser *FDPasser
	conn     *net.UnixConn
}

// NewJSONRPCClient creates a new client connected to the specified UNIX socket.
func NewJSONRPCClient(socketPath string) (*JSONRPCClient, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to splitfdstream server: %w", err)
	}

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("connection is not a unix socket")
	}

	return &JSONRPCClient{
		fdPasser: NewFDPasser(unixConn),
		conn:     unixConn,
	}, nil
}

// Ping sends a ping request to the server.
func (c *JSONRPCClient) Ping(message string) (*SplitFDStreamResponse, error) {
	params := SplitFDStreamParams{
		Options: map[string]interface{}{"message": message},
	}
	req := NewRequest(MethodPing, params, "ping-1")
	return c.sendRequest(req)
}

// sendRequest sends a JSON-RPC request and waits for the response.
func (c *JSONRPCClient) sendRequest(req *SplitFDStreamRequest) (*SplitFDStreamResponse, error) {
	// Marshal and send request
	data, err := req.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Add newline
	data = append(data, '\n')

	if err := c.fdPasser.SendMessage(data); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Read response line using recvmsg to not miss any FDs
	respLine, err := c.fdPasser.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var resp SplitFDStreamResponse
	if err := json.Unmarshal(respLine, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &resp, nil
}

// SendRawRequest sends a raw JSON-RPC request string and returns the response.
func (c *JSONRPCClient) SendRawRequest(requestLine string) (*SplitFDStreamResponse, error) {
	// Add newline if not present
	if !strings.HasSuffix(requestLine, "\n") {
		requestLine += "\n"
	}

	if err := c.fdPasser.SendMessage([]byte(requestLine)); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Read response
	respData, err := c.fdPasser.ReceiveMessage(4096)
	if err != nil {
		return nil, fmt.Errorf("failed to receive response: %w", err)
	}

	// Parse response
	var resp SplitFDStreamResponse
	respStr := strings.TrimSpace(string(respData))
	if err := json.Unmarshal([]byte(respStr), &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &resp, nil
}

// Close closes the client connection.
func (c *JSONRPCClient) Close() error {
	if c.fdPasser != nil {
		c.fdPasser.Close()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ApplySplitFDStream sends an apply_splitfdstream request to the server.
func (c *JSONRPCClient) ApplySplitFDStream(layerID string, streamData []byte, fds []*os.File, ignoreChownErrors bool, mountLabel string) (int64, error) {
	// Build request
	params := SplitFDStreamParams{
		LayerID: layerID,
		Options: map[string]interface{}{
			"numFileDescriptors": len(fds),
			"streamSize":         int64(len(streamData)),
			"ignoreChownErrors":  ignoreChownErrors,
			"mountLabel":         mountLabel,
		},
	}
	req := NewRequest(MethodApplySplitFDStream, params, "apply-1")

	// Send request and get ACK
	resp, err := c.sendRequest(req)
	if err != nil {
		return 0, err
	}
	if resp.Error != nil {
		return 0, resp.Error
	}

	// Send FDs with first chunk if any
	if len(fds) > 0 {
		chunkSize := 32 * 1024
		if len(streamData) < chunkSize {
			chunkSize = len(streamData)
		}
		if err := c.fdPasser.SendFileDescriptors(fds, streamData[:chunkSize]); err != nil {
			return 0, fmt.Errorf("failed to send file descriptors: %w", err)
		}
		streamData = streamData[chunkSize:]
	}

	// Send remaining data
	if len(streamData) > 0 {
		if err := c.fdPasser.SendMessage(streamData); err != nil {
			return 0, fmt.Errorf("failed to send stream data: %w", err)
		}
	}

	// Get final response
	finalResp, err := c.receiveResponse()
	if err != nil {
		return 0, err
	}
	if finalResp.Error != nil {
		return 0, finalResp.Error
	}
	if finalResp.Result != nil && finalResp.Result.Size != nil {
		return *finalResp.Result.Size, nil
	}
	return 0, nil
}

// receiveResponse reads and parses a JSON-RPC response.
func (c *JSONRPCClient) receiveResponse() (*SplitFDStreamResponse, error) {
	respData, err := c.fdPasser.ReceiveMessage(4096)
	if err != nil {
		return nil, fmt.Errorf("failed to receive response: %w", err)
	}
	var resp SplitFDStreamResponse
	if err := json.Unmarshal(bytes.TrimSpace(respData), &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return &resp, nil
}

// GetSplitFDStream sends a get_splitfdstream request to the server and receives the stream data.
// Returns the stream data and any file descriptors received from the server.
// Supports batched FD receiving for layers with more than 200 files.
func (c *JSONRPCClient) GetSplitFDStream(layerID, parentID string) ([]byte, []*os.File, error) {
	// Build request
	params := SplitFDStreamParams{
		LayerID:  layerID,
		ParentID: parentID,
	}
	req := NewRequest(MethodGetSplitFDStream, params, "get-1")

	// Send request and get metadata response
	resp, err := c.sendRequest(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.Error != nil {
		return nil, nil, resp.Error
	}

	// Extract metadata
	if resp.Result == nil {
		return nil, nil, fmt.Errorf("no result in response")
	}

	streamSize := int64(0)
	if resp.Result.Size != nil {
		streamSize = *resp.Result.Size
	}

	numBatches := resp.Result.NumBatches
	if numBatches == 0 {
		numBatches = 1 // Fallback for older servers
	}

	var streamData []byte
	var allFDs []*os.File

	// Receive FDs in batches
	for batch := 0; batch < numBatches; batch++ {
		// Receive this batch's FDs with data chunk
		data, batchFDs, err := c.fdPasser.ReceiveFileDescriptors(32 * 1024)
		if err != nil {
			// Close any FDs received so far
			for _, fd := range allFDs {
				fd.Close()
			}
			return nil, nil, fmt.Errorf("failed to receive batch %d: %w", batch, err)
		}

		allFDs = append(allFDs, batchFDs...)
		streamData = append(streamData, data...)

		// Send "continue" (except for last batch)
		if batch < numBatches-1 {
			if err := c.fdPasser.SendMessage([]byte("continue\n")); err != nil {
				for _, fd := range allFDs {
					fd.Close()
				}
				return nil, nil, fmt.Errorf("failed to send continue after batch %d: %w", batch, err)
			}
		}
	}

	// Receive remaining stream data
	remaining := streamSize - int64(len(streamData))
	for remaining > 0 {
		chunkSize := int(remaining)
		if chunkSize > 32*1024 {
			chunkSize = 32 * 1024
		}
		data, err := c.fdPasser.ReceiveMessage(chunkSize)
		if err != nil {
			// Close any received FDs on error
			for _, fd := range allFDs {
				fd.Close()
			}
			return nil, nil, fmt.Errorf("failed to receive stream data: %w", err)
		}
		streamData = append(streamData, data...)
		remaining -= int64(len(data))
	}

	return streamData, allFDs, nil
}
