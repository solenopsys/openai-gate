package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"sip-webrtc-openai/internal/api"
	"sip-webrtc-openai/internal/bridge"
	"sip-webrtc-openai/internal/controllers"
	"sip-webrtc-openai/internal/sip"
	"sip-webrtc-openai/internal/store"
	"sip-webrtc-openai/internal/webrtc"

	"github.com/joho/godotenv"
)

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	// Environment variables for ports
	sipHost := getEnv("SIP_HOST", "0.0.0.0")
	sipPublicHost := getEnv("SIP_PUBLIC_HOST", "0.0.0.0")
	sipPort := getEnvAsInt("SIP_PORT", 5060)
	httpPort := getEnvAsInt("HTTP_PORT", 8080)
	contextApiPort := getEnvAsInt("CONTEXT_API_PORT", 8081)
	enableSIP := getEnvAsBool("ENABLE_SIP", true)
	enableWeb := getEnvAsBool("ENABLE_WEB", true)
	enableContextAPI := getEnvAsBool("ENABLE_CONTEXT_API", true)

	// Get OpenAI API key
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is required")
	}

	log.Printf("Starting SIP-WebRTC-OpenAI Bridge")
	log.Printf("SIP enabled: %v, WebRTC enabled: %v, Context API enabled: %v", enableSIP, enableWeb, enableContextAPI)

	// Инициализируем context store
	contextStore, err := store.NewContextStore("contexts.db")
	if err != nil {
		log.Fatalf("Failed to initialize context store: %v", err)
	}
	defer contextStore.Close()
	log.Printf("✅ Context store initialized")

	recorder, err := controllers.NewOpusRecorderController(contextStore)
	if err != nil {
		log.Fatalf("Failed to initialize recorder: %v", err)
	}

	// Инициализируем context API
	var contextAPI *api.ContextAPI
	if enableContextAPI {
		contextAPI = api.NewContextAPI(contextStore, recorder)
		log.Printf("✅ Context API initialized")
	}

	// Настраиваем опции записи
	recorder.SetRecordingOptions(true, true) // записывать входящие и исходящие

	// Получаем хендлеры от контроллера записи
	incomingHandler := recorder.GetIncomingPacketHandler()
	outgoingHandler := recorder.GetOutgoingPacketHandler()
	sessionEndHandler := recorder.GetSessionEndHandler()

	// Create bridge с context store
	bridgeInstance := bridge.NewBridge(apiKey)
	bridgeInstance.SetContextStore(contextStore) // Передаем store в bridge

	// Основной код с записью на диск
	br := bridgeInstance
	br.SetIncomingOpusPacketHandler(func(session *bridge.BridgeSession, opusData []byte, metadata bridge.OpusPacketMetadata) {
		// Преобразуем метаданные для совместимости
		recorderMetadata := controllers.OpusPacketMetadata{
			SessionID:   metadata.SessionID,
			Timestamp:   metadata.Timestamp,
			Source:      metadata.Direction,
			PacketSize:  metadata.PacketSize,
			SequenceNum: metadata.SequenceNum,
		}

		// Вызываем хендлер контроллера записи
		incomingHandler(session, opusData, recorderMetadata)

		// Дополнительная логика (если нужна)
		log.Printf("Recording aiu packet: Session=%s, Size=%d bytes",
			metadata.SessionID, metadata.PacketSize)
	})

	br.SetOutgoingOpusPacketHandler(func(session *bridge.BridgeSession, opusData []byte, metadata bridge.OpusPacketMetadata) {
		// Преобразуем метаданные для совместимости
		recorderMetadata := controllers.OpusPacketMetadata{
			SessionID:   metadata.SessionID,
			Timestamp:   metadata.Timestamp,
			Source:      metadata.Direction,
			PacketSize:  metadata.PacketSize,
			SequenceNum: metadata.SequenceNum,
		}

		// Вызываем хендлер контроллера записи
		outgoingHandler(session, opusData, recorderMetadata)

		// Дополнительная логика (если нужна)
		log.Printf("Recording user packet: Session=%s, Size=%d bytes",
			metadata.SessionID, metadata.PacketSize)
	})

	br.SetSessionEndHandler(func(session *bridge.BridgeSession, reason string) {
		// Вызываем хендлер контроллера записи
		sessionEndHandler(session, reason)

		// Дополнительная логика завершения сессии
		log.Printf("Session %s ended with reason: %s", session.GetID(), reason)
	})

	// Channel to track server errors
	errChan := make(chan error, 3)

	// Start Context API if enabled
	if enableContextAPI && contextAPI != nil {
		go func() {
			log.Printf("Starting Context API on :%d", contextApiPort)
			if err := contextAPI.Start(fmt.Sprintf(":%d", contextApiPort)); err != nil {
				log.Printf("Context API error: %v", err)
				errChan <- fmt.Errorf("Context API failed: %w", err)
			}
		}()
	}

	// Start SIP server if enabled
	var sipServer *sip.SIPServer
	if enableSIP {
		sipServer = sip.NewSIPServer(sipHost, sipPublicHost, sipPort, bridgeInstance)

		go func() {
			log.Printf("Starting SIP server on %s(%s):%d", sipHost, sipPublicHost, sipPort)
			if err := sipServer.Start(); err != nil {
				log.Printf("SIP server error: %v", err)
				errChan <- fmt.Errorf("SIP server failed: %w", err)
			}
		}()
	}

	// Start WebRTC service if enabled
	var httpServer *http.Server
	if enableWeb {
		webrtcService := webrtc.NewWebRTCService(bridgeInstance)

		// Настраиваем маршруты
		mux := http.NewServeMux()
		mux.HandleFunc("/ws", webrtcService.HandleWebSocket)

		// Служим статические файлы (включая ваш index.html)
		mux.Handle("/", http.FileServer(http.Dir("./")))

		httpServer = &http.Server{
			Addr:         fmt.Sprintf(":%d", httpPort),
			Handler:      mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		}

		go func() {
			log.Printf("Starting WebRTC/HTTP server on :%d", httpPort)
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("WebRTC server error: %v", err)
				errChan <- fmt.Errorf("WebRTC server failed: %w", err)
			}
		}()
	}

	log.Printf("✅ All services started successfully")
	if enableWeb {
		log.Printf("🌐 WebRTC interface: http://localhost:%d", httpPort)
	}
	if enableSIP {
		log.Printf("📞 SIP endpoint: %s:%d", sipHost, sipPort)
	}
	if enableContextAPI {
		log.Printf("🔧 Context API: http://localhost:%d", contextApiPort)
	}

	// Wait for shutdown signal or server error
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigChan:
		log.Printf("Received signal: %v", sig)
	case err := <-errChan:
		log.Printf("Server error: %v", err)
	}

	log.Println("Shutting down...")

	// Graceful shutdown
	shutdownChan := make(chan struct{})
	go func() {
		defer close(shutdownChan)

		// Stop SIP server
		if sipServer != nil {
			log.Println("Stopping SIP server...")
			sipServer.Stop()
		}

		// Stop HTTP server
		if httpServer != nil {
			log.Println("Stopping HTTP server...")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(ctx); err != nil {
				log.Printf("HTTP server shutdown error: %v", err)
			}
		}

		// Shutdown bridge
		log.Println("Shutting down bridge...")
		bridgeInstance.Shutdown()

		// Close context store
		log.Println("Closing context store...")
		contextStore.Close()
	}()

	// Wait for shutdown to complete or timeout
	select {
	case <-shutdownChan:
		log.Println("Shutdown complete")
	case <-time.After(10 * time.Second):
		log.Println("Shutdown timeout, forcing exit")
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvAsInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvAsBool(key string, fallback bool) bool {
	if value, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
	}
	return fallback
}
