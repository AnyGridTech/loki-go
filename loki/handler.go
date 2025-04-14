package loki

import (
	"context"
	"log/slog"
	"os"

	"github.com/prometheus/common/model"
)

type LokiHandler struct {
	client  *Client
	level   slog.Level
	labels  model.LabelSet
	handler slog.Handler
}

func NewLokiHandler(client *Client, level slog.Level, labels model.LabelSet) *LokiHandler {
	textHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return &LokiHandler{
		client:  client,
		level:   level,
		labels:  labels,
		handler: textHandler,
	}
}

func (h *LokiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *LokiHandler) Handle(ctx context.Context, record slog.Record) error {
	if err := h.handler.Handle(ctx, record); err != nil {
		return err
	}
	// Create a copy of the labels to avoid overwriting shared labels
	labels := make(model.LabelSet, len(h.labels))
	for k, v := range h.labels {
		labels[k] = v
	}

	labels["level"] = model.LabelValue(record.Level.String())
	// Extract the log message
	message := record.Message

	// Send the log entry to Loki
	return h.client.Handle(labels, record.Time, message)
}

func (h *LokiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Add attributes to the handler (optional)
	return h
}

func (h *LokiHandler) WithGroup(name string) slog.Handler {
	// Add group context to the handler (optional)
	return h
}
