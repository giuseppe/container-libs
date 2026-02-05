package splitfdstream

import (
	"errors"
	"io"
	"os"

	"go.podman.io/storage/pkg/idtools"
)

// ApplySplitFDStreamOpts contains optional arguments for ApplySplitFDStream methods.
// This API is experimental and can be changed without bumping the major version number.
type ApplySplitFDStreamOpts struct {
	Stream            io.Reader
	FileDescriptors   []*os.File
	IDMappings        *idtools.IDMappings
	MountLabel        string
	IgnoreChownErrors bool
	ForceMask         *os.FileMode

	// Target specification - exactly one of these must be set:
	//
	// StagingDir: Extract to staging directory for PrepareStagedLayer workflow.
	//             Used when creating layers through the staged layer API.
	StagingDir string
	//
	// LayerID: Extract directly to existing layer's diff directory.
	//          Used for direct layer application.
	LayerID string
}

// Validate checks that exactly one target (StagingDir or LayerID) is specified.
// This enforces the mutual exclusion constraint at runtime.
func (opts *ApplySplitFDStreamOpts) Validate() error {
	if opts == nil {
		return errors.New("options cannot be nil")
	}

	hasStagingDir := opts.StagingDir != ""
	hasLayerID := opts.LayerID != ""

	if !hasStagingDir && !hasLayerID {
		return errors.New("either StagingDir or LayerID must be specified")
	}

	if hasStagingDir && hasLayerID {
		return errors.New("StagingDir and LayerID are mutually exclusive")
	}

	return nil
}

// GetSplitFDStreamOpts contains optional arguments for GetSplitFDStream methods.
// This API is experimental and can be changed without bumping the major version number.
type GetSplitFDStreamOpts struct {
	IDMappings *idtools.IDMappings
	MountLabel string
}

// SplitFDStreamDriver provides splitfdstream capabilities for layer operations.
// Drivers that implement this interface support both apply and create operations.
// This API is experimental and can be changed without bumping the major version number.
type SplitFDStreamDriver interface {
	// ApplySplitFDStream applies changes from a split FD stream.
	ApplySplitFDStream(options *ApplySplitFDStreamOpts) (int64, error)

	// GetSplitFDStream generates a split FD stream from the layer differences.
	GetSplitFDStream(id, parent string, options *GetSplitFDStreamOpts) (io.ReadCloser, []*os.File, error)
}
