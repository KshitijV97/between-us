// Package archive opens an uploaded export file safely. A zip (WhatsApp
// "Include media" or iOS exports, a zipped Telegram ChatExport folder) is read
// from its central directory without extracting anything to disk; a plain
// .txt/.json file passes straight through.
//
// Hardening (docs/parsers/whatsapp.md §1.1, docs/08-security-privacy.md §5):
//   - the container type is decided by magic bytes, never by extension;
//   - limits on entry count, declared total size and per-entry compression
//     ratio are enforced before any entry body is read;
//   - names that are absolute, contain "..", backslashes or NUL, and symlink
//     entries reject the whole upload (a genuine export never has them);
//   - reading the chat entry goes through a counting reader, because declared
//     sizes in a zip can lie;
//   - media is only enumerated (FR-ING-005), executables and nested archives
//     are reported in diagnostics and otherwise ignored.
package archive

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/kshitijv/chat-analyzer/backend/internal/ingest"
)

// Limits bound what an upload may contain.
type Limits struct {
	// MaxEntries is the maximum number of entries in a zip.
	MaxEntries int
	// MaxTotalUncompressed bounds the sum of declared uncompressed sizes, and
	// the bytes actually read from the chat entry.
	MaxTotalUncompressed int64
	// MaxRatio is the maximum uncompressed:compressed ratio for an entry
	// larger than ratioFloor.
	MaxRatio int64
}

// DefaultLimits returns the documented defaults.
func DefaultLimits() Limits {
	return Limits{MaxEntries: 20_000, MaxTotalUncompressed: 2 << 30, MaxRatio: 100}
}

// ratioFloor exempts small entries from the ratio check: tiny files (a short
// chat, a text full of repeated characters) legitimately compress far better
// than 100:1 and cannot do harm.
const ratioFloor = 1 << 20

// Errors returned by Open. Use errors.Is / errors.As.
var (
	// ErrLimitExceeded means the upload breaks a size, count or ratio limit.
	ErrLimitExceeded = errors.New("archive: upload exceeds safety limits")
	// ErrUnsafeEntry means the zip contains a dangerous name or a symlink.
	ErrUnsafeEntry = errors.New("archive: unsafe zip entry")
	// ErrNoChatEntry means the zip contains no chat text or JSON file.
	ErrNoChatEntry = errors.New("archive: no chat file found in zip")
	// ErrAmbiguousChatEntry is wrapped by *AmbiguousEntryError.
	ErrAmbiguousChatEntry = errors.New("archive: more than one possible chat file in zip")
	// ErrNotRegularFile means the path is a directory or device.
	ErrNotRegularFile = errors.New("archive: not a regular file")
)

// AmbiguousEntryError lists the candidate chat files so the user can choose
// one (pass it to OpenEntry).
type AmbiguousEntryError struct {
	Candidates []string
}

func (e *AmbiguousEntryError) Error() string {
	return fmt.Sprintf("archive: %d possible chat files in zip; choose one", len(e.Candidates))
}

// Unwrap returns ErrAmbiguousChatEntry.
func (e *AmbiguousEntryError) Unwrap() error { return ErrAmbiguousChatEntry }

// MediaItem is one enumerated media entry. Name is user data (P2 inventory,
// FR-ING-020), never logged.
type MediaItem struct {
	Name string
	Size int64
	Type ingest.MediaType
}

// Upload is an opened export. Close releases the underlying file.
type Upload struct {
	// Source is the chat file: a zip entry or the plain file itself.
	Source ingest.Source
	// IsZip reports whether the upload was a zip archive.
	IsZip bool
	// EntryName is the selected zip entry ("" for plain files).
	EntryName string
	// Media lists media entries in archive order; MediaCounts aggregates them.
	Media       []MediaItem
	MediaCounts map[ingest.MediaType]int
	// Diagnostics are content-free findings (ignored executables, nested
	// archives, OS metadata files).
	Diagnostics []ingest.Diagnostic

	closer io.Closer
}

// Close releases the file handle.
func (u *Upload) Close() error {
	if u.closer == nil {
		return nil
	}
	return u.closer.Close()
}

// zipMagic are the local-file-header and empty-archive signatures.
var zipMagic = [][]byte{[]byte("PK\x03\x04"), []byte("PK\x05\x06")}

// Open opens path, auto-selecting the chat entry inside a zip.
func Open(filePath string, lim Limits) (*Upload, error) {
	return OpenEntry(filePath, "", lim)
}

// OpenEntry is Open with an explicit zip entry name, used after an
// *AmbiguousEntryError. entry is ignored for plain files.
func OpenEntry(filePath, entry string, lim Limits) (*Upload, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrNotRegularFile
	}
	isZip, err := sniffZip(filePath)
	if err != nil {
		return nil, err
	}
	if !isZip {
		return &Upload{Source: ingest.FileSource{Path: filePath}, MediaCounts: map[ingest.MediaType]int{}}, nil
	}
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return nil, fmt.Errorf("archive: unreadable zip: %w", err)
	}
	up, err := fromZip(&zr.Reader, entry, lim)
	if err != nil {
		_ = zr.Close() // the validation error is the one worth reporting
		return nil, err
	}
	up.closer = zr
	return up, nil
}

func sniffZip(filePath string) (bool, error) {
	f, err := os.Open(filePath) // #nosec G304 -- the caller chose the upload path
	if err != nil {
		return false, err
	}
	defer f.Close()
	var head [4]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	for _, m := range zipMagic {
		if n == 4 && bytes.Equal(head[:], m) {
			return true, nil
		}
	}
	return false, nil
}

// fromZip validates the central directory and builds the Upload.
func fromZip(zr *zip.Reader, entry string, lim Limits) (*Upload, error) {
	if lim == (Limits{}) {
		lim = DefaultLimits()
	}
	if len(zr.File) > lim.MaxEntries {
		return nil, fmt.Errorf("%w: %d entries (max %d)", ErrLimitExceeded, len(zr.File), lim.MaxEntries)
	}

	// Pass 1: validate every entry before reading any body.
	var total uint64
	// Limits as unsigned values for comparison with zip header fields;
	// negative limits (misconfiguration) are treated as zero.
	maxTotal := uint64(max(lim.MaxTotalUncompressed, 0))
	maxRatio := uint64(max(lim.MaxRatio, 0))
	for _, f := range zr.File {
		if err := checkName(f.Name); err != nil {
			return nil, err
		}
		if f.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: symlink entry", ErrUnsafeEntry)
		}
		// Compare before adding: declared sizes are attacker-controlled, and
		// a plain sum could wrap around uint64 and slip under the limit.
		if f.UncompressedSize64 > maxTotal-total {
			return nil, fmt.Errorf("%w: declared uncompressed size over %d bytes", ErrLimitExceeded, lim.MaxTotalUncompressed)
		}
		total += f.UncompressedSize64
		if total > maxTotal {
			return nil, fmt.Errorf("%w: declared uncompressed size over %d bytes", ErrLimitExceeded, lim.MaxTotalUncompressed)
		}
		if f.UncompressedSize64 > ratioFloor && (f.CompressedSize64 == 0 || f.UncompressedSize64/f.CompressedSize64 > maxRatio) {
			return nil, fmt.Errorf("%w: compression ratio above %d:1", ErrLimitExceeded, lim.MaxRatio)
		}
	}

	up := &Upload{IsZip: true, MediaCounts: map[ingest.MediaType]int{}}
	chat, err := selectChat(zr.File, entry)
	if err != nil {
		return nil, err
	}
	up.EntryName = chat.Name
	up.Source = entrySource{f: chat, limit: entryLimit(chat, lim)}

	// Pass 2: inventory everything else.
	for _, f := range zr.File {
		if f == chat || f.FileInfo().IsDir() {
			continue
		}
		if osMetadata(f.Name) {
			continue
		}
		ext := strings.ToLower(path.Ext(f.Name))
		switch {
		case executableExt[ext]:
			up.Diagnostics = append(up.Diagnostics, ingest.Diagnostic{
				Severity: ingest.SeverityWarning, Code: "executable_entry_ignored",
				Detail: "an executable or script file (" + ext + ") in the zip was ignored",
			})
			continue
		case archiveExt[ext]:
			up.Diagnostics = append(up.Diagnostics, ingest.Diagnostic{
				Severity: ingest.SeverityInfo, Code: "nested_archive_ignored",
				Detail: "a nested archive (" + ext + ") in the zip was not opened",
			})
			continue
		}
		mt := mediaTypeForExt(ext)
		up.Media = append(up.Media, MediaItem{Name: f.Name, Size: saturatingInt64(f.UncompressedSize64), Type: mt})
		up.MediaCounts[mt]++
	}
	return up, nil
}

// checkName rejects zip-slip and otherwise malformed entry names.
func checkName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty name", ErrUnsafeEntry)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: NUL in name", ErrUnsafeEntry)
	case strings.Contains(name, `\`):
		return fmt.Errorf("%w: backslash in name", ErrUnsafeEntry)
	case strings.HasPrefix(name, "/") || (len(name) > 1 && name[1] == ':'):
		return fmt.Errorf("%w: absolute path", ErrUnsafeEntry)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return fmt.Errorf("%w: parent directory reference", ErrUnsafeEntry)
		}
	}
	return nil
}

// selectChat picks the chat entry: an explicit name; else _chat.txt; else the
// single .txt; else result.json; else the single .json
// (docs/parsers/whatsapp.md §1.1 step 2).
func selectChat(files []*zip.File, entry string) (*zip.File, error) {
	var txt, jsn []*zip.File
	for _, f := range files {
		if f.FileInfo().IsDir() || osMetadata(f.Name) {
			continue
		}
		if entry != "" {
			if f.Name == entry {
				return f, nil
			}
			continue
		}
		switch strings.ToLower(path.Ext(f.Name)) {
		case ".txt":
			txt = append(txt, f)
		case ".json":
			jsn = append(jsn, f)
		}
	}
	if entry != "" {
		return nil, fmt.Errorf("%w: entry %q not found", ErrNoChatEntry, entry)
	}
	for _, f := range txt {
		if path.Base(f.Name) == "_chat.txt" {
			return f, nil
		}
	}
	if pick, err := single(txt); pick != nil || err != nil {
		return pick, err
	}
	for _, f := range jsn {
		if path.Base(f.Name) == "result.json" {
			return f, nil
		}
	}
	if pick, err := single(jsn); pick != nil || err != nil {
		return pick, err
	}
	return nil, ErrNoChatEntry
}

// single returns the only file, nil when empty, or an ambiguity error.
func single(fs []*zip.File) (*zip.File, error) {
	switch len(fs) {
	case 0:
		return nil, nil
	case 1:
		return fs[0], nil
	}
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = f.Name
	}
	sort.Strings(names)
	return nil, &AmbiguousEntryError{Candidates: names}
}

// osMetadata skips macOS resource forks and Finder metadata added by zipping
// on a Mac.
func osMetadata(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(base, "._") || base == ".DS_Store"
}

// entryLimit bounds bytes read from one entry: MaxRatio times its compressed
// size (but at least ratioFloor, so small entries are never starved), capped
// by the total budget. archive/zip also rejects reads past the declared size;
// this is defence in depth against a lying central directory.
func entryLimit(f *zip.File, lim Limits) int64 {
	c := saturatingInt64(f.CompressedSize64)
	limit := int64(math.MaxInt64)
	if lim.MaxRatio > 0 && c <= math.MaxInt64/lim.MaxRatio { // avoid overflow
		limit = c * lim.MaxRatio
	}
	limit = max(limit, ratioFloor)
	return min(limit, lim.MaxTotalUncompressed)
}

// entrySource is a re-openable ingest.Source over one zip entry.
type entrySource struct {
	f     *zip.File
	limit int64
}

// Open implements ingest.Source. Each call decompresses from the start.
func (s entrySource) Open() (io.ReadCloser, error) {
	rc, err := s.f.Open()
	if err != nil {
		return nil, fmt.Errorf("archive: open entry: %w", err)
	}
	return &limitedReadCloser{rc: rc, remaining: s.limit}, nil
}

// Name implements ingest.Source with the entry's base name.
func (s entrySource) Name() string { return path.Base(s.f.Name) }

// Size implements ingest.Source with the declared uncompressed size.
func (s entrySource) Size() int64 { return saturatingInt64(s.f.UncompressedSize64) }

// saturatingInt64 converts a zip header size to int64, clamping values that
// do not fit (headers are attacker-controlled).
func saturatingInt64(u uint64) int64 {
	if u > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(u)
}

// limitedReadCloser fails (rather than silently truncating) once more than
// the allowed number of bytes has been produced.
type limitedReadCloser struct {
	rc        io.ReadCloser
	remaining int64
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	if l.remaining < 0 {
		return 0, fmt.Errorf("%w: entry decompressed beyond its limit", ErrLimitExceeded)
	}
	if int64(len(p)) > l.remaining+1 {
		p = p[:l.remaining+1]
	}
	n, err := l.rc.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return n, fmt.Errorf("%w: entry decompressed beyond its limit", ErrLimitExceeded)
	}
	return n, err
}

func (l *limitedReadCloser) Close() error { return l.rc.Close() }

// Extension tables. WhatsApp names media like IMG-20240101-WA0001.jpg,
// PTT-…opus (voice notes), STK-…webp (stickers).
var (
	executableExt = setOf(".exe", ".msi", ".bat", ".cmd", ".com", ".scr", ".ps1", ".vbs", ".sh", ".bash",
		".js", ".jar", ".apk", ".app", ".dmg", ".dll", ".so", ".dylib", ".py", ".pl", ".rb", ".php", ".lnk")
	archiveExt = setOf(".zip", ".rar", ".7z", ".tar", ".gz", ".tgz", ".bz2", ".xz")
	mediaByExt = map[string]ingest.MediaType{
		".jpg": ingest.MediaImage, ".jpeg": ingest.MediaImage, ".png": ingest.MediaImage, ".heic": ingest.MediaImage,
		".heif": ingest.MediaImage, ".bmp": ingest.MediaImage, ".tif": ingest.MediaImage, ".tiff": ingest.MediaImage,
		".webp": ingest.MediaSticker, ".tgs": ingest.MediaSticker,
		".gif": ingest.MediaGIF,
		".mp4": ingest.MediaVideo, ".mov": ingest.MediaVideo, ".3gp": ingest.MediaVideo, ".mkv": ingest.MediaVideo,
		".webm": ingest.MediaVideo, ".avi": ingest.MediaVideo, ".m4v": ingest.MediaVideo,
		".opus": ingest.MediaVoiceNote,
		".mp3":  ingest.MediaAudio, ".m4a": ingest.MediaAudio, ".aac": ingest.MediaAudio, ".wav": ingest.MediaAudio,
		".ogg": ingest.MediaAudio, ".amr": ingest.MediaAudio, ".flac": ingest.MediaAudio,
		".vcf": ingest.MediaContact,
		".pdf": ingest.MediaDocument, ".doc": ingest.MediaDocument, ".docx": ingest.MediaDocument,
		".xls": ingest.MediaDocument, ".xlsx": ingest.MediaDocument, ".ppt": ingest.MediaDocument,
		".pptx": ingest.MediaDocument, ".txt": ingest.MediaDocument, ".csv": ingest.MediaDocument,
		".odt": ingest.MediaDocument, ".rtf": ingest.MediaDocument, ".json": ingest.MediaDocument,
		".html": ingest.MediaDocument,
	}
)

func mediaTypeForExt(ext string) ingest.MediaType {
	if t, ok := mediaByExt[ext]; ok {
		return t
	}
	return ingest.MediaUnknown
}

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}
