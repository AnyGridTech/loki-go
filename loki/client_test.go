package loki

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/AnyGridTech/loki-go/pkg/urlutil"
	"github.com/grafana/loki/pkg/push"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
)

var uri urlutil.URLValue

func TestNewClient(t *testing.T) {
	uri.URL = &url.URL{Scheme: "http", Host: "localhost:3100"}
	cfg := Config{
		URL: uri,
	}
	client, err := New(cfg)
	assert.NoError(t, err)
	assert.NotNil(t, client)
}

func TestHandle(t *testing.T) {
	uri.URL = &url.URL{Scheme: "http", Host: "localhost:3100"}

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
	uri.URL = &url.URL{Scheme: "http", Host: "localhost:3100"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/x-protobuf", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		URL: uri,
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
	uri.URL = &url.URL{Scheme: "http", Host: "localhost:3100"}
	cfg := Config{
		URL: uri,
	}
	client, err := New(cfg)
	assert.NoError(t, err)

	client.Stop()
	assert.NotPanics(t, func() { client.Stop() })
}
func TestSendLogsToLokiUsingClient(t *testing.T) {
    // Define the Loki endpoint
    uri.URL = &url.URL{Scheme: "http", Host: "localhost:3100", Path: "/loki/api/v1/push"}
    // Create a client configuration
    cfg := Config{
		BatchWait: time.Second,
		BatchSize: 1024 * 1024, // 1MB
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
