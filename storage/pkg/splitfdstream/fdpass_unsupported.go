//go:build !linux

package splitfdstream

import (
	"errors"
	"net"
	"os"
)

var errUnsupported = errors.New("splitfdstream is not supported on this platform")

// FDPasser provides functionality for passing file descriptors over UNIX sockets.
type FDPasser struct{}

// NewFDPasser creates a new FDPasser for the given UNIX socket connection.
func NewFDPasser(conn *net.UnixConn) *FDPasser {
	return &FDPasser{}
}

// SendFileDescriptors sends file descriptors over the UNIX socket along with a message.
func (p *FDPasser) SendFileDescriptors(fds []*os.File, message []byte) error {
	return errUnsupported
}

// ReceiveFileDescriptors receives file descriptors from the UNIX socket along with a message.
func (p *FDPasser) ReceiveFileDescriptors(bufSize int) ([]byte, []*os.File, error) {
	return nil, nil, errUnsupported
}

// SendMessage sends a message without file descriptors.
func (p *FDPasser) SendMessage(message []byte) error {
	return errUnsupported
}

// ReceiveMessage receives a message without file descriptors.
func (p *FDPasser) ReceiveMessage(bufSize int) ([]byte, error) {
	return nil, errUnsupported
}

// ReadLine reads bytes until a newline is encountered.
func (p *FDPasser) ReadLine() ([]byte, error) {
	return nil, errUnsupported
}

// Close closes the underlying UNIX socket connection.
func (p *FDPasser) Close() error {
	return nil
}

// CreateSocketPair creates a pair of connected UNIX sockets.
func CreateSocketPair() (*net.UnixConn, *net.UnixConn, error) {
	return nil, nil, errUnsupported
}
