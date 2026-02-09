package chrootarchive

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/fileutils"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/splitfdstream"
	"go.podman.io/storage/pkg/system"
	"go.podman.io/storage/pkg/unshare"
	"golang.org/x/sys/unix"
)

// splitFDStreamSocketDescriptor is the fd for the Unix socket used to
// receive file descriptors via SCM_RIGHTS in the re-exec child.
const splitFDStreamSocketDescriptor = 5

func init() {
	reexec.Register("storage-untar-splitfdstream", untarSplitFDStream)
}

// UntarSplitFDStream extracts a splitfdstream into dest inside a chroot.
// The stream provides tar headers as inline data, with file content
// delivered via the fds slice (for reflink-based copying).
// FDs are streamed to the child process one-at-a-time over a Unix socket
// using SCM_RIGHTS, avoiding EMFILE from inheriting too many FDs at exec.
func UntarSplitFDStream(stream io.Reader, fds []*os.File, dest string, options *archive.TarOptions) error {
	if stream == nil {
		return fmt.Errorf("empty stream")
	}
	if options == nil {
		options = &archive.TarOptions{}
		options.InUserNS = unshare.IsRootless()
	}

	idMappings := idtools.NewIDMappingsFromMaps(options.UIDMaps, options.GIDMaps)
	rootIDs := idMappings.RootPair()

	dest = filepath.Clean(dest)
	if err := fileutils.Exists(dest); os.IsNotExist(err) {
		if err := idtools.MkdirAllAndChownNew(dest, 0o755, rootIDs); err != nil {
			return err
		}
	}

	destVal, err := newUnpackDestination(dest, dest)
	if err != nil {
		return err
	}
	defer destVal.Close()

	return invokeUnpackSplitFDStream(stream, fds, destVal, options)
}

// untarSplitFDStream is the re-exec entry point for "storage-untar-splitfdstream".
// It runs inside a chroot and receives FDs lazily via SCM_RIGHTS from a Unix
// socket, then calls archive.UnpackFromIterator for full extraction logic.
func untarSplitFDStream() {
	runtime.LockOSThread()
	flag.Parse()

	var options archive.TarOptions

	// Read TarOptions from fd 3 (same as regular untar)
	if err := json.NewDecoder(os.NewFile(tarOptionsDescriptor, "options")).Decode(&options); err != nil {
		fatal(err)
	}

	dst := flag.Arg(0)
	var root string
	if len(flag.Args()) > 1 {
		root = flag.Arg(1)
	}

	// Handle the root fd (same pattern as regular untar)
	if root == procPathForFd(rootFileDescriptor) {
		rootFd := os.NewFile(rootFileDescriptor, "tar-root")
		defer rootFd.Close()
		if err := unix.Fchdir(int(rootFd.Fd())); err != nil {
			fatal(err)
		}
		root = "."
	} else if root == "" {
		root = dst
	}

	if err := chroot(root); err != nil {
		fatal(err)
	}

	// We need to be able to set any perms
	oldMask, err := system.Umask(0)
	if err != nil {
		fatal(err)
	}
	defer func() {
		_, _ = system.Umask(oldMask)
	}()

	if unshare.IsRootless() {
		options.InUserNS = true
	}

	// Set up FD receiver from the Unix socket (fd 5)
	sockFile := os.NewFile(splitFDStreamSocketDescriptor, "fd-socket")
	sockConn, err := net.FileConn(sockFile)
	sockFile.Close() // FileConn dups the fd
	if err != nil {
		fatal(fmt.Errorf("failed to create net.Conn from fd socket: %w", err))
	}
	unixConn, ok := sockConn.(*net.UnixConn)
	if !ok {
		sockConn.Close()
		fatal(fmt.Errorf("fd socket is not a Unix connection"))
	}
	defer unixConn.Close()

	fdPasser := splitfdstream.NewFDPasser(unixConn)

	// Create an iterator that receives FDs lazily via SCM_RIGHTS
	recv := func() (*os.File, error) {
		_, fds, err := fdPasser.ReceiveFileDescriptors(1)
		if err != nil {
			return nil, fmt.Errorf("failed to receive FD via SCM_RIGHTS: %w", err)
		}
		if len(fds) != 1 {
			// Close any unexpected FDs
			for _, f := range fds {
				f.Close()
			}
			return nil, fmt.Errorf("expected 1 FD, got %d", len(fds))
		}
		return fds[0], nil
	}

	iter := splitfdstream.NewIteratorWithFDReceiver(os.Stdin, recv)
	if err := archive.UnpackFromIterator(iter, dst, &options); err != nil {
		fatal(err)
	}

	// Fully consume stdin in case it is zero padded
	if _, err := flush(os.Stdin); err != nil {
		fatal(err)
	}

	os.Exit(0)
}

// invokeUnpackSplitFDStream forks a re-exec child process that chroots into
// dest and unpacks the splitfdstream using archive.UnpackFromIterator.
// FDs are sent to the child one-at-a-time over a Unix socket using SCM_RIGHTS.
func invokeUnpackSplitFDStream(stream io.Reader, fds []*os.File, dest *unpackDestination, options *archive.TarOptions) error {
	// Create pipe for TarOptions (fd 3)
	optR, optW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("splitfdstream options pipe: %w", err)
	}

	// Create Unix socketpair for passing FDs via SCM_RIGHTS
	sockFDs, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		optR.Close()
		optW.Close()
		return fmt.Errorf("splitfdstream socketpair: %w", err)
	}
	parentSockFile := os.NewFile(uintptr(sockFDs[0]), "fd-socket-parent")
	childSockFile := os.NewFile(uintptr(sockFDs[1]), "fd-socket-child")

	cmd := reexec.Command("storage-untar-splitfdstream", dest.dest, procPathForFd(rootFileDescriptor))
	cmd.Stdin = stream

	// ExtraFiles: [optionsPipe(fd3), rootFD(fd4), socketEnd(fd5)]
	cmd.ExtraFiles = append(cmd.ExtraFiles, optR)          // fd 3
	cmd.ExtraFiles = append(cmd.ExtraFiles, dest.root)     // fd 4
	cmd.ExtraFiles = append(cmd.ExtraFiles, childSockFile) // fd 5

	output := bytes.NewBuffer(nil)
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Start(); err != nil {
		optW.Close()
		optR.Close()
		parentSockFile.Close()
		childSockFile.Close()
		return fmt.Errorf("splitfdstream untar error on re-exec cmd: %w", err)
	}

	// Close the child's end in the parent
	childSockFile.Close()

	// Write TarOptions JSON to the pipe
	if err := json.NewEncoder(optW).Encode(options); err != nil {
		optW.Close()
		parentSockFile.Close()
		return fmt.Errorf("splitfdstream options json encode failed: %w", err)
	}
	optW.Close()

	// Send FDs one-at-a-time over the socket using SCM_RIGHTS.
	// The child receives them lazily as it processes external chunks.
	parentConn, err := net.FileConn(parentSockFile)
	parentSockFile.Close() // FileConn dups the fd
	if err != nil {
		return fmt.Errorf("splitfdstream parent socket: %w", err)
	}
	parentUnix, ok := parentConn.(*net.UnixConn)
	if !ok {
		parentConn.Close()
		return fmt.Errorf("splitfdstream parent socket is not Unix")
	}

	fdPasser := splitfdstream.NewFDPasser(parentUnix)
	for _, f := range fds {
		// Send one FD with a 1-byte dummy message (required by sendmsg)
		if err := fdPasser.SendFileDescriptors([]*os.File{f}, []byte{0}); err != nil {
			parentUnix.Close()
			return fmt.Errorf("splitfdstream send FD: %w", err)
		}
	}
	parentUnix.Close() // signal EOF to child

	if err := cmd.Wait(); err != nil {
		// Exhaust input to avoid blocking the producer
		if _, discardErr := io.Copy(io.Discard, stream); discardErr != nil {
			return fmt.Errorf("splitfdstream unpacking failed (error: %w; output: %s)\nexhausting input failed (error: %w)", err, output, discardErr)
		}
		return fmt.Errorf("splitfdstream unpacking failed (error: %w; output: %s)", err, output)
	}
	return nil
}
