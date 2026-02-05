//go:build linux

package splitfdstream

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// FDPasser provides functionality for passing file descriptors over UNIX sockets
// using the SCM_RIGHTS control message mechanism.
type FDPasser struct {
	conn *net.UnixConn
}

// NewFDPasser creates a new FDPasser for the given UNIX socket connection.
func NewFDPasser(conn *net.UnixConn) *FDPasser {
	return &FDPasser{conn: conn}
}

// SendFileDescriptors sends file descriptors over the UNIX socket along with a message.
// The message can be empty but is required for the underlying socket API.
func (p *FDPasser) SendFileDescriptors(fds []*os.File, message []byte) error {
	if len(fds) == 0 {
		return fmt.Errorf("no file descriptors to send")
	}

	// Convert file pointers to raw file descriptors
	rawFds := make([]int, len(fds))
	for i, f := range fds {
		rawFds[i] = int(f.Fd())
	}

	// Create control message with file descriptors
	rights := unix.UnixRights(rawFds...)

	// Send message with file descriptors
	_, _, err := p.conn.WriteMsgUnix(message, rights, nil)
	if err != nil {
		return fmt.Errorf("failed to send file descriptors: %w", err)
	}

	return nil
}

// ReceiveFileDescriptors receives file descriptors from the UNIX socket along with a message.
// Returns the message bytes and the received file descriptors.
func (p *FDPasser) ReceiveFileDescriptors(bufSize int) ([]byte, []*os.File, error) {
	// Prepare buffers
	messageBuf := make([]byte, bufSize)
	// Each FD is 4 bytes (int32). For 200 FDs we need 800 bytes.
	// Use 1024 bytes for safety margin.
	oobBuf := make([]byte, unix.CmsgSpace(1024))

	// Receive message and control data
	n, oobn, _, _, err := p.conn.ReadMsgUnix(messageBuf, oobBuf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to receive message: %w", err)
	}

	// Parse control messages
	var fds []*os.File
	if oobn > 0 {
		scms, err := unix.ParseSocketControlMessage(oobBuf[:oobn])
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse control messages: %w", err)
		}

		for _, scm := range scms {
			if scm.Header.Level == unix.SOL_SOCKET && scm.Header.Type == unix.SCM_RIGHTS {
				rawFds, err := unix.ParseUnixRights(&scm)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to parse unix rights: %w", err)
				}

				// Convert raw file descriptors to *os.File
				for _, rawFd := range rawFds {
					file := os.NewFile(uintptr(rawFd), fmt.Sprintf("fd:%d", rawFd))
					if file == nil {
						// Close any previously converted files on error
						for _, f := range fds {
							f.Close()
						}
						return nil, nil, fmt.Errorf("failed to create os.File from fd %d", rawFd)
					}
					fds = append(fds, file)
				}
			}
		}
	}

	return messageBuf[:n], fds, nil
}

// SendMessage sends a message without file descriptors.
func (p *FDPasser) SendMessage(message []byte) error {
	_, err := p.conn.Write(message)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	return nil
}

// ReceiveMessage receives a message, using recvmsg to not miss any FDs.
// Any FDs received are closed since the caller doesn't expect them.
func (p *FDPasser) ReceiveMessage(bufSize int) ([]byte, error) {
	data, fds, err := p.ReceiveFileDescriptors(bufSize)
	if err != nil {
		return nil, err
	}
	// Close any unexpected FDs
	for _, fd := range fds {
		fd.Close()
	}
	return data, nil
}

// ReadLine reads bytes until a newline is encountered, using recvmsg.
// Any FDs received are closed since line-reading doesn't expect them.
func (p *FDPasser) ReadLine() ([]byte, error) {
	var line []byte
	buf := make([]byte, 1)
	oobBuf := make([]byte, unix.CmsgSpace(1024))

	for {
		n, oobn, _, _, err := p.conn.ReadMsgUnix(buf, oobBuf)
		if err != nil {
			return nil, fmt.Errorf("failed to read: %w", err)
		}

		// Close any FDs received during line reading
		if oobn > 0 {
			scms, _ := unix.ParseSocketControlMessage(oobBuf[:oobn])
			for _, scm := range scms {
				if scm.Header.Level == unix.SOL_SOCKET && scm.Header.Type == unix.SCM_RIGHTS {
					rawFds, _ := unix.ParseUnixRights(&scm)
					for _, fd := range rawFds {
						unix.Close(fd)
					}
				}
			}
		}

		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			break
		}
		line = append(line, buf[0])
	}
	return line, nil
}

// Close closes the underlying UNIX socket connection.
func (p *FDPasser) Close() error {
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}

// CreateSocketPair creates a pair of connected UNIX sockets that can be used
// for file descriptor passing. Returns the client and server connections.
func CreateSocketPair() (*net.UnixConn, *net.UnixConn, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create socket pair: %w", err)
	}

	// Convert raw file descriptors to *os.File and then to *net.UnixConn
	clientFile := os.NewFile(uintptr(fds[0]), "client")
	serverFile := os.NewFile(uintptr(fds[1]), "server")

	clientConn, err := net.FileConn(clientFile)
	if err != nil {
		clientFile.Close()
		serverFile.Close()
		return nil, nil, fmt.Errorf("failed to create client connection: %w", err)
	}

	serverConn, err := net.FileConn(serverFile)
	if err != nil {
		clientConn.Close()
		serverFile.Close()
		return nil, nil, fmt.Errorf("failed to create server connection: %w", err)
	}

	// Close the original files since the connections now own them
	clientFile.Close()
	serverFile.Close()

	clientUnix, ok := clientConn.(*net.UnixConn)
	if !ok {
		clientConn.Close()
		serverConn.Close()
		return nil, nil, fmt.Errorf("failed to cast client to UnixConn")
	}

	serverUnix, ok := serverConn.(*net.UnixConn)
	if !ok {
		clientConn.Close()
		serverConn.Close()
		return nil, nil, fmt.Errorf("failed to cast server to UnixConn")
	}

	return clientUnix, serverUnix, nil
}
