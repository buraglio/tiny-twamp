package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// validateTLSFlags returns an error if exactly one of cert/key is non-empty.
func validateTLSFlags(cert, key string) error {
	if (cert == "") != (key == "") {
		return fmt.Errorf("-metrics-tls-cert and -metrics-tls-key must both be set or both be empty")
	}
	return nil
}

// runExporter starts the tinytwamp Prometheus exporter mode.
// probeMode is one of "background", "scrape", or "dual".
func runExporter(
	port int,
	configURL, hostname string,
	configRefresh time.Duration,
	probeMode, metricsAddr, metricsTLSCert, metricsTLSKey string,
	synced bool,
	logFile *os.File,
) {
	out := io.Writer(os.Stdout)
	if logFile != nil {
		out = logFile
	}
	logger := log.New(out, "[TWAMP-Light-Exporter] ", log.LstdFlags|log.Lmicroseconds)

	// Validate TLS flags
	if err := validateTLSFlags(metricsTLSCert, metricsTLSKey); err != nil {
		logger.Fatalf("%v", err)
	}

	// Pre-validate TLS key pair is readable before starting any goroutines
	if metricsTLSCert != "" {
		if _, err := tls.LoadX509KeyPair(metricsTLSCert, metricsTLSKey); err != nil {
			logger.Fatalf("Cannot load TLS cert/key: %v", err)
		}
	}

	// Fetch initial config — fatal on failure
	logger.Printf("Fetching config from %s", configURL)
	initialCfg, err := fetchConfig(configURL)
	if err != nil {
		logger.Fatalf("Cannot fetch initial config: %v", err)
	}
	logger.Printf("Config loaded: topology=%s hosts=%d probe_interval=%v probe_mode=%s",
		initialCfg.Topology, len(initialCfg.Hosts), initialCfg.ProbeInterval, probeMode)

	// Validate dual mode requirements
	if probeMode == "dual" && initialCfg.InfluxDB.URL == "" {
		logger.Fatalf("probe-mode=dual requires influxdb.url in config")
	}

	// Resolve hostname
	if hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			logger.Fatalf("Cannot determine hostname: %v", err)
		}
		hostname = h
	}

	if configRefresh == 0 {
		configRefresh = initialCfg.ConfigRefresh
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newPrometheusStore(hostname)

	var wg sync.WaitGroup

	// Goroutine: TWAMP-Light reflector — increments reflected counter
	wg.Add(1)
	go func() {
		defer wg.Done()
		al, _ := parseAllowlist("")
		rl := newRateLimiter(0)
		srv := NewServer(logFile, rl, al, synced)
		srv.onReflect = store.IncrementReflected
		if err := srv.Start(port); err != nil {
			logger.Printf("Server error: %v", err)
		}
	}()

	switch probeMode {
	case "background":
		configCh := make(chan AgentConfig, 1)
		resultsCh := make(chan ProbeResult, 200)
		configCh <- initialCfg

		wg.Add(1)
		go func() {
			defer wg.Done()
			runConfigPoller(ctx, configURL, configRefresh, configCh, logger)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			runProbeScheduler(ctx, configCh, resultsCh, hostname, port, synced, logFile, logger)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case r, ok := <-resultsCh:
					if !ok {
						return
					}
					store.Update(r)
				case <-ctx.Done():
					return
				}
			}
		}()

	case "scrape":
		// Config poller keeps currentCfg fresh; scrapes probe inline.
		var cfgMu sync.RWMutex
		currentCfg := initialCfg

		configCh := make(chan AgentConfig, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			runConfigPoller(ctx, configURL, configRefresh, configCh, logger)
		}()

		// Config updater goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case cfg := <-configCh:
					cfgMu.Lock()
					currentCfg = cfg
					cfgMu.Unlock()
				case <-ctx.Done():
					return
				}
			}
		}()

		// Override the HTTP handler to probe inline on each scrape
		origHandler := store.Handler()
		var scrapeMu sync.Mutex
		scrapeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scrapeMu.Lock()
			defer scrapeMu.Unlock()
			cfgMu.RLock()
			cfg := currentCfg
			cfgMu.RUnlock()
			targets := cfg.targetsFor(hostname)
			for _, target := range targets {
				result := runBurst(target, cfg, hostname, port, synced, logFile)
				store.Update(result)
			}
			origHandler.ServeHTTP(w, r)
		})
		store.scrapeHandler = scrapeHandler

	case "dual":
		configCh := make(chan AgentConfig, 1)
		promCh := make(chan ProbeResult, 200)
		influxCh := make(chan ProbeResult, 200)
		schedulerCh := make(chan ProbeResult, 200)
		configCh <- initialCfg

		wg.Add(1)
		go func() {
			defer wg.Done()
			runConfigPoller(ctx, configURL, configRefresh, configCh, logger)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			runProbeScheduler(ctx, configCh, schedulerCh, hostname, port, synced, logFile, logger)
		}()

		// Dispatcher: fan-out scheduler results to both Prom and Influx channels
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(promCh)
			defer close(influxCh)
			for {
				select {
				case r, ok := <-schedulerCh:
					if !ok {
						return
					}
					select {
					case promCh <- r:
					default:
					}
					select {
					case influxCh <- r:
					default:
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case r, ok := <-promCh:
					if !ok {
						return
					}
					store.Update(r)
				case <-ctx.Done():
					return
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newInfluxWriter(initialCfg.InfluxDB, logger)
			w.run(ctx, influxCh)
		}()

	default:
		logger.Fatalf("unknown -probe-mode %q; must be background, scrape, or dual", probeMode)
	}

	// Start metrics HTTP server
	mux := http.NewServeMux()
	if store.scrapeHandler != nil {
		mux.Handle("/metrics", store.scrapeHandler)
	} else {
		mux.Handle("/metrics", store.Handler())
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><a href="/metrics">Metrics</a></body></html>`)
	})

	srv := &http.Server{
		Addr:              metricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		if metricsTLSCert != "" {
			logger.Printf("Metrics HTTPS server listening on %s", metricsAddr)
			err = srv.ListenAndServeTLS(metricsTLSCert, metricsTLSKey)
		} else {
			logger.Printf("Metrics HTTP server listening on %s", metricsAddr)
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Printf("Metrics server error: %v", err)
		}
	}()

	platformWaitForShutdown(cancel, logger)
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx) //nolint:errcheck
	wg.Wait()
}
