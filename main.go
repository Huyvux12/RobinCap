// Command capcut-tts-go serves the CapCut TTS/STT port over HTTP with a dashboard.
package main

import (
	"context"
	"embed"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Huyvux12/RobinCap/capcut"
)

//go:embed web/index.html web/docs.html openapi.json
var assets embed.FS

var version = "dev"

type Config struct {
	Port             string
	APIKey           string
	RateRPM          int
	RateBurst        int
	BatchConcurrency int
	BatchMax         int
	CacheSize        int
	CacheTTL         time.Duration
	MaxUploadMB      int
	MaxTextChars     int
	TTSTimeout       time.Duration
	STTTimeout       time.Duration
	CORSOrigin       string
	DefaultSTTLang   string
}

func envStr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}

func loadConfig() Config {
	return Config{
		Port:             envStr("PORT", "8080"),
		APIKey:           os.Getenv("API_KEY"),
		RateRPM:          envInt("RATE_LIMIT_RPM", 120),
		RateBurst:        envInt("RATE_LIMIT_BURST", 30),
		BatchConcurrency: envInt("BATCH_CONCURRENCY", 8),
		BatchMax:         envInt("BATCH_MAX", 200),
		CacheSize:        envInt("CACHE_SIZE", 2000),
		CacheTTL:         envDur("CACHE_TTL", 6*time.Hour),
		MaxUploadMB:      envInt("MAX_UPLOAD_MB", 25),
		MaxTextChars:     envInt("MAX_TEXT_CHARS", 3000),
		TTSTimeout:       envDur("TTS_TIMEOUT", 60*time.Second),
		STTTimeout:       envDur("STT_TIMEOUT", 180*time.Second),
		CORSOrigin:       envStr("CORS_ORIGINS", "*"),
		DefaultSTTLang:   envStr("STT_DEFAULT_LANGUAGE", "vi-VN"),
	}
}

func main() {
	cfg := loadConfig()
	dev, src, err := capcut.DeviceFromEnv()
	if err != nil {
		log.Fatalf("device config (%s): %v", src, err)
	}
	cc := capcut.NewClient(dev)
	cc.HedgeAfter = envDur("TTS_HEDGE_AFTER", 4500*time.Millisecond)
	cc.MaxHedges = envInt("TTS_MAX_HEDGES", 2)
	s := newServer(cfg, cc, src)
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      6 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("capcut-tts-go %s listening on :%s (device=%s, api_key=%v, rate=%d/min)", version, cfg.Port, src, cfg.APIKey != "", cfg.RateRPM)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
