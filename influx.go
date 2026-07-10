package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
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
		if err := w.write(ctx, body); err != nil {
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
			// Drain remaining queued results
			draining := true
			for draining {
				select {
				case r, ok := <-results:
					if !ok {
						draining = false
					} else {
						batch = append(batch, lineProtocol(r))
					}
				default:
					draining = false
				}
			}
			// Use a fresh context for the final write — ctx is already cancelled
			if len(batch) > 0 {
				body := strings.Join(batch, "\n")
				batch = batch[:0]
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := w.write(shutdownCtx, body); err != nil {
					w.logger.Printf("[InfluxWriter] shutdown flush error: %v", err)
				}
			}
			return
		}
	}
}

// write posts a Line Protocol body to InfluxDB with exponential backoff retry.
func (w *InfluxWriter) write(ctx context.Context, body string) error {
	q := url.Values{"org": {w.cfg.Org}, "bucket": {w.cfg.Bucket}, "precision": {"ns"}}
	endpoint := strings.TrimRight(w.cfg.URL, "/") + "/api/v2/write?" + q.Encode()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(body))
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
		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			return nil
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		lastErr = fmt.Errorf("unexpected status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return fmt.Errorf("after 3 attempts: %w", lastErr)
}
