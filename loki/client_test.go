package loki

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/AnyGridTech/loki-go/v2/pkg/backoff"
	"github.com/AnyGridTech/loki-go/v2/pkg/urlutil"
	"github.com/grafana/loki/pkg/push"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
)

func TestNewClient(t *testing.T) {
	var uri urlutil.URLValue
	err := uri.Set("http://localhost:3100/loki/api/v1/push")
	if err != nil {
		t.Fatalf("Failed to set URL: %v", err)
	}
	cfg := Config{
		URL: uri,
	}
	client, err := New(cfg)
	assert.NoError(t, err)
	assert.NotNil(t, client)
}

func TestHandle(t *testing.T) {
	var uri urlutil.URLValue
	err := uri.Set("http://localhost:3100/loki/api/v1/push")
	if err != nil {
		t.Fatalf("Failed to set URL: %v", err)
	}
	cfg := Config{
		URL: uri,
	}
	client, err := New(cfg)
	assert.NoError(t, err)

	labels := model.LabelSet{"job": "test"}
	timestamp := time.Now()
	line := "test log line"

	err = client.Handle(labels, timestamp, line)
	assert.NoError(t, err)
}

func TestSendBatch(t *testing.T) {
	var uri urlutil.URLValue
	err := uri.Set("http://localhost:3100/loki/api/v1/push")
	if err != nil {
		t.Fatalf("Failed to set URL: %v", err)
	}
	cfg := Config{
		BatchWait: time.Second,
		URL:       uri,
	}
	client, err := New(cfg)
	assert.NoError(t, err)

	batch := newBatch(entry{
		tenantID: "test-tenant",
		labels:   model.LabelSet{"job": "test"},
		Entry: push.Entry{
			Timestamp: time.Now(),
			Line:      "test log line",
		},
	})

	client.sendBatch("test-tenant", batch)
}
func TestStopClient(t *testing.T) {
	var uri urlutil.URLValue
	err := uri.Set("http://localhost:3100/loki/api/v1/push")
	if err != nil {
		t.Fatalf("Failed to set URL: %v", err)
	}
	cfg := Config{
		BackoffConfig: backoff.BackoffConfig{
			MinBackoff: 100 * time.Millisecond,
			MaxBackoff: 10 * time.Second,
			MaxRetries: 10,
		},
		BatchWait: time.Second,
		BatchSize: 1024 * 1024, // 1MB
		Timeout:   10 * time.Second,

		URL: uri,
	}
	client, err := New(cfg)
	assert.NoError(t, err)

	client.Stop()
	assert.NotPanics(t, func() { client.Stop() })
}
func TestSendLogsToLokiUsingClient(t *testing.T) {
	// Define the Loki endpoint
	var uri urlutil.URLValue
	err := uri.Set("http://localhost:3100/loki/api/v1/push")
	if err != nil {
		t.Fatalf("Failed to set URL: %v", err)
	}
	// Create a client configuration
	cfg := Config{
		BackoffConfig: backoff.BackoffConfig{
			MinBackoff: 100 * time.Millisecond,
			MaxBackoff: 10 * time.Second,
			MaxRetries: 10,
		},
		BatchWait: time.Second,
		BatchSize: 256 * 1024, // 1MB
		Timeout:   10 * time.Second,

		URL: uri,
	}

	// Initialize the client
	client, err := New(cfg)
	assert.NoError(t, err)
	assert.NotNil(t, client)

	// Define log labels and log entry
	labels := model.LabelSet{"job": "test-job"}
	timestamp := time.Now()
	line := "Test log line from Go test using custom client"
	// Send the log entry using the client
	err = client.Handle(labels, timestamp, line)
	assert.NoError(t, err)

	// Stop the client after the test
	client.Stop()
}
func TestBatchStress(t *testing.T) {
	var uri urlutil.URLValue
	err := uri.Set("http://localhost:3100/loki/api/v1/push")
	if err != nil {
		t.Fatalf("Failed to set URL: %v", err)
	}
	cfg := Config{
		BackoffConfig: backoff.BackoffConfig{
			MinBackoff: 100 * time.Millisecond,
			MaxBackoff: 10 * time.Second,
			MaxRetries: 10,
		},
		BatchWait: 100 * time.Millisecond,
		BatchSize: 10 * 1024, // 10KB
		Timeout:   10 * time.Second,
		URL:       uri,
	}

	client, err := New(cfg)
	assert.NoError(t, err)
	defer client.Stop()

	// Simulate a high number of log entries
	numEntries := 10000
	for i := 0; i < numEntries; i++ {
		labels := model.LabelSet{"job": "stress-test"}
		timestamp := time.Now()
		line := fmt.Sprintf("log line %d", i)

		err := client.Handle(labels, timestamp, line)
		assert.NoError(t, err)
	}

	// Allow some time for batches to be processed
	time.Sleep(2 * time.Second)

	// Ensure no entries are left in the channel
	assert.Equal(t, 0, len(client.entries))
}
func TestSlogWrapperSendLogsToLoki(t *testing.T) {
	// Define the Loki endpoint
	var uri urlutil.URLValue
	err := uri.Set("http://localhost:3100/loki/api/v1/push")
	if err != nil {
		t.Fatalf("Failed to set URL: %v", err)
	}
	// Create a client configuration
	cfg := Config{URL: uri}
	labels := model.LabelSet{
		"api": "example-api", // Add custom labels as needed
	}
	// Initialize the client
	client, err := New(cfg)
	assert.NoError(t, err)
	assert.NotNil(t, client)
	defer client.Stop()
	logger := slog.New(NewLokiHandler(client, slog.LevelInfo, labels))
	// Wrap the slog.Logger

	// Log messages using the wrapped logger
	logger.Info("Test log message 1")
	logger.Error("Test log message 2")
	logger.Warn("Test log message 3")

	// Allow some time for logs to be processed
	time.Sleep(2 * time.Second)

	// Ensure no entries are left in the channel
	assert.Equal(t, 0, len(client.entries))
}
