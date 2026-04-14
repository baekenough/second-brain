package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/baekenough/second-brain/internal/collector/extractor"
)

// registryExtractor adapts [extractor.Registry] to the [Extractor] interface.
// It looks up the correct extractor by file extension and calls Extract.
type registryExtractor struct {
	reg     *extractor.Registry
	timeout time.Duration
}

// NewRegistryExtractor returns an [Extractor] backed by the given
// [extractor.Registry]. Each extraction call is bounded by timeout; when
// timeout is zero, [extractor.ExtractTimeout] seconds are used.
func NewRegistryExtractor(reg *extractor.Registry, timeout time.Duration) Extractor {
	if timeout <= 0 {
		timeout = time.Duration(extractor.ExtractTimeout) * time.Second
	}
	return &registryExtractor{reg: reg, timeout: timeout}
}

// ExtractFromPath finds the extractor for path's extension and runs it.
// Returns an error when no extractor supports the extension.
func (r *registryExtractor) ExtractFromPath(ctx context.Context, path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	e := r.reg.Find(ext)
	if e == nil {
		return "", fmt.Errorf("no extractor for extension %q", ext)
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return e.Extract(ctx, path)
}
