package scheduler
import (
	"github.com/baekenough/second-brain/internal/store"
)
var _ ActiveDocumentCounter = (*store.DocumentStore)(nil)
