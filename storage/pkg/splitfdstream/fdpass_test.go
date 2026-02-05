//go:build linux

package splitfdstream

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateSocketPair(t *testing.T) {
	client, server, err := CreateSocketPair()
	if err != nil {
		t.Fatalf("Failed to create socket pair: %v", err)
	}
	defer client.Close()
	defer server.Close()

	// Test basic message passing without file descriptors
	clientPasser := NewFDPasser(client)
	serverPasser := NewFDPasser(server)

	testMessage := []byte("hello world")

	// Send from client to server
	err = clientPasser.SendMessage(testMessage)
	if err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	// Receive at server
	received, err := serverPasser.ReceiveMessage(1024)
	if err != nil {
		t.Fatalf("Failed to receive message: %v", err)
	}

	if string(received) != string(testMessage) {
		t.Errorf("Received message %q, want %q", received, testMessage)
	}
}

func TestSendReceiveFileDescriptors(t *testing.T) {
	client, server, err := CreateSocketPair()
	if err != nil {
		t.Fatalf("Failed to create socket pair: %v", err)
	}
	defer client.Close()
	defer server.Close()

	clientPasser := NewFDPasser(client)
	serverPasser := NewFDPasser(server)

	// Create a temporary file to send
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.txt")
	testContent := []byte("test file content")
	if err := os.WriteFile(tmpFile, testContent, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Open the file for reading
	file, err := os.Open(tmpFile)
	if err != nil {
		t.Fatalf("Failed to open test file: %v", err)
	}
	defer file.Close()

	// Send file descriptor with a message
	testMessage := []byte("file attached")
	err = clientPasser.SendFileDescriptors([]*os.File{file}, testMessage)
	if err != nil {
		t.Fatalf("Failed to send file descriptors: %v", err)
	}

	// Receive at server
	receivedMessage, receivedFds, err := serverPasser.ReceiveFileDescriptors(1024)
	if err != nil {
		t.Fatalf("Failed to receive file descriptors: %v", err)
	}

	// Check message
	if string(receivedMessage) != string(testMessage) {
		t.Errorf("Received message %q, want %q", receivedMessage, testMessage)
	}

	// Check file descriptors
	if len(receivedFds) != 1 {
		t.Fatalf("Expected 1 file descriptor, got %d", len(receivedFds))
	}

	receivedFile := receivedFds[0]
	defer receivedFile.Close()

	// Read content from received file descriptor
	receivedContent := make([]byte, len(testContent))
	n, err := receivedFile.Read(receivedContent)
	if err != nil {
		t.Fatalf("Failed to read from received file descriptor: %v", err)
	}

	if n != len(testContent) {
		t.Errorf("Read %d bytes, want %d", n, len(testContent))
	}

	if string(receivedContent) != string(testContent) {
		t.Errorf("Received file content %q, want %q", receivedContent, testContent)
	}
}

func TestSendMultipleFileDescriptors(t *testing.T) {
	client, server, err := CreateSocketPair()
	if err != nil {
		t.Fatalf("Failed to create socket pair: %v", err)
	}
	defer client.Close()
	defer server.Close()

	clientPasser := NewFDPasser(client)
	serverPasser := NewFDPasser(server)

	// Create multiple temporary files
	tmpDir := t.TempDir()
	var files []*os.File
	var expectedContents [][]byte

	for i := 0; i < 3; i++ {
		tmpFile := filepath.Join(tmpDir, "test"+string(rune('0'+i))+".txt")
		content := []byte("content " + string(rune('0'+i)))
		expectedContents = append(expectedContents, content)

		if err := os.WriteFile(tmpFile, content, 0644); err != nil {
			t.Fatalf("Failed to create test file %d: %v", i, err)
		}

		file, err := os.Open(tmpFile)
		if err != nil {
			t.Fatalf("Failed to open test file %d: %v", i, err)
		}
		defer file.Close()
		files = append(files, file)
	}

	// Send multiple file descriptors
	testMessage := []byte("multiple files attached")
	err = clientPasser.SendFileDescriptors(files, testMessage)
	if err != nil {
		t.Fatalf("Failed to send file descriptors: %v", err)
	}

	// Receive at server
	receivedMessage, receivedFds, err := serverPasser.ReceiveFileDescriptors(1024)
	if err != nil {
		t.Fatalf("Failed to receive file descriptors: %v", err)
	}

	// Check message
	if string(receivedMessage) != string(testMessage) {
		t.Errorf("Received message %q, want %q", receivedMessage, testMessage)
	}

	// Check file descriptors
	if len(receivedFds) != len(files) {
		t.Fatalf("Expected %d file descriptors, got %d", len(files), len(receivedFds))
	}

	// Verify each file descriptor content
	for i, receivedFile := range receivedFds {
		defer receivedFile.Close()

		content := make([]byte, len(expectedContents[i]))
		n, err := receivedFile.Read(content)
		if err != nil {
			t.Errorf("Failed to read from received file descriptor %d: %v", i, err)
			continue
		}

		if n != len(expectedContents[i]) {
			t.Errorf("File %d: read %d bytes, want %d", i, n, len(expectedContents[i]))
			continue
		}

		if string(content) != string(expectedContents[i]) {
			t.Errorf("File %d: received content %q, want %q", i, content, expectedContents[i])
		}
	}
}

func TestSendFileDescriptorsEmptyList(t *testing.T) {
	client, server, err := CreateSocketPair()
	if err != nil {
		t.Fatalf("Failed to create socket pair: %v", err)
	}
	defer client.Close()
	defer server.Close()

	clientPasser := NewFDPasser(client)

	// Try to send empty file descriptor list
	err = clientPasser.SendFileDescriptors([]*os.File{}, []byte("no files"))
	if err == nil {
		t.Errorf("Expected error when sending empty file descriptor list")
	}
}

func TestFDPasserClose(t *testing.T) {
	client, server, err := CreateSocketPair()
	if err != nil {
		t.Fatalf("Failed to create socket pair: %v", err)
	}
	defer server.Close()

	clientPasser := NewFDPasser(client)

	// Close the passer
	err = clientPasser.Close()
	if err != nil {
		t.Errorf("Failed to close FDPasser: %v", err)
	}

	// Try to use after close - should fail
	err = clientPasser.SendMessage([]byte("test"))
	if err == nil {
		t.Errorf("Expected error when using closed FDPasser")
	}
}
