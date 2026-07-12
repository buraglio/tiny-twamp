package main

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusStore holds all registered TWAMP metrics and their registry.
type PrometheusStore struct {
	rttMin    *prometheus.GaugeVec
	rttAvg    *prometheus.GaugeVec
	rttMax    *prometheus.GaugeVec
	rttStddev *prometheus.GaugeVec
	jitter    *prometheus.GaugeVec
	lossRatio *prometheus.GaugeVec
	pktSent   *prometheus.CounterVec
	pktRecv   *prometheus.CounterVec
	reflected *prometheus.CounterVec
	registry  *prometheus.Registry
	hostname  string
}

var probeLabels = []string{"source", "target", "topology", "site"}

func newPrometheusStore(hostname string) *PrometheusStore {
	reg := prometheus.NewRegistry()

	newGauge := func(name, help string) *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, probeLabels)
		reg.MustRegister(g)
		return g
	}
	newCounter := func(name, help string, labels []string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		reg.MustRegister(c)
		return c
	}

	s := &PrometheusStore{
		rttMin:    newGauge("twamp_rtt_min_milliseconds", "Minimum RTT in burst (ms)"),
		rttAvg:    newGauge("twamp_rtt_avg_milliseconds", "Average RTT in burst (ms)"),
		rttMax:    newGauge("twamp_rtt_max_milliseconds", "Maximum RTT in burst (ms)"),
		rttStddev: newGauge("twamp_rtt_stddev_milliseconds", "RTT standard deviation (ms)"),
		jitter:    newGauge("twamp_jitter_milliseconds", "Mean absolute jitter (ms)"),
		lossRatio: newGauge("twamp_loss_ratio", "Packet loss ratio 0.0-1.0"),
		pktSent:   newCounter("twamp_packets_sent_total", "Cumulative packets sent", probeLabels),
		pktRecv:   newCounter("twamp_packets_received_total", "Cumulative packets received", probeLabels),
		reflected: newCounter("twamp_reflected_packets_total", "Packets reflected since startup", []string{"source"}),
		registry:  reg,
		hostname:  hostname,
	}
	// Pre-initialize so the counter appears at zero before any packets arrive.
	s.reflected.WithLabelValues(hostname)
	return s
}

// Update sets all probe gauges and adds to cumulative counters for one ProbeResult.
func (s *PrometheusStore) Update(r ProbeResult) {
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	labels := prometheus.Labels{
		"source":   r.Source,
		"target":   r.Target,
		"topology": r.Topology,
		"site":     r.Site,
	}
	s.rttMin.With(labels).Set(ms(r.RttMin))
	s.rttAvg.With(labels).Set(ms(r.RttAvg))
	s.rttMax.With(labels).Set(ms(r.RttMax))
	s.rttStddev.With(labels).Set(ms(r.RttStddev))
	s.jitter.With(labels).Set(ms(r.Jitter))
	s.lossRatio.With(labels).Set(r.LossPct / 100.0)
	s.pktSent.With(labels).Add(float64(r.Sent))
	s.pktRecv.With(labels).Add(float64(r.Recv))
}

// IncrementReflected adds 1 to the reflected-packets counter for this host.
func (s *PrometheusStore) IncrementReflected() {
	s.reflected.WithLabelValues(s.hostname).Inc()
}

// Handler returns an http.Handler that serves the Prometheus text exposition format.
func (s *PrometheusStore) Handler() http.Handler {
	return promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})
}
