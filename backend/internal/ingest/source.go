package ingest

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// FileSource is a Source backed by a file on disk.
type FileSource struct {
	Path string
}

// Open implements Source.
func (s FileSource) Open() (io.ReadCloser, error) { return os.Open(s.Path) }

// Name implements Source; only the base name is exposed.
func (s FileSource) Name() string { return filepath.Base(s.Path) }

// Size implements Source.
func (s FileSource) Size() int64 {
	fi, err := os.Stat(s.Path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// BytesSource is an in-memory Source, used by tests and small uploads.
type BytesSource struct {
	Data     []byte
	FileName string
}

// Open implements Source.
func (s BytesSource) Open() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.Data)), nil
}

// Name implements Source.
func (s BytesSource) Name() string { return s.FileName }

// Size implements Source.
func (s BytesSource) Size() int64 { return int64(len(s.Data)) }

// ReadSample returns up to SampleSize bytes from the start of src for Detect.
func ReadSample(src Source) ([]byte, error) {
	rc, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := make([]byte, SampleSize)
	n, err := io.ReadFull(rc, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}
