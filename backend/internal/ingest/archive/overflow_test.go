package archive

import (
	"archive/zip"
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Declared sizes in a zip are attacker-controlled. Two entries whose declared
// uncompressed sizes sum past 2^64 used to wrap the running total back under
// the limit; the check must compare before adding.
func TestDeclaredSizeOverflowIsRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	chat, err := zw.Create("_chat.txt")
	if err != nil {
		t.Fatal(err)
	}
	// 2,000 bytes of chat: with the huge entry below, the declared sum is
	// 2000 + (2^64 - 1001) = 2^64 + 999, which wraps to 999 in uint64.
	body := bytes.Repeat([]byte("01/02/2024, 10:00 - Alice: hi there\n"), 60)[:2000]
	if _, err := chat.Write(body); err != nil {
		t.Fatal(err)
	}
	// A raw entry whose header claims an enormous size with a modest ratio,
	// so only the total-size check can catch it.
	huge := uint64(math.MaxUint64 - 1000)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "photo.jpg", Method: zip.Store,
		CompressedSize64: huge / 2, UncompressedSize64: huge,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "crafted.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	up, err := Open(path, DefaultLimits())
	if err == nil {
		up.Close()
		t.Fatal("crafted zip with overflowing declared sizes was accepted")
	}
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("err = %v, want ErrLimitExceeded", err)
	}
}
