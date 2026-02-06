package splitfdstream

import (
	"encoding/json"
	"fmt"
)

// JSON-RPC 2.0 protocol constants
const (
	JSONRPCVersion = "2.0"
)

// Method names for the splitfdstream JSON-RPC API
const (
	MethodApplySplitFDStream = "apply_splitfdstream"
	MethodGetSplitFDStream   = "get_splitfdstream"
	MethodLookupDigest       = "lookup_digest"
	MethodPing               = "ping"
)

// Error codes following JSON-RPC 2.0 specification
const (
	ErrorCodeParseError     = -32700
	ErrorCodeInvalidRequest = -32600
	ErrorCodeMethodNotFound = -32601
	ErrorCodeInvalidParams  = -32602
	ErrorCodeInternalError  = -32603

	// Application-specific error codes (range -32000 to -32099)
	ErrorCodeDriverNotSupported  = -32000
	ErrorCodeFileDescriptorError = -32001
	ErrorCodeDigestNotFound      = -32002
	ErrorCodeStoreNotAvailable   = -32003
)

// SplitFDStreamRequest represents a JSON-RPC request for splitfdstream operations.
type SplitFDStreamRequest struct {
	JSONRPC string              `json:"jsonrpc"`
	Method  string              `json:"method"`
	Params  SplitFDStreamParams `json:"params"`
	ID      interface{}         `json:"id"`
}

// SplitFDStreamResponse represents a JSON-RPC response for splitfdstream operations.
type SplitFDStreamResponse struct {
	JSONRPC string               `json:"jsonrpc"`
	Result  *SplitFDStreamResult `json:"result,omitempty"`
	Error   *SplitFDStreamError  `json:"error,omitempty"`
	ID      interface{}          `json:"id"`
}

// SplitFDStreamParams contains the parameters for splitfdstream operations.
type SplitFDStreamParams struct {
	LayerID  string                 `json:"layerId"`
	ParentID string                 `json:"parentId,omitempty"`
	Options  map[string]interface{} `json:"options,omitempty"`
}

// SplitFDStreamResult contains the result of a splitfdstream operation.
type SplitFDStreamResult struct {
	Size            *int64 `json:"size,omitempty"`
	FileDescriptors *int   `json:"fileDescriptors,omitempty"`
	BatchSize       int    `json:"batchSize,omitempty"`
	NumBatches      int    `json:"numBatches,omitempty"`
	Offset          int64  `json:"offset,omitempty"`
	ChunkSize       int64  `json:"chunkSize,omitempty"`
	Message         string `json:"message,omitempty"`
}

// SplitFDStreamError represents an error in a JSON-RPC response.
type SplitFDStreamError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Error implements the error interface for SplitFDStreamError.
func (e *SplitFDStreamError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// NewRequest creates a new splitfdstream JSON-RPC request.
func NewRequest(method string, params SplitFDStreamParams, id interface{}) *SplitFDStreamRequest {
	return &SplitFDStreamRequest{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  params,
		ID:      id,
	}
}

// NewSuccessResponse creates a new successful JSON-RPC response.
func NewSuccessResponse(result *SplitFDStreamResult, id interface{}) *SplitFDStreamResponse {
	return &SplitFDStreamResponse{
		JSONRPC: JSONRPCVersion,
		Result:  result,
		ID:      id,
	}
}

// NewErrorResponse creates a new error JSON-RPC response.
func NewErrorResponse(code int, message string, data interface{}, id interface{}) *SplitFDStreamResponse {
	return &SplitFDStreamResponse{
		JSONRPC: JSONRPCVersion,
		Error: &SplitFDStreamError{
			Code:    code,
			Message: message,
			Data:    data,
		},
		ID: id,
	}
}

// ParseRequest parses a JSON-RPC request from bytes.
func ParseRequest(data []byte) (*SplitFDStreamRequest, error) {
	var req SplitFDStreamRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, &SplitFDStreamError{
			Code:    ErrorCodeParseError,
			Message: "Parse error",
			Data:    err.Error(),
		}
	}

	// Validate JSON-RPC version
	if req.JSONRPC != JSONRPCVersion {
		return nil, &SplitFDStreamError{
			Code:    ErrorCodeInvalidRequest,
			Message: "Invalid Request",
			Data:    fmt.Sprintf("unsupported JSON-RPC version: %s", req.JSONRPC),
		}
	}

	// Validate method
	if req.Method == "" {
		return nil, &SplitFDStreamError{
			Code:    ErrorCodeInvalidRequest,
			Message: "Invalid Request",
			Data:    "missing method",
		}
	}

	return &req, nil
}

// Marshal serializes the request to JSON bytes.
func (r *SplitFDStreamRequest) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

// Marshal serializes the response to JSON bytes.
func (r *SplitFDStreamResponse) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

// IsValid checks if the request is valid for the specified method.
func (r *SplitFDStreamRequest) IsValid() error {
	switch r.Method {
	case MethodApplySplitFDStream:
		if r.Params.LayerID == "" {
			return &SplitFDStreamError{
				Code:    ErrorCodeInvalidParams,
				Message: "Invalid params",
				Data:    "layerId is required for apply_splitfdstream",
			}
		}
	case MethodGetSplitFDStream:
		if r.Params.LayerID == "" {
			return &SplitFDStreamError{
				Code:    ErrorCodeInvalidParams,
				Message: "Invalid params",
				Data:    "layerId is required for get_splitfdstream",
			}
		}
	case MethodLookupDigest:
		// Validation is done in ParseLookupDigestParams
	case MethodPing:
		// No validation required for ping
	default:
		return &SplitFDStreamError{
			Code:    ErrorCodeMethodNotFound,
			Message: "Method not found",
			Data:    fmt.Sprintf("unknown method: %s", r.Method),
		}
	}
	return nil
}

// ApplySplitFDStreamParams represents parameters specific to apply_splitfdstream method.
type ApplySplitFDStreamParams struct {
	LayerID            string `json:"layerId"`
	MountLabel         string `json:"mountLabel,omitempty"`
	IgnoreChownErrors  bool   `json:"ignoreChownErrors,omitempty"`
	NumFileDescriptors int    `json:"numFileDescriptors,omitempty"`
	StreamSize         int64  `json:"streamSize"`
}

// GetSplitFDStreamParams represents parameters specific to get_splitfdstream method.
type GetSplitFDStreamParams struct {
	LayerID    string `json:"layerId"`
	ParentID   string `json:"parentId,omitempty"`
	MountLabel string `json:"mountLabel,omitempty"`
}

// PingParams represents parameters for the ping method.
type PingParams struct {
	Message string `json:"message,omitempty"`
}

// ParseApplySplitFDStreamParams parses the generic params as ApplySplitFDStreamParams.
func ParseApplySplitFDStreamParams(params SplitFDStreamParams) (*ApplySplitFDStreamParams, error) {
	// Merge Options into a flat map with top-level fields
	flatMap := make(map[string]interface{})
	for k, v := range params.Options {
		flatMap[k] = v
	}
	flatMap["layerId"] = params.LayerID
	if params.ParentID != "" {
		flatMap["parentId"] = params.ParentID
	}

	data, err := json.Marshal(flatMap)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}

	var applyParams ApplySplitFDStreamParams
	if err := json.Unmarshal(data, &applyParams); err != nil {
		return nil, fmt.Errorf("failed to parse apply params: %w", err)
	}

	return &applyParams, nil
}

// ParseGetSplitFDStreamParams parses the generic params as GetSplitFDStreamParams.
func ParseGetSplitFDStreamParams(params SplitFDStreamParams) (*GetSplitFDStreamParams, error) {
	// Merge Options into a flat map with top-level fields
	flatMap := make(map[string]interface{})
	for k, v := range params.Options {
		flatMap[k] = v
	}
	flatMap["layerId"] = params.LayerID
	if params.ParentID != "" {
		flatMap["parentId"] = params.ParentID
	}

	data, err := json.Marshal(flatMap)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}

	var getParams GetSplitFDStreamParams
	if err := json.Unmarshal(data, &getParams); err != nil {
		return nil, fmt.Errorf("failed to parse get params: %w", err)
	}

	return &getParams, nil
}

// ParsePingParams parses the generic params as PingParams.
func ParsePingParams(params SplitFDStreamParams) (*PingParams, error) {
	// Merge Options into a flat map
	flatMap := make(map[string]interface{})
	for k, v := range params.Options {
		flatMap[k] = v
	}

	data, err := json.Marshal(flatMap)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}

	var pingParams PingParams
	if err := json.Unmarshal(data, &pingParams); err != nil {
		return nil, fmt.Errorf("failed to parse ping params: %w", err)
	}

	return &pingParams, nil
}

// LookupDigestParams represents parameters specific to lookup_digest method.
type LookupDigestParams struct {
	Digest string `json:"digest"`
}

// ParseLookupDigestParams parses the generic params as LookupDigestParams.
func ParseLookupDigestParams(params SplitFDStreamParams) (*LookupDigestParams, error) {
	// Merge Options into a flat map
	flatMap := make(map[string]interface{})
	for k, v := range params.Options {
		flatMap[k] = v
	}

	data, err := json.Marshal(flatMap)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal params: %w", err)
	}

	var lookupParams LookupDigestParams
	if err := json.Unmarshal(data, &lookupParams); err != nil {
		return nil, fmt.Errorf("failed to parse lookup params: %w", err)
	}

	if lookupParams.Digest == "" {
		return nil, &SplitFDStreamError{
			Code:    ErrorCodeInvalidParams,
			Message: "Invalid params",
			Data:    "digest is required for lookup_digest",
		}
	}

	return &lookupParams, nil
}
