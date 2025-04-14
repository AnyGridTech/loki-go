// Package loki provides a client implementation for sending log entries to a Loki server.
// It supports batching, retries with backoff, and metrics for monitoring the client's behavior.
//
// The Client struct is the main component of this package, responsible for managing log entries,
// batching them, and sending them to the Loki server. It also provides support for multi-tenancy
// and external labels.
//
// Metrics:
// - encoded_bytes_total: Number of bytes encoded and ready to send.
// - sent_bytes_total: Number of bytes successfully sent.
// - dropped_bytes_total: Number of bytes dropped due to failed retries.
// - sent_entries_total: Number of log entries successfully sent.
// - dropped_entries_total: Number of log entries dropped due to failed retries.
// - request_duration_seconds: Duration of send requests.
// - batch_retries_total: Number of times batches have been retried.
// - promtail_stream_lag_seconds: Difference between current time and last batch timestamp for successful sends.
//
// Key Features:
// - Batching: Log entries are batched based on size and time constraints.
// - Retry with Backoff: Failed requests are retried with exponential backoff.
// - Multi-Tenancy: Supports sending logs to multiple tenants using the X-Scope-OrgID header.
// - External Labels: Allows adding external labels to log entries.
// - Metrics: Exposes Prometheus metrics for monitoring the client's behavior.
//
// Usage:
// - Use New or NewWithLogger to create a new Client instance.
// - Call Handle or HandleWithMetadata to add log entries to the client.
// - Call Stop to gracefully stop the client and send any remaining batches.
//
// Configuration:
// - Config struct allows customization of the client's behavior, including batch size, batch wait time,
//   timeout, and backoff settings. Defaults are applied if not explicitly set.
//
// Example:
//   cfg := loki.Config{
//       URL: &url.URL{Host: "loki.example.com"},
//       BatchSize: 1024 * 1024, // 1MB
//       BatchWait: 1 * time.Second,
//   }
//   client, err := loki.New(cfg)
//   if err != nil {
//       log.Fatalf("failed to create loki client: %v", err)
//   }
//   defer client.Stop()
//
//   labels := model.LabelSet{"job": "example"}
//   client.Handle(labels, time.Now(), "This is a log entry")
package loki

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/AnyGridTech/loki-go/pkg/backoff"
	"github.com/AnyGridTech/loki-go/pkg/metric"
	"github.com/grafana/loki/pkg/push"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/version"
	"github.com/prometheus/prometheus/promql/parser"
)

const (
	protoContentType = "application/x-protobuf"
	JSONContentType  = "application/json"
	maxErrMsgLen     = 1024

	// Label reserved to override the tenant ID while processing
	// pipeline stages
	ReservedLabelTenantID = "__tenant_id__"

	LatencyLabel = "filename"
	HostLabel    = "host"
)

var (
	encodedBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "encoded_bytes_total",
		Help:      "Number of bytes encoded and ready to send.",
	}, []string{HostLabel})
	sentBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "sent_bytes_total",
		Help:      "Number of bytes sent.",
	}, []string{HostLabel})
	droppedBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "dropped_bytes_total",
		Help:      "Number of bytes dropped because failed to be sent to the ingester after all retries.",
	}, []string{HostLabel})
	sentEntries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "sent_entries_total",
		Help:      "Number of log entries sent to the ingester.",
	}, []string{HostLabel})
	droppedEntries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "dropped_entries_total",
		Help:      "Number of log entries dropped because failed to be sent to the ingester after all retries.",
	}, []string{HostLabel})
	requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "promtail",
		Name:      "request_duration_seconds",
		Help:      "Duration of send requests.",
	}, []string{"status_code", HostLabel})
	batchRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "batch_retries_total",
		Help:      "Number of times batches has had to be retried.",
	}, []string{HostLabel})
	streamLag *metric.Gauges

	countersWithHost = []*prometheus.CounterVec{
		encodedBytes, sentBytes, droppedBytes, sentEntries, droppedEntries,
	}

	UserAgent = fmt.Sprintf("promtail/%s", version.Version)
)

func init() {
	prometheus.MustRegister(encodedBytes)
	prometheus.MustRegister(sentBytes)
	prometheus.MustRegister(droppedBytes)
	prometheus.MustRegister(sentEntries)
	prometheus.MustRegister(droppedEntries)
	prometheus.MustRegister(requestDuration)
	prometheus.MustRegister(batchRetries)
	var err error
	streamLag, err = metric.NewGauges("promtail_stream_lag_seconds",
		"Difference between current time and last batch timestamp for successful sends",
		metric.GaugeConfig{Action: "set"},
		int64(1*time.Minute.Seconds()), // This strips out files which update slowly and reduces noise in this metric.
	)
	if err != nil {
		panic(err)
	}
	prometheus.MustRegister(streamLag)
}

type Client struct {
	logger         *slog.Logger
	cfg            Config
	client         *http.Client
	once           sync.Once
	entries        chan entry
	quit           chan struct{}
	wg             sync.WaitGroup
	externalLabels model.LabelSet
}

type entry struct {
	tenantID string
	labels   model.LabelSet
	push.Entry
}

// New makes a new Client from config
func New(cfg Config) (*Client, error) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return NewWithLogger(cfg, logger)
}

// NewWithDefault creates a new client with default configuration.
func NewWithDefault(url string) (*Client, error) {
	cfg, err := NewDefaultConfig(url)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

// NewWithLogger makes a new Client from a logger and a config
func NewWithLogger(cfg Config, logger *slog.Logger) (*Client, error) {
	if cfg.URL.URL == nil {
		return nil, errors.New("client needs target URL")
	}
	applyDefaults(&cfg)
	c := &Client{
		logger:  slog.With(logger, "component", "client", "host", cfg.URL.Host),
		cfg:     cfg,
		quit:    make(chan struct{}),
		entries: make(chan entry, 1000),

		externalLabels: cfg.ExternalLabels.LabelSet,
	}

	err := cfg.Client.Validate()
	if err != nil {
		return nil, err
	}

	c.client, err = config.NewClientFromConfig(cfg.Client, "promtail", config.WithHTTP2Disabled())
	if err != nil {
		return nil, err
	}

	c.client.Timeout = cfg.Timeout

	// Initialize counters to 0 so the metrics are exported before the first
	// occurrence of incrementing to avoid missing metrics.
	for _, counter := range countersWithHost {
		counter.WithLabelValues(c.cfg.URL.Host).Add(0)
	}

	c.wg.Add(1)
	go c.run()
	return c, nil
}

func (c *Client) run() {
	batches := map[string]*batch{}

	// Given the client handles multiple batches (1 per tenant) and each batch
	// can be created at a different point in time, we look for batches whose
	// max wait time has been reached every 10 times per BatchWait, so that the
	// maximum delay we have sending batches is 10% of the max waiting time.
	// We apply a cap of 10ms to the ticker, to avoid too frequent checks in
	// case the BatchWait is very low.
	minWaitCheckFrequency := 100 * time.Millisecond
	maxWaitCheckFrequency := c.cfg.BatchWait / 10
	if maxWaitCheckFrequency < minWaitCheckFrequency {
		maxWaitCheckFrequency = minWaitCheckFrequency
	}

	maxWaitCheck := time.NewTicker(maxWaitCheckFrequency)

	defer func() {
		// Send all pending batches
		for tenantID, batch := range batches {
			c.sendBatch(tenantID, batch)
		}

		c.wg.Done()
	}()

	for {
		select {
		case <-c.quit:
			return

		case e := <-c.entries:
			batch, ok := batches[e.tenantID]

			// If the batch doesn't exist yet, we create a new one with the entry
			if !ok {
				batches[e.tenantID] = newBatch(e)
				break
			}

			// If adding the entry to the batch will increase the size over the max
			// size allowed, we do send the current batch and then create a new one
			if batch.sizeBytesAfter(e) > c.cfg.BatchSize {
				c.sendBatch(e.tenantID, batch)

				batches[e.tenantID] = newBatch(e)
				break
			}

			// The max size of the batch isn't reached, so we can add the entry
			batch.add(e)

		case <-maxWaitCheck.C:
			// Send all batches whose max wait time has been reached
			for tenantID, batch := range batches {
				if batch.age() < c.cfg.BatchWait {
					continue
				}

				c.sendBatch(tenantID, batch)
				delete(batches, tenantID)
			}
		}
	}
}

func (c *Client) sendBatch(tenantID string, batch *batch) {
	var (
		err          error
		buf          []byte
		entriesCount int
	)
	if c.cfg.EncodeJson {
		buf, entriesCount, err = batch.encodeJSON()
	} else {
		buf, entriesCount, err = batch.encode()
	}

	if err != nil {
		c.logger.Error("error encoding batch", slog.String("error", err.Error()))
		return
	}
	bufBytes := float64(len(buf))
	encodedBytes.WithLabelValues(c.cfg.URL.Host).Add(bufBytes)

	ctx := context.Background()
	backoff := backoff.New(ctx, c.cfg.BackoffConfig)
	var status int
	for backoff.Ongoing() {
		start := time.Now()
		status, err = c.send(ctx, tenantID, buf)
		requestDuration.WithLabelValues(strconv.Itoa(status), c.cfg.URL.Host).Observe(time.Since(start).Seconds())

		if err == nil {
			sentBytes.WithLabelValues(c.cfg.URL.Host).Add(bufBytes)
			sentEntries.WithLabelValues(c.cfg.URL.Host).Add(float64(entriesCount))
			for _, s := range batch.streams {
				lbls, err := parser.ParseMetric(s.Labels)
				if err != nil {
					// is this possible?
					c.logger.Warn("error converting stream label string to label.Labels, cannot update lagging metric", slog.String("error", err.Error()))
					return
				}
				var lblSet model.LabelSet
				for i := range lbls {
					if lbls[i].Name == LatencyLabel {
						lblSet = model.LabelSet{
							model.LabelName(HostLabel):    model.LabelValue(c.cfg.URL.Host),
							model.LabelName(LatencyLabel): model.LabelValue(lbls[i].Value),
						}
					}
				}
				if lblSet != nil {
					streamLag.With(lblSet).Set(time.Since(s.Entries[len(s.Entries)-1].Timestamp).Seconds())
				}
			}
			return
		}

		// Only retry 429s, 500s and connection-level errors.
		if status > 0 && status != 429 && status/100 != 5 {
			break
		}
		c.logger.Warn("error sending batch, will retry", "status", status, slog.String("error", err.Error()))
		batchRetries.WithLabelValues(c.cfg.URL.Host).Inc()
		backoff.Wait()
	}

	if err != nil {
		c.logger.Error("final error sending batch", slog.String("status", strconv.Itoa(status)), slog.String("error", err.Error()))
		droppedBytes.WithLabelValues(c.cfg.URL.Host).Add(bufBytes)
		droppedEntries.WithLabelValues(c.cfg.URL.Host).Add(float64(entriesCount))
	}
}

func (c *Client) send(ctx context.Context, tenantID string, buf []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequest("POST", c.cfg.URL.String(), bytes.NewReader(buf))
	if err != nil {
		return -1, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", protoContentType)
	if c.cfg.EncodeJson {
		req.Header.Set("Content-Type", JSONContentType)
	}
	req.Header.Set("User-Agent", UserAgent)

	// If the tenant ID is not empty promtail is running in multi-tenant mode, so
	// we should send it to Loki
	if tenantID != "" {
		req.Header.Set("X-Scope-OrgID", tenantID)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return -1, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Error("error closing response body", slog.String("error", err.Error()))
		}
	}()

	if resp.StatusCode/100 != 2 {
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxErrMsgLen))
		line := ""
		if scanner.Scan() {
			line = scanner.Text()
		}
		err = fmt.Errorf("server returned HTTP status %s (%d): %s", resp.Status, resp.StatusCode, line)
	}
	return resp.StatusCode, err
}

func (c *Client) getTenantID(labels model.LabelSet) string {
	// Check if it has been overridden while processing the pipeline stages
	if value, ok := labels[ReservedLabelTenantID]; ok {
		return string(value)
	}

	// Check if has been specified in the config
	if c.cfg.TenantID != "" {
		return c.cfg.TenantID
	}

	// Defaults to an empty string, which means the X-Scope-OrgID header
	// will not be sent
	return ""
}

// Stop the client.
func (c *Client) Stop() {
	c.once.Do(func() { close(c.quit) })
	c.wg.Wait()
}

// Handle implement EntryHandler; adds a new line to the next batch; send is async.
func (c *Client) Handle(ls model.LabelSet, t time.Time, s string) error {
	ls, tenantID := mergeLabels(c, ls)

	c.entries <- entry{tenantID, ls, push.Entry{
		Timestamp: t,
		Line:      s,
	}}
	return nil
}

// Handle implement EntryHandler; adds a new line to the next batch; send is async.
func (c *Client) HandleWithMetadata(ls model.LabelSet, t time.Time, s string, m push.LabelsAdapter) error {
	ls, tenantID := mergeLabels(c, ls)

	c.entries <- entry{tenantID, ls, push.Entry{
		Timestamp:          t,
		Line:               s,
		StructuredMetadata: m,
	}}
	return nil
}

func mergeLabels(c *Client, ls model.LabelSet) (model.LabelSet, string) {
	// Get the tenant  ID in case it has been overridden while processing
	// the pipeline stages, then remove the special label
	// Clone the label set to not manipulate the input one
	if len(c.externalLabels) > 0 {
		ls = c.externalLabels.Merge(ls)
	}

	tenantID := c.getTenantID(ls)
	if _, ok := ls[ReservedLabelTenantID]; ok {

		ls = ls.Clone()
		delete(ls, ReservedLabelTenantID)
	}
	return ls, tenantID
}

func (c *Client) UnregisterLatencyMetric(labels model.LabelSet) {
	labels[HostLabel] = model.LabelValue(c.cfg.URL.Host)
	streamLag.Delete(labels)
}

func applyDefaults(cfg *Config) {
    if cfg.Timeout == 0 {
        cfg.Timeout = 10 * time.Second
    }
    if cfg.BatchSize == 0 {
        cfg.BatchSize = 1024 * 1024 // 1MB
    }
    if cfg.BatchWait == 0 {
        cfg.BatchWait = 1 * time.Second
    }
    if cfg.BackoffConfig.MinBackoff == 0 {
        cfg.BackoffConfig.MinBackoff = 100 * time.Millisecond
    }
    if cfg.BackoffConfig.MaxBackoff == 0 {
        cfg.BackoffConfig.MaxBackoff = 10 * time.Second
    }
    if cfg.BackoffConfig.MaxRetries == 0 {
        cfg.BackoffConfig.MaxRetries = 5
    }
}