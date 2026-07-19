// Market-AI-Factory server — product registry, onboarding wizard, and monitor.
// Follows the Market-AI backend conventions: zap logging, net/http mux,
// embedded-schema SQLite, graceful shutdown.
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/alpaca"
	"github.com/wrcron2/market-ai-factory/backend/internal/db"
	"github.com/wrcron2/market-ai-factory/backend/internal/llm"
	"github.com/wrcron2/market-ai-factory/backend/internal/monitor"
	"github.com/wrcron2/market-ai-factory/backend/internal/pipeline"
	"github.com/wrcron2/market-ai-factory/backend/internal/registry"
	"github.com/wrcron2/market-ai-factory/backend/internal/wizard"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	dsn := getEnv("FACTORY_DB_DSN", "./factory.db")
	database, err := db.Open(dsn)
	if err != nil {
		logger.Fatal("db open failed", zap.Error(err))
	}
	defer database.Close()
	logger.Info("database ready", zap.String("dsn", dsn))

	repoRoot := getEnv("FACTORY_REPO_ROOT", ".")
	workRoot := getEnv("FACTORY_WORK_ROOT", "./product-workdirs")
	alpacaClient := alpaca.New()
	reg := registry.New(database, logger,
		registry.NewMetricsProvider(repoRoot, alpacaClient, logger), workRoot)
	engine := wizard.NewEngine(database, logger, repoRoot, workRoot,
		wizard.DefaultSteps(alpacaClient))
	wiz := wizard.NewHandler(engine, database, logger)

	// Each product's ops team: 2h deterministic checks + daily AI review.
	mon := monitor.New(database, logger, alpacaClient, llm.New(), repoRoot)
	mon.Start()

	mux := http.NewServeMux()
	// Health first — the Market-AI lesson: stats-style endpoints can 500 on an
	// empty DB, so the Factory ships a dedicated, dependency-free health probe.
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"market-ai-factory"}`))
	})
	mux.HandleFunc("/api/products", reg.Products)
	mux.HandleFunc("/api/products/", reg.Product)
	mux.HandleFunc("/api/killall", reg.KillAll)
	mux.HandleFunc("/api/wizard/steps", wiz.Steps)
	mux.HandleFunc("/api/wizard/runs", wiz.Runs)
	mux.HandleFunc("/api/wizard/runs/", wiz.RunByID)

	pipelineHandler := pipeline.New(repoRoot, database, logger)
	mux.HandleFunc("/api/pipeline/repos", pipelineHandler.Repos)
	mux.HandleFunc("/api/pipeline/status", pipelineHandler.Status)
	mux.HandleFunc("/api/pipeline/run/scout", pipelineHandler.RunScout)
	mux.HandleFunc("/api/pipeline/run/research", pipelineHandler.RunResearch)
	mux.HandleFunc("/api/pipeline/logs", pipelineHandler.Logs)
	mux.HandleFunc("/api/pipeline/logs/clear", pipelineHandler.ClearLogs)
	mux.HandleFunc("/api/pipeline/report/{id}", pipelineHandler.Report)

	port := getEnv("FACTORY_PORT", "9080")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("factory server listening", zap.String("port", port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server failed", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	logger.Info("server stopped")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
