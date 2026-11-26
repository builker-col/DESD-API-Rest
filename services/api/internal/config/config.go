package config

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type ConverterConfig struct {
	DPI           int
	MaxDim        int
	JPEGQuality   int
	FastPDFImages bool
	PDFChunkSize  int
}

type Config struct {
	ListenAddr       string
	PostgresURI      string
	RabbitMQHost     string
	RabbitMQUser     string
	RabbitMQPass     string
	JWTSecretKey     string
	WorkerQueue      string
	DuplicateQueue   string
	DLXExchange      string
	SharedDataDir    string
	IngestWorkers    int
	Converter        ConverterConfig
	HotfolderEnabled bool
	HotfolderRoot    string
	HotfolderPoll    time.Duration
	HotfolderUser    string
}

func Load() Config {
	pgURI := strings.TrimSpace(os.Getenv("POSTGRES_URI"))
	if pgURI == "" {
		pgURI = getEnv("SQLALCHEMY_DATABASE_URI", "postgresql://user:password@postgres:5432/desd")
	}

	defaultWorkers := runtime.NumCPU()
	if defaultWorkers > 6 {
		defaultWorkers = 6
	}
	if defaultWorkers < 2 {
		defaultWorkers = 2
	}

	pollSec := parseFloatEnv("HOTFOLDER_POLL_INTERVAL", 0.5)
	if pollSec < 0.1 {
		pollSec = 0.1
	}

	return Config{
		ListenAddr:     getEnv("API_LISTEN_ADDR", ":5000"),
		PostgresURI:    pgURI,
		RabbitMQHost:   getEnv("RABBITMQ_HOST", "rabbitmq"),
		RabbitMQUser:   getEnv("RABBITMQ_USER", "user"),
		RabbitMQPass:   getEnv("RABBITMQ_PASS", "password"),
		JWTSecretKey:   getEnv("JWT_SECRET_KEY", "jwt_secret_key"),
		WorkerQueue:    getEnv("WORKER_QUEUE_NAME", "queue_worker"),
		DuplicateQueue: getEnv("DUPLICATE_QUEUE_NAME", "queue_dude"),
		DLXExchange:    getEnv("RABBITMQ_DLX", "desd.dlx"),
		SharedDataDir:  getEnv("SHARED_DATA_DIR", "/shared-data"),
		IngestWorkers:  parseIntEnv("INGEST_CONCURRENCY", defaultWorkers),
		Converter: ConverterConfig{
			DPI:           parseIntEnv("CONVERTER_DPI", 150),
			MaxDim:        parseIntEnv("CONVERTER_MAX_DIM", 960),
			JPEGQuality:   parseIntEnv("CONVERTER_JPEG_QUALITY", 85),
			FastPDFImages: parseBoolEnv("CONVERTER_FAST_PDFIMAGES", true),
			PDFChunkSize:  parseIntEnv("CONVERTER_PDF_CHUNK_SIZE", 8),
		},
		HotfolderEnabled: parseBoolEnv("HOTFOLDER_ENABLED", true),
		HotfolderRoot:    getEnv("HOTFOLDER_ROOT", "/hotfolder"),
		HotfolderPoll:    time.Duration(pollSec * float64(time.Second)),
		HotfolderUser:    getEnv("HOTFOLDER_DEFAULT_USER", "admin"),
	}
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseIntEnv(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func parseFloatEnv(key string, fallback float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func parseBoolEnv(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes" || v == "si"
}
