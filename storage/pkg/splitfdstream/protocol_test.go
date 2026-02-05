package splitfdstream

import (
	"encoding/json"
	"testing"
)

func TestNewRequest(t *testing.T) {
	params := SplitFDStreamParams{
		LayerID:  "test-layer",
		ParentID: "parent-layer",
		Options:  map[string]interface{}{"test": "value"},
	}

	req := NewRequest(MethodApplySplitFDStream, params, "test-id")

	if req.JSONRPC != JSONRPCVersion {
		t.Errorf("Expected JSONRPC %s, got %s", JSONRPCVersion, req.JSONRPC)
	}

	if req.Method != MethodApplySplitFDStream {
		t.Errorf("Expected method %s, got %s", MethodApplySplitFDStream, req.Method)
	}

	if req.Params.LayerID != "test-layer" {
		t.Errorf("Expected layerId %s, got %s", "test-layer", req.Params.LayerID)
	}

	if req.ID != "test-id" {
		t.Errorf("Expected ID %s, got %v", "test-id", req.ID)
	}
}

func TestNewSuccessResponse(t *testing.T) {
	result := &SplitFDStreamResult{
		Size:    int64Ptr(1024),
		Message: "success",
	}

	resp := NewSuccessResponse(result, "test-id")

	if resp.JSONRPC != JSONRPCVersion {
		t.Errorf("Expected JSONRPC %s, got %s", JSONRPCVersion, resp.JSONRPC)
	}

	if resp.Result == nil {
		t.Fatal("Expected result, got nil")
	}

	if *resp.Result.Size != 1024 {
		t.Errorf("Expected size 1024, got %d", *resp.Result.Size)
	}

	if resp.Error != nil {
		t.Errorf("Expected no error, got %v", resp.Error)
	}

	if resp.ID != "test-id" {
		t.Errorf("Expected ID %s, got %v", "test-id", resp.ID)
	}
}

func TestNewErrorResponse(t *testing.T) {
	resp := NewErrorResponse(ErrorCodeMethodNotFound, "Method not found", "test data", "test-id")

	if resp.JSONRPC != JSONRPCVersion {
		t.Errorf("Expected JSONRPC %s, got %s", JSONRPCVersion, resp.JSONRPC)
	}

	if resp.Result != nil {
		t.Errorf("Expected no result, got %v", resp.Result)
	}

	if resp.Error == nil {
		t.Fatal("Expected error, got nil")
	}

	if resp.Error.Code != ErrorCodeMethodNotFound {
		t.Errorf("Expected error code %d, got %d", ErrorCodeMethodNotFound, resp.Error.Code)
	}

	if resp.Error.Message != "Method not found" {
		t.Errorf("Expected error message %s, got %s", "Method not found", resp.Error.Message)
	}

	if resp.Error.Data != "test data" {
		t.Errorf("Expected error data %s, got %v", "test data", resp.Error.Data)
	}

	if resp.ID != "test-id" {
		t.Errorf("Expected ID %s, got %v", "test-id", resp.ID)
	}
}

func TestParseRequest_Valid(t *testing.T) {
	jsonData := `{
		"jsonrpc": "2.0",
		"method": "apply_splitfdstream",
		"params": {
			"layerId": "test-layer",
			"parentId": "parent-layer"
		},
		"id": 1
	}`

	req, err := ParseRequest([]byte(jsonData))
	if err != nil {
		t.Fatalf("Failed to parse request: %v", err)
	}

	if req.Method != MethodApplySplitFDStream {
		t.Errorf("Expected method %s, got %s", MethodApplySplitFDStream, req.Method)
	}

	if req.Params.LayerID != "test-layer" {
		t.Errorf("Expected layerId %s, got %s", "test-layer", req.Params.LayerID)
	}

	if req.Params.ParentID != "parent-layer" {
		t.Errorf("Expected parentId %s, got %s", "parent-layer", req.Params.ParentID)
	}
}

func TestParseRequest_InvalidJSON(t *testing.T) {
	jsonData := `{"invalid": json}`

	_, err := ParseRequest([]byte(jsonData))
	if err == nil {
		t.Fatal("Expected error for invalid JSON")
	}

	rpcErr, ok := err.(*SplitFDStreamError)
	if !ok {
		t.Fatalf("Expected SplitFDStreamError, got %T", err)
	}

	if rpcErr.Code != ErrorCodeParseError {
		t.Errorf("Expected error code %d, got %d", ErrorCodeParseError, rpcErr.Code)
	}
}

func TestParseRequest_InvalidJSONRPCVersion(t *testing.T) {
	jsonData := `{
		"jsonrpc": "1.0",
		"method": "test",
		"params": {},
		"id": 1
	}`

	_, err := ParseRequest([]byte(jsonData))
	if err == nil {
		t.Fatal("Expected error for invalid JSON-RPC version")
	}

	rpcErr, ok := err.(*SplitFDStreamError)
	if !ok {
		t.Fatalf("Expected SplitFDStreamError, got %T", err)
	}

	if rpcErr.Code != ErrorCodeInvalidRequest {
		t.Errorf("Expected error code %d, got %d", ErrorCodeInvalidRequest, rpcErr.Code)
	}
}

func TestParseRequest_MissingMethod(t *testing.T) {
	jsonData := `{
		"jsonrpc": "2.0",
		"params": {},
		"id": 1
	}`

	_, err := ParseRequest([]byte(jsonData))
	if err == nil {
		t.Fatal("Expected error for missing method")
	}

	rpcErr, ok := err.(*SplitFDStreamError)
	if !ok {
		t.Fatalf("Expected SplitFDStreamError, got %T", err)
	}

	if rpcErr.Code != ErrorCodeInvalidRequest {
		t.Errorf("Expected error code %d, got %d", ErrorCodeInvalidRequest, rpcErr.Code)
	}
}

func TestRequestMarshal(t *testing.T) {
	req := NewRequest(MethodPing, SplitFDStreamParams{}, "test-id")

	data, err := req.Marshal()
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	// Parse it back to verify it's valid JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to parse marshaled request: %v", err)
	}

	if parsed["jsonrpc"] != JSONRPCVersion {
		t.Errorf("Expected jsonrpc %s, got %v", JSONRPCVersion, parsed["jsonrpc"])
	}

	if parsed["method"] != MethodPing {
		t.Errorf("Expected method %s, got %v", MethodPing, parsed["method"])
	}
}

func TestResponseMarshal(t *testing.T) {
	resp := NewSuccessResponse(&SplitFDStreamResult{Message: "test"}, "test-id")

	data, err := resp.Marshal()
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}

	// Parse it back to verify it's valid JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to parse marshaled response: %v", err)
	}

	if parsed["jsonrpc"] != JSONRPCVersion {
		t.Errorf("Expected jsonrpc %s, got %v", JSONRPCVersion, parsed["jsonrpc"])
	}

	result, ok := parsed["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected result object, got %T", parsed["result"])
	}

	if result["message"] != "test" {
		t.Errorf("Expected message 'test', got %v", result["message"])
	}
}

func TestRequestValidation(t *testing.T) {
	tests := []struct {
		name    string
		req     *SplitFDStreamRequest
		wantErr bool
		errCode int
	}{
		{
			name: "valid apply request",
			req: &SplitFDStreamRequest{
				Method: MethodApplySplitFDStream,
				Params: SplitFDStreamParams{LayerID: "test-layer"},
			},
			wantErr: false,
		},
		{
			name: "apply request missing layer ID",
			req: &SplitFDStreamRequest{
				Method: MethodApplySplitFDStream,
				Params: SplitFDStreamParams{},
			},
			wantErr: true,
			errCode: ErrorCodeInvalidParams,
		},
		{
			name: "valid get request",
			req: &SplitFDStreamRequest{
				Method: MethodGetSplitFDStream,
				Params: SplitFDStreamParams{LayerID: "test-layer"},
			},
			wantErr: false,
		},
		{
			name: "get request missing layer ID",
			req: &SplitFDStreamRequest{
				Method: MethodGetSplitFDStream,
				Params: SplitFDStreamParams{},
			},
			wantErr: true,
			errCode: ErrorCodeInvalidParams,
		},
		{
			name: "valid ping request",
			req: &SplitFDStreamRequest{
				Method: MethodPing,
				Params: SplitFDStreamParams{},
			},
			wantErr: false,
		},
		{
			name: "unknown method",
			req: &SplitFDStreamRequest{
				Method: "unknown_method",
				Params: SplitFDStreamParams{},
			},
			wantErr: true,
			errCode: ErrorCodeMethodNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.IsValid()

			if (err != nil) != tt.wantErr {
				t.Errorf("IsValid() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				rpcErr, ok := err.(*SplitFDStreamError)
				if !ok {
					t.Fatalf("Expected SplitFDStreamError, got %T", err)
				}

				if rpcErr.Code != tt.errCode {
					t.Errorf("Expected error code %d, got %d", tt.errCode, rpcErr.Code)
				}
			}
		})
	}
}

func TestParseSpecificParams(t *testing.T) {
	// Create JSON data directly to test parsing
	jsonData := `{
		"layerId": "test-layer",
		"parentId": "parent-layer",
		"mountLabel": "test-label",
		"ignoreChownErrors": true,
		"maxFileDescriptors": 10
	}`

	var genericMap map[string]interface{}
	if err := json.Unmarshal([]byte(jsonData), &genericMap); err != nil {
		t.Fatalf("Failed to unmarshal test data: %v", err)
	}

	// Convert to SplitFDStreamParams
	params := SplitFDStreamParams{
		LayerID:  genericMap["layerId"].(string),
		ParentID: genericMap["parentId"].(string),
		Options:  genericMap,
	}

	applyParams, err := ParseApplySplitFDStreamParams(params)
	if err != nil {
		t.Fatalf("Failed to parse apply params: %v", err)
	}

	if applyParams.LayerID != "test-layer" {
		t.Errorf("Expected layerId %s, got %s", "test-layer", applyParams.LayerID)
	}

	if applyParams.MountLabel != "test-label" {
		t.Errorf("Expected mountLabel %s, got %s", "test-label", applyParams.MountLabel)
	}

	if !applyParams.IgnoreChownErrors {
		t.Errorf("Expected ignoreChownErrors true, got false")
	}

	// Test get params
	getParams, err := ParseGetSplitFDStreamParams(params)
	if err != nil {
		t.Fatalf("Failed to parse get params: %v", err)
	}

	if getParams.LayerID != "test-layer" {
		t.Errorf("Expected layerId %s, got %s", "test-layer", getParams.LayerID)
	}

	if getParams.ParentID != "parent-layer" {
		t.Errorf("Expected parentId %s, got %s", "parent-layer", getParams.ParentID)
	}

	// Test ping params
	pingData := `{"message": "hello"}`
	var pingMap map[string]interface{}
	if err := json.Unmarshal([]byte(pingData), &pingMap); err != nil {
		t.Fatalf("Failed to unmarshal ping data: %v", err)
	}

	pingParams := SplitFDStreamParams{
		Options: pingMap,
	}

	parsedPing, err := ParsePingParams(pingParams)
	if err != nil {
		t.Fatalf("Failed to parse ping params: %v", err)
	}

	if parsedPing.Message != "hello" {
		t.Errorf("Expected message %s, got %s", "hello", parsedPing.Message)
	}
}

func TestSplitFDStreamError(t *testing.T) {
	err := &SplitFDStreamError{
		Code:    ErrorCodeMethodNotFound,
		Message: "Method not found",
		Data:    "additional data",
	}

	expectedStr := "JSON-RPC error -32601: Method not found"
	if err.Error() != expectedStr {
		t.Errorf("Expected error string %s, got %s", expectedStr, err.Error())
	}
}

// Helper function to create an int64 pointer
func int64Ptr(v int64) *int64 {
	return &v
}
