package docker

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestDecodeLogStream(t *testing.T) {
	stream := append(logFrame(1, []byte("stdout\n")), logFrame(2, []byte("stderr\n"))...)
	got, truncated, err := decodeLogStream(bytes.NewReader(stream), 1024)
	if err != nil {
		t.Fatalf("decodeLogStream() error = %v", err)
	}
	if truncated {
		t.Fatal("decodeLogStream() unexpectedly truncated output")
	}
	if string(got) != "stdout\nstderr\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestDecodeLogStreamTruncatesAndConsumesFrames(t *testing.T) {
	stream := append(logFrame(1, []byte("12345")), logFrame(2, []byte("67890"))...)
	got, truncated, err := decodeLogStream(bytes.NewReader(stream), 7)
	if err != nil {
		t.Fatalf("decodeLogStream() error = %v", err)
	}
	if !truncated {
		t.Fatal("decodeLogStream() did not report truncation")
	}
	if string(got) != "1234567" {
		t.Fatalf("output = %q, want %q", got, "1234567")
	}
}

func TestDecodeLogStreamRejectsPartialFrame(t *testing.T) {
	_, _, err := decodeLogStream(bytes.NewReader([]byte{1, 0, 0}), 100)
	if err == nil {
		t.Fatal("decodeLogStream() accepted a partial header")
	}
}

func TestWriteBuildContext(t *testing.T) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- writeBuildContext(writer, "FROM scratch\n", []byte("binary")) }()
	contents, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading context: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("writeBuildContext() error = %v", err)
	}
	if !bytes.Contains(contents, []byte("Dockerfile")) || !bytes.Contains(contents, []byte("binary")) {
		t.Fatal("build context did not contain expected files")
	}
}

func logFrame(stream byte, contents []byte) []byte {
	frame := make([]byte, 8+len(contents))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(contents)))
	copy(frame[8:], contents)
	return frame
}
