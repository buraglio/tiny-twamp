package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

// lineProtocol formats a ProbeResult as a single InfluxDB v2 Line Protocol line.
// Format: measurement,tag1=v1,tag2=v2 field1=f1,field2=f2 timestamp
func lineProtocol(r ProbeResult) string {
	escape := func(s string) string {
		s = strings.ReplaceAll(s, " ", `\ `)
		s = strings.ReplaceAll(s, ",", `\,`)
		s = strings.ReplaceAll(s, "=", `\=`)
		return s
	}

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

	tags := fmt.Sprintf("source=%s,target=%s,topology=%s,site=%s",
		escape(r.Source),
		escape(r.Target),
		escape(r.Topology),
		escape(r.Site),
	)

	fields := fmt.Sprintf(
		"rtt_min_ms=%.3f,rtt_avg_ms=%.3f,rtt_max_ms=%.3f,rtt_stddev_ms=%.3f,jitter_ms=%.3f,loss_pct=%.3f,packets_sent=%di,packets_recv=%di",
		ms(r.RttMin),
		ms(r.RttAvg),
		ms(r.RttMax),
		ms(r.RttStddev),
		ms(r.Jitter),
		r.LossPct,
		r.Sent,
		r.Recv,
	)

	return fmt.Sprintf("twamp_rtt,%s %s %d", tags, fields, r.SentAt.UnixNano())
}

// InfluxWriter batches ProbeResults and flushes to InfluxDB.
type InfluxWriter struct {
	cfg    InfluxConfig
	logger *log.Logger
	client *http.Client
}

func newInfluxWriter(cfg InfluxConfig, logger *log.Logger) *InfluxWriter {
	return &InfluxWriter{
		cfg:    cfg,
		logger: logger,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// run reads ProbeResults from the channel, batches them, and flushes to InfluxDB.
// Exits when ctx is cancelled and results channel is drained.
func (w *InfluxWriter) run(ctx context.Context, results <-chan ProbeResult) {
	const maxBatch = 100
	flushInterval := 10 * time.Second
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var batch []string

	flush := func() {
		if len(batch) == 0 {
			return
		}
		body := strings.Join(batch, "\n")
		batch = batch[:0]
		if err := w.write(body); err != nil {
			w.logger.Printf("[InfluxWriter] write error: %v", err)
		}
	}

	for {
		select {
		case r, ok := <-results:
			if !ok {
				flush()
				return
			}
			batch = append(batch, lineProtocol(r))
			if len(batch) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}

// write posts a Line Protocol body to InfluxDB with exponential backoff retry.
func (w *InfluxWriter) write(body string) error {
	url := fmt.Sprintf("%s/api/v2/write?org=%s&bucket=%s&precision=ns",
		w.cfg.URL, w.cfg.Org, w.cfg.Bucket)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			time.Sleep(wait)
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Token "+w.cfg.Token)
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")

		resp, err := w.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP error: %w", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return fmt.Errorf("after 3 attempts: %w", lastErr)
}
