package collector

import (
	"context"
	"time"

	"github.com/baekenough/second-brain/internal/model"
)

// TelegramCollector is a placeholder for future Telegram data collection.
// It implements the Collector interface but is always disabled.
type TelegramCollector struct{}

// NewTelegramCollector returns a TelegramCollector.
func NewTelegramCollector() *TelegramCollector {
	return &TelegramCollector{}
}

func (c *TelegramCollector) Name() string {
	return "telegram"
}

func (c *TelegramCollector) Source() model.SourceType {
	return model.SourceType("telegram")
}

func (c *TelegramCollector) Enabled() bool {
	return false
}

func (c *TelegramCollector) Collect(_ context.Context, _ time.Time) ([]model.Document, error) {
	return nil, nil
}
