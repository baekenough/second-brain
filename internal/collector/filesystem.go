package collector

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/baekenough/second-brain/internal/collector/extractor"
	"github.com/baekenough/second-brain/internal/model"
)

const (
	// fileReadTimeout is the per-file read deadline for plain-text files to
	// avoid blocking on Google Drive virtual filesystem on-demand downloads.
	fileReadTimeout = 3 * time.Second

	// gworkspaceReadTimeout is the per-file read deadline for Google Workspace
	// link files, which are tiny; a shorter timeout is sufficient.
	gworkspaceReadTimeout = 2 * time.Second

	// maxTextFileBytes is the maximum file size for inline text content.
	// Larger files are indexed as metadata only.
	maxTextFileBytes = 512 * 1024 // 512 KB

	// maxGWorkspaceFileBytes is the maximum file size for Google Workspace
	// link files. These should be tiny; 0-byte or oversized files are virtual.
	maxGWorkspaceFileBytes = 1024 // 1 KB

	// extractTimeout is the per-file deadline for binary content extraction
	// (PDF, Office formats). Larger than fileReadTimeout because extraction
	// involves parsing, not just I/O.
	extractTimeout = 10 * time.Second

	// maxFilenameBytes is the maximum byte length of a single filename
	// component (not full path). POSIX, ext4, and 9p/virtio-fs all enforce a
	// 255-byte limit per component; filenames longer than this cause lstat(2)
	// to fail on those filesystems (e.g. minikube virtio-fs mounts).
	maxFilenameBytes = 255
)

const maxContentBytes = 1 << 20 // 1 MB

// skipDirs are directory names that should never be traversed.
var skipDirs = map[string]bool{
	".git":       true,
	"node_modules": true,
	"dist":       true,
	".next":      true,
	".omc":       true,
	".sisyphus":  true,
	".claude":    true,
}

// fullContentExts are extensions whose full text is read (up to maxContentBytes).
// Note: .html is intentionally excluded — it is handled by the extractor registry.
var fullContentExts = map[string]bool{
	".md":  true,
	".txt": true,
	".csv": true,
	".json": true,
	".js":  true,
	".ts":  true,
	".tsx": true,
	".py":  true,
	".sh":  true,
}

// extractorRegistry is the shared extractor registry for binary/structured formats.
var extractorRegistry = extractor.NewRegistry()

// gworkspaceExts are tiny Google Workspace link files.
var gworkspaceExts = map[string]bool{
	".gsheet":  true,
	".gdoc":    true,
	".gscript": true,
	".gslides": true,
	".gform":   true,
}

// skipExts are extensions that should be silently ignored.
var skipExts = map[string]bool{
	".bak":     true,
	".gitkeep": true,
	".plist":   true,
	".lock":    true,
	".DS_Store": true,
}

var urlRegexp = regexp.MustCompile(`https?://[^\s"']+`)

// FilesystemCollector walks a local directory tree and indexes files.
// It is strictly read-only; it never acquires file locks.
type FilesystemCollector struct {
	rootPath      string
	driveExporter *DriveExporter
}

// NewFilesystemCollector returns a FilesystemCollector rooted at rootPath.
func NewFilesystemCollector(rootPath string) *FilesystemCollector {
	return &FilesystemCollector{rootPath: rootPath}
}

// NewFilesystemCollectorWithDriveExport returns a FilesystemCollector that uses
// the provided DriveExporter to export Google Workspace file content. When
// exporter is nil (ADC not configured), the collector falls back to URL-only
// metadata for workspace stub files.
func NewFilesystemCollectorWithDriveExport(rootPath string, exporter *DriveExporter) *FilesystemCollector {
	return &FilesystemCollector{rootPath: rootPath, driveExporter: exporter}
}

func (c *FilesystemCollector) Name() string             { return "filesystem" }
func (c *FilesystemCollector) Source() model.SourceType { return model.SourceFilesystem }
func (c *FilesystemCollector) Enabled() bool            { return c.rootPath != "" }

// Collect walks the root directory and returns documents for files modified
// after since. Individual file errors are logged and skipped.
func (c *FilesystemCollector) Collect(_ context.Context, since time.Time) ([]model.Document, error) {
	start := time.Now()
	var docs []model.Document
	var skipped int

	err := filepath.WalkDir(c.rootPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			slog.Warn("filesystem: walk error, skipping", "path", path, "error", walkErr)
			skipped++
			return nil // continue
		}

		if isFilenameTooLong(d.Name()) {
			nameBytes := len(d.Name())
			if d.IsDir() {
				slog.Warn("filesystem: skipping directory with too-long name",
					"path", path, "bytes", nameBytes)
				return filepath.SkipDir
			}
			slog.Warn("filesystem: skipping file with too-long name",
				"path", path, "bytes", nameBytes)
			skipped++
			return nil
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			slog.Warn("filesystem: stat failed, skipping", "path", path, "error", err)
			skipped++
			return nil
		}

		if !info.ModTime().After(since) {
			return nil
		}

		doc, ok := c.buildDocument(path, info)
		if !ok {
			skipped++
			return nil
		}
		docs = append(docs, doc)
		if len(docs)%100 == 0 {
			slog.Info("filesystem: progress", "collected", len(docs), "skipped", skipped)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("filesystem: walk %q: %w", c.rootPath, err)
	}

	slog.Info("filesystem: collected documents",
		"count", len(docs),
		"skipped", skipped,
		"elapsed", time.Since(start).Round(time.Millisecond),
		"root", c.rootPath)
	return docs, nil
}

// ListActiveSourceIDs walks the entire root directory and returns the relative
// path of every indexable file, regardless of modification time. This allows
// the scheduler to detect files that were removed since the last collection run.
func (c *FilesystemCollector) ListActiveSourceIDs(_ context.Context) ([]string, error) {
	var ids []string

	err := filepath.WalkDir(c.rootPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			slog.Warn("filesystem: walk error during ID listing, skipping", "path", path, "error", walkErr)
			return nil
		}

		if isFilenameTooLong(d.Name()) {
			nameBytes := len(d.Name())
			if d.IsDir() {
				slog.Warn("filesystem: skipping directory with too-long name during ID listing",
					"path", path, "bytes", nameBytes)
				return filepath.SkipDir
			}
			slog.Warn("filesystem: skipping file with too-long name during ID listing",
				"path", path, "bytes", nameBytes)
			return nil
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			slog.Warn("filesystem: stat failed during ID listing, skipping", "path", path, "error", err)
			return nil
		}

		ext := strings.ToLower(filepath.Ext(info.Name()))
		if skipExts[ext] || info.Name() == ".DS_Store" {
			return nil
		}

		relPath, err := filepath.Rel(c.rootPath, path)
		if err != nil {
			relPath = path
		}
		ids = append(ids, relPath)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("filesystem: walk %q for ID listing: %w", c.rootPath, err)
	}

	return ids, nil
}

// buildDocument constructs a Document for a single file. Returns false when
// the file should be skipped entirely.
func (c *FilesystemCollector) buildDocument(absPath string, info fs.FileInfo) (model.Document, bool) {
	ext := strings.ToLower(filepath.Ext(info.Name()))

	if skipExts[ext] || info.Name() == ".DS_Store" {
		return model.Document{}, false
	}

	relPath, err := filepath.Rel(c.rootPath, absPath)
	if err != nil {
		relPath = absPath
	}

	content := c.extractContent(absPath, relPath, info, ext)

	return model.Document{
		ID:         uuid.New(),
		SourceType: model.SourceFilesystem,
		SourceID:   relPath,
		Title:      info.Name(),
		Content:    content,
		Metadata: map[string]any{
			"path": relPath,
			"ext":  ext,
			"size": info.Size(),
			"dir":  filepath.Dir(relPath),
		},
		CollectedAt: info.ModTime().UTC(),
	}, true
}

// extractContent returns the appropriate content string for the given file.
func (c *FilesystemCollector) extractContent(absPath, relPath string, info fs.FileInfo, ext string) string {
	switch {
	case fullContentExts[ext]:
		if info.Size() > maxTextFileBytes {
			return fmt.Sprintf("File: %s\nPath: %s\nSize: %s\n[large file — metadata only]",
				info.Name(), relPath, humanSize(info.Size()))
		}
		content := c.readTextContent(absPath)
		if content == "" {
			return fmt.Sprintf("File: %s\nPath: %s\nSize: %s\n[content unavailable]",
				info.Name(), relPath, humanSize(info.Size()))
		}
		return content

	case extractorRegistry.Find(ext) != nil:
		// HTML, PDF, Office formats — use the extractor registry.
		return c.extractWithRegistry(absPath, relPath, info, ext)

	case gworkspaceExts[ext]:
		// size == 0 means Google Drive streaming stub — metadata only to avoid download.
		if info.Size() == 0 {
			return fmt.Sprintf("File: %s\nPath: %s\n[Google Drive stub — metadata only]",
				info.Name(), relPath)
		}
		if info.Size() > maxGWorkspaceFileBytes {
			return fmt.Sprintf("File: %s\nPath: %s\nSize: %s\n[content too large for inline indexing]",
				info.Name(), relPath, humanSize(info.Size()))
		}
		return c.readGWorkspaceContent(absPath, relPath, info.Name())

	case isImageExt(ext):
		return fmt.Sprintf("Image: %s\nPath: %s\nSize: %s", info.Name(), relPath, humanSize(info.Size()))

	case isArchiveExt(ext):
		return fmt.Sprintf("Archive: %s\nPath: %s\nSize: %s", info.Name(), relPath, humanSize(info.Size()))

	default:
		// Unknown extension: metadata only.
		return fmt.Sprintf("File: %s\nPath: %s\nSize: %s", info.Name(), relPath, humanSize(info.Size()))
	}
}

// extractWithRegistry uses the extractor registry to extract text from
// structured/binary files (HTML, PDF, Office). On error it falls back to
// metadata-only content and logs a warning.
func (c *FilesystemCollector) extractWithRegistry(absPath, relPath string, info fs.FileInfo, ext string) string {
	ex := extractorRegistry.Find(ext)
	if ex == nil {
		return fmt.Sprintf("File: %s\nPath: %s\nSize: %s", info.Name(), relPath, humanSize(info.Size()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), extractTimeout)
	defer cancel()

	text, err := ex.Extract(ctx, absPath)
	if err != nil {
		slog.Warn("filesystem: extractor failed, using metadata only",
			"path", absPath, "ext", ext, "error", err)
		return fmt.Sprintf("File: %s\nPath: %s\nSize: %s\n[extraction failed]",
			info.Name(), relPath, humanSize(info.Size()))
	}
	if text == "" {
		return fmt.Sprintf("File: %s\nPath: %s\nSize: %s\n[no text content extracted]",
			info.Name(), relPath, humanSize(info.Size()))
	}
	// Apply the same maxContentBytes truncation used by readTextContent.
	if len(text) > maxContentBytes {
		truncated := []byte(text[:maxContentBytes])
		for !utf8.Valid(truncated) && len(truncated) > 0 {
			truncated = truncated[:len(truncated)-1]
		}
		text = string(truncated) + "\n[content truncated]"
	}
	return text
}

// readFileWithTimeout reads the file at path, returning an error if the read
// does not complete within timeout. This prevents blocking on Google Drive
// virtual filesystem files that require on-demand network downloads.
func readFileWithTimeout(path string, timeout time.Duration) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := os.ReadFile(path)
		ch <- result{data, err}
	}()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("read timeout after %s", timeout)
	}
}

// readTextContent reads up to maxContentBytes from a plain-text file.
// Returns an empty string on error (including timeout), allowing the caller
// to fall back to a metadata-only document.
func (c *FilesystemCollector) readTextContent(absPath string) string {
	data, err := readFileWithTimeout(absPath, fileReadTimeout)
	if err != nil {
		slog.Warn("filesystem: read failed, using metadata only", "path", absPath, "error", err)
		return ""
	}
	if len(data) > maxContentBytes {
		// Truncate at a valid UTF-8 boundary.
		truncated := data[:maxContentBytes]
		for !utf8.Valid(truncated) && len(truncated) > 0 {
			truncated = truncated[:len(truncated)-1]
		}
		return string(truncated) + "\n[content truncated]"
	}
	return string(data)
}

// readGWorkspaceContent reads a tiny Google Workspace link file and extracts any URL.
// When a DriveExporter is configured it also attempts to export the file's text
// content via the Drive API. On any failure it falls back to URL-only metadata.
func (c *FilesystemCollector) readGWorkspaceContent(absPath, relPath, filename string) string {
	data, err := readFileWithTimeout(absPath, gworkspaceReadTimeout)
	if err != nil {
		slog.Warn("filesystem: read gworkspace file failed, using metadata only", "path", absPath, "error", err)
		ext := strings.ToLower(filepath.Ext(filename))
		return fmt.Sprintf("%s: %s\nPath: %s\n[content unavailable]", gworkspaceTypeName(ext), filename, relPath)
	}

	docURL := ""
	if len(data) > 0 {
		if m := urlRegexp.Find(data); m != nil {
			docURL = string(m)
		}
	}

	ext := strings.ToLower(filepath.Ext(filename))
	typeName := gworkspaceTypeName(ext)
	header := fmt.Sprintf("%s: %s\nPath: %s\nURL: %s", typeName, filename, relPath, docURL)

	// Attempt Drive API export when the exporter is enabled.
	if c.driveExporter.Enabled() && len(data) > 0 {
		fileID := ExtractFileID(data)
		if fileID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), extractTimeout)
			defer cancel()

			exported, err := c.driveExporter.Export(ctx, fileID, ext)
			if err != nil {
				slog.Warn("filesystem: drive export failed, falling back to URL only",
					"path", absPath, "file_id", fileID, "error", err)
			} else if exported != "" {
				// Prepend header (title + URL) before the exported body.
				combined := header + "\n\n" + exported
				if len(combined) > maxContentBytes {
					truncated := []byte(combined[:maxContentBytes])
					for !utf8.Valid(truncated) && len(truncated) > 0 {
						truncated = truncated[:len(truncated)-1]
					}
					return string(truncated) + "\n[content truncated]"
				}
				return combined
			}
		}
	}

	return header
}

// gworkspaceTypeName returns the human-readable type name for a workspace extension.
func gworkspaceTypeName(ext string) string {
	switch ext {
	case ".gsheet":
		return "Google Sheet"
	case ".gdoc":
		return "Google Doc"
	case ".gscript":
		return "Google Apps Script"
	case ".gslides":
		return "Google Slides"
	case ".gform":
		return "Google Form"
	default:
		return "Google Workspace"
	}
}

// humanSize formats a byte count as a human-readable string (e.g. "1.2 MB").
func humanSize(bytes int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.0f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func isImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".svg":
		return true
	}
	return false
}

func isArchiveExt(ext string) bool {
	switch ext {
	case ".zip", ".apk", ".tar", ".gz":
		return true
	}
	return false
}

// isFilenameTooLong reports whether name (a single path component, not a full
// path) exceeds maxFilenameBytes bytes. POSIX, ext4, and 9p/virtio-fs all
// enforce a 255-byte per-component limit; attempting lstat(2) on a longer name
// fails with ENAMETOOLONG on those filesystems.
//
// Note: len(name) counts bytes, not Unicode code points. Korean characters are
// 3 bytes each in UTF-8, so a 100-character Korean filename is 300 bytes and
// therefore too long.
func isFilenameTooLong(name string) bool {
	return len(name) > maxFilenameBytes
}

// isBinaryExt and fileTypeName are kept for any future binary formats that
// are not handled by the extractor registry. PDF, xlsx, pptx, docx, and html
// have been moved to the registry and are no longer classified here.
