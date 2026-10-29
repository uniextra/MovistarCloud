package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"movistar-cloud-uploader/internal/config"
	"movistar-cloud-uploader/internal/logger"
	"movistar-cloud-uploader/internal/sapi"
	"movistar-cloud-uploader/internal/uploader"
	"movistar-cloud-uploader/internal/webui"
)

const AppVersion = "v2.0.0"

func main() {
	logger.Log("=====================================================")
	logger.Log("🚀 Movistar Cloud - Galería Uploader %s (Nativo en Go)", AppVersion)
	logger.Log("=====================================================")

	// Initialize SAPI client with empty or default configuration
	client := sapi.NewClient(config.AccountConfig{})
	jobManager := uploader.NewJobManager(client)
	uiServer := webui.NewServer(client, jobManager)

	webPort := "5000"
	if p := os.Getenv("PORT"); p != "" {
		webPort = p
	} else if p := os.Getenv("WEB_PORT"); p != "" {
		webPort = p
	}

	uiHTTP := &http.Server{
		Addr:    ":" + webPort,
		Handler: uiServer.Routes(),
	}

	go func() {
		logger.Log("[WebUI] ✔ Panel de Control escuchando en :%s", webPort)
		if err := uiHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Log("[FATAL] Error en servidor Web UI: %v", err)
		}
	}()

	// Graceful shutdown handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan

	logger.Log("[System] Recibida señal %v. Apagando servicio de forma ordenada...", sig)
	jobManager.StopJob()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := uiHTTP.Shutdown(shutdownCtx); err != nil {
		logger.Log("[WebUI] Error al apagar servidor: %v", err)
	}

	logger.Log("[System] ✔ Servidor apagado limpiamente.")
}
