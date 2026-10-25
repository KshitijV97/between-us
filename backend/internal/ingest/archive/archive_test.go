package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kshitijv97/chat-analyzer/backend/internal/ingest"
	// Registers the Telegram parser for the end-to-end test.
	_ "github.com/kshitijv97/chat-analyzer/backend/internal/ingest/telegram"
)

// entry describes one file to put in a test zip. Zips are generated per test
// and never committed (.gitignore blocks *.zip).
type entry struct {
	name    string
	data    []byte
	mode    fs.FileMode // 0 = regular file
	deflate bool
}

func writeZip(t *testing.T, entries []entry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.name, Method: zip.Store}
		if e.deflate {
			fh.Method = zip.Deflate
		}
		if e.mode != 0 {
			fh.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, "upload.bin", buf.Bytes())
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readSource(t *testing.T, src ingest.Source) string {
	t.Helper()
	rc, err := src.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const chatText = "[31/12/23, 21:41:05] Alice: hi\n[31/12/23, 21:42:00] Bob: hello\n"

func TestWhatsAppIOSZip(t *testing.T) {
	path := writeZip(t, []entry{
		{name: "IMG-20240101-WA0001.jpg", data: []byte("jpg")},
		{name: "_chat.txt", data: []byte(chatText), deflate: true},
		{name: "PTT-20240101-WA0002.opus", data: []byte("opus")},
		{name: "STK-20240101-WA0003.webp", data: []byte("webp")},
		{name: "VID-20240101-WA0004.mp4", data: []byte("mp4")},
		{name: "plan.pdf", data: []byte("pdf")},
		{name: "Carol.vcf", data: []byte("vcf")},
		{name: "weird.xyz", data: []byte("?")},
		{name: "evil.exe", data: []byte("MZ")},
		{name: "inner.zip", data: []byte("PK")},
		{name: "__MACOSX/._chat.txt", data: []byte("junk")},
		{name: "folder/", mode: fs.ModeDir | 0o755},
	})
	up, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	if !up.IsZip || up.EntryName != "_chat.txt" || up.Source.Name() != "_chat.txt" || up.Source.Size() != int64(len(chatText)) {
		t.Fatalf("upload %+v", up)
	}
	// The source is re-openable: two-pass parsers read it twice.
	for range 2 {
		if readSource(t, up.Source) != chatText {
			t.Error("chat entry content mismatch")
		}
	}
	want := map[ingest.MediaType]int{
		ingest.MediaImage: 1, ingest.MediaVoiceNote: 1, ingest.MediaSticker: 1, ingest.MediaVideo: 1,
		ingest.MediaDocument: 1, ingest.MediaContact: 1, ingest.MediaUnknown: 1,
	}
	if len(up.MediaCounts) != len(want) {
		t.Errorf("counts %v", up.MediaCounts)
	}
	for k, v := range want {
		if up.MediaCounts[k] != v {
			t.Errorf("count[%s] = %d, want %d", k, up.MediaCounts[k], v)
		}
	}
	if len(up.Media) != 7 || up.Media[0] != (MediaItem{Name: "IMG-20240101-WA0001.jpg", Size: 3, Type: ingest.MediaImage}) {
		t.Errorf("media %+v", up.Media)
	}
	codes := map[string]int{}
	for _, d := range up.Diagnostics {
		codes[d.Code]++
		if strings.Contains(d.Detail, "evil") || strings.Contains(d.Detail, "inner") {
			t.Errorf("diagnostic leaks a file name: %q", d.Detail)
		}
	}
	if codes["executable_entry_ignored"] != 1 || codes["nested_archive_ignored"] != 1 {
		t.Errorf("diagnostics %+v", up.Diagnostics)
	}
}

func TestAndroidZipSingleTxt(t *testing.T) {
	path := writeZip(t, []entry{
		{name: "WhatsApp Chat with Bob.txt", data: []byte(chatText)},
		{name: "IMG-1.jpg", data: []byte("x")},
	})
	up, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	if up.EntryName != "WhatsApp Chat with Bob.txt" {
		t.Errorf("entry %q", up.EntryName)
	}
}

func TestTelegramZipParsesEndToEnd(t *testing.T) {
	js := `{"name":"Bob","type":"personal_chat","id":1,"messages":[` +
		`{"id":1,"type":"message","date":"2024-01-01T10:00:00","date_unixtime":"1704103200","from":"Alice","from_id":"user1","text":"hi"}]}`
	path := writeZip(t, []entry{
		{name: "ChatExport_2024-01-01/", mode: fs.ModeDir | 0o755},
		{name: "ChatExport_2024-01-01/photos/photo_1.jpg", data: []byte("jpg")},
		{name: "ChatExport_2024-01-01/result.json", data: []byte(js), deflate: true},
		{name: "ChatExport_2024-01-01/other.json", data: []byte("{}")},
	})
	up, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	if up.EntryName != "ChatExport_2024-01-01/result.json" || up.MediaCounts[ingest.MediaImage] != 1 {
		t.Fatalf("upload %+v", up)
	}
	sample, err := ingest.ReadSample(up.Source)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := ingest.Detect(sample)
	if err != nil || p.Platform() != ingest.PlatformTelegram {
		t.Fatalf("detect: %v %v", p, err)
	}
	rep, err := p.Parse(context.Background(), up.Source, ingest.Options{}, discardSink{})
	if err != nil || rep.Messages != 1 {
		t.Fatalf("parse: %+v %v", rep, err)
	}
}

type discardSink struct{}

func (discardSink) Batch(context.Context, []ingest.Message) error { return nil }
func (discardSink) Diagnostic(ingest.Diagnostic)                  {}

func TestAmbiguousAndExplicitEntry(t *testing.T) {
	path := writeZip(t, []entry{
		{name: "b.txt", data: []byte("b")},
		{name: "a.txt", data: []byte("a")},
	})
	_, err := Open(path, DefaultLimits())
	var amb *AmbiguousEntryError
	if !errors.As(err, &amb) || !errors.Is(err, ErrAmbiguousChatEntry) || strings.Join(amb.Candidates, ",") != "a.txt,b.txt" {
		t.Fatalf("err = %v", err)
	}
	up, err := OpenEntry(path, "b.txt", DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	if readSource(t, up.Source) != "b" || len(up.Media) != 1 {
		t.Errorf("explicit entry: %+v", up)
	}
	if _, err := OpenEntry(path, "missing.txt", DefaultLimits()); !errors.Is(err, ErrNoChatEntry) {
		t.Errorf("missing entry err = %v", err)
	}
}

func TestNoChatEntry(t *testing.T) {
	path := writeZip(t, []entry{{name: "IMG-1.jpg", data: []byte("x")}})
	if _, err := Open(path, DefaultLimits()); !errors.Is(err, ErrNoChatEntry) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnsafeNames(t *testing.T) {
	for _, name := range []string{"../evil.txt", "a/../../evil.txt", "/etc/passwd.txt", `dir\evil.txt`, "C:/evil.txt", "nul\x00.txt"} {
		path := writeZip(t, []entry{{name: "_chat.txt", data: []byte("x")}, {name: name, data: []byte("x")}})
		if _, err := Open(path, DefaultLimits()); !errors.Is(err, ErrUnsafeEntry) {
			t.Errorf("%q: err = %v, want ErrUnsafeEntry", name, err)
		}
	}
}

func TestSymlinkRejected(t *testing.T) {
	path := writeZip(t, []entry{
		{name: "_chat.txt", data: []byte("x")},
		{name: "link", data: []byte("/etc/passwd"), mode: fs.ModeSymlink | 0o777},
	})
	if _, err := Open(path, DefaultLimits()); !errors.Is(err, ErrUnsafeEntry) {
		t.Fatalf("err = %v", err)
	}
}

func TestZipBombRatio(t *testing.T) {
	path := writeZip(t, []entry{{name: "_chat.txt", data: make([]byte, 8<<20), deflate: true}})
	if _, err := Open(path, DefaultLimits()); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("err = %v", err)
	}
	// Small, highly compressible entries are fine.
	path = writeZip(t, []entry{{name: "_chat.txt", data: bytes.Repeat([]byte("ha"), 1000), deflate: true}})
	up, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatalf("small compressible entry rejected: %v", err)
	}
	up.Close()
}

func TestTooManyEntriesAndDeclaredTotal(t *testing.T) {
	var es []entry
	for i := range 6 {
		es = append(es, entry{name: string(rune('a'+i)) + ".jpg", data: []byte("x")})
	}
	es = append(es, entry{name: "_chat.txt", data: []byte("x")})
	lim := DefaultLimits()
	lim.MaxEntries = 5
	if _, err := Open(writeZip(t, es), lim); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("entries err = %v", err)
	}

	lim = DefaultLimits()
	lim.MaxTotalUncompressed = 100
	if _, err := Open(writeZip(t, []entry{{name: "_chat.txt", data: make([]byte, 200)}}), lim); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("total err = %v", err)
	}
}

// TestLyingDeclaredSize stores more bytes than the central directory declares;
// reading must fail rather than produce unbounded output.
func TestLyingDeclaredSize(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 3<<20)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "_chat.txt", Method: zip.Store,
		CompressedSize64: uint64(len(payload)), UncompressedSize64: 10,
		CRC32: crc32.ChecksumIEEE(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	up, err := Open(writeFile(t, "lying.zip", buf.Bytes()), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	rc, err := up.Source.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if err == nil {
		t.Fatalf("read %d bytes without error", n)
	}
}

func TestLimitedReadCloser(t *testing.T) {
	l := &limitedReadCloser{rc: io.NopCloser(strings.NewReader("0123456789")), remaining: 4}
	b, err := io.ReadAll(l)
	if !errors.Is(err, ErrLimitExceeded) || len(b) > 5 {
		t.Fatalf("read %q err %v", b, err)
	}
	l = &limitedReadCloser{rc: io.NopCloser(strings.NewReader("0123")), remaining: 4}
	if b, err := io.ReadAll(l); err != nil || string(b) != "0123" {
		t.Fatalf("exact-limit read %q err %v", b, err)
	}
}

func TestPlainFilePassthrough(t *testing.T) {
	// Extension is irrelevant: a .zip name with text content is plain…
	p := writeFile(t, "chat.zip", []byte(chatText))
	up, err := Open(p, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if up.IsZip || readSource(t, up.Source) != chatText || up.Source.Name() != "chat.zip" || up.Close() != nil {
		t.Errorf("plain upload %+v", up)
	}
	// …and zip content named .txt is a zip.
	zp := writeZip(t, []entry{{name: "_chat.txt", data: []byte(chatText)}})
	renamed := filepath.Join(filepath.Dir(zp), "looks-like.txt")
	if err := os.Rename(zp, renamed); err != nil {
		t.Fatal(err)
	}
	up, err = Open(renamed, DefaultLimits())
	if err != nil || !up.IsZip {
		t.Fatalf("zip named .txt: %+v %v", up, err)
	}
	up.Close()

	// Empty files and tiny files are plain.
	up, err = Open(writeFile(t, "empty.txt", nil), DefaultLimits())
	if err != nil || up.IsZip {
		t.Errorf("empty: %+v %v", up, err)
	}
	if _, err := Open(t.TempDir(), DefaultLimits()); !errors.Is(err, ErrNotRegularFile) {
		t.Errorf("directory err = %v", err)
	}
}

func TestCorruptZip(t *testing.T) {
	p := writeFile(t, "bad.zip", []byte("PK\x03\x04garbage"))
	if _, err := Open(p, DefaultLimits()); err == nil {
		t.Fatal("expected error for corrupt zip")
	}
}
