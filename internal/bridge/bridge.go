package bridge

import (
	"context"
	"fmt"
	"log"
	"sip-webrtc-openai/internal/controllers"
	"sip-webrtc-openai/internal/openai"
	"sip-webrtc-openai/internal/store"
	"sync"
	"sync/atomic"
	"time"
)

// OpusPacketMetadata содержит метаданные о пакете Opus
type OpusPacketMetadata struct {
	SessionID   string
	Timestamp   time.Time
	Direction   string // "incoming" или "outgoing"
	PacketSize  int
	SequenceNum int64
}

// OpusPacketHandler - тип функции для обработки Opus пакетов
type OpusPacketHandler func(session *BridgeSession, opusData []byte, metadata OpusPacketMetadata)

// SessionEndHandler - тип функции для обработки окончания сессии
type SessionEndHandler func(session *BridgeSession, reason string)

type AudioEndpoint interface {
	GetID() string
	GetIdentifier() string      // НОВОЕ: получение номера телефона/идентификатора
	SendOpusFrame([]byte) error // Метод для прямой передачи Opus
	Close()
}

type BridgeSession struct {
	id         string
	identifier string // НОВОЕ: номер телефона/идентификатор
	endpoint   AudioEndpoint
	openAI     *openai.OpenAIService
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.RWMutex

	// Обработка аудио
	opusBuffer chan []byte // Буфер для Opus frames

	// Состояние
	isActive          bool
	isResponsePlaying int32

	// Критическое отслеживание из референсного кода
	audioSampleCount  int64 // атомарно - сэмплы отправленные для проигрывания
	playedSampleCount int64 // атомарно - сэмплы фактически проигранные
	currentItemID     string
	currentItemMutex  sync.RWMutex

	// Счетчики для отладки
	inputPacketCount  int64
	outputPacketCount int64
	opusFrameCount    int64 // Счетчик Opus frames

	// Счетчики для sequence numbers
	incomingSeqNum int64
	outgoingSeqNum int64
}

// GetID возвращает ID сессии
func (s *BridgeSession) GetID() string {
	return s.id
}

// GetIdentifier возвращает номер телефона/идентификатор сессии
func (s *BridgeSession) GetIdentifier() string {
	return s.identifier
}

type Bridge struct {
	sessions map[string]*BridgeSession
	mu       sync.RWMutex
	apiKey   string

	// Новые хендлеры для Opus пакетов
	onIncomingOpusPacket OpusPacketHandler
	onOutgoingOpusPacket OpusPacketHandler
	onSessionEnd         SessionEndHandler
	handlersMu           sync.RWMutex
	contextStore         *store.ContextStore // НОВОЕ

}

func NewBridge(apiKey string) *Bridge {
	return &Bridge{
		sessions: make(map[string]*BridgeSession),
		apiKey:   apiKey,
	}
}

// SetIncomingOpusPacketHandler устанавливает хендлер для входящих Opus пакетов
func (b *Bridge) SetIncomingOpusPacketHandler(handler OpusPacketHandler) {
	b.handlersMu.Lock()
	defer b.handlersMu.Unlock()
	b.onIncomingOpusPacket = handler
}

// SetOutgoingOpusPacketHandler устанавливает хендлер для исходящих Opus пакетов
func (b *Bridge) SetOutgoingOpusPacketHandler(handler OpusPacketHandler) {
	b.handlersMu.Lock()
	defer b.handlersMu.Unlock()
	b.onOutgoingOpusPacket = handler
}

// SetSessionEndHandler устанавливает хендлер для окончания сессии
func (b *Bridge) SetSessionEndHandler(handler SessionEndHandler) {
	b.handlersMu.Lock()
	defer b.handlersMu.Unlock()
	b.onSessionEnd = handler
}

// callIncomingOpusHandler вызывает хендлер входящих пакетов
func (b *Bridge) callIncomingOpusHandler(session *BridgeSession, opusData []byte) {
	b.handlersMu.RLock()
	handler := b.onIncomingOpusPacket
	b.handlersMu.RUnlock()

	if handler != nil {
		seqNum := atomic.AddInt64(&session.incomingSeqNum, 1)
		metadata := OpusPacketMetadata{
			SessionID:   session.id,
			Timestamp:   time.Now(),
			Direction:   "incoming",
			PacketSize:  len(opusData),
			SequenceNum: seqNum,
		}

		// Вызываем хендлер в отдельной горутине чтобы не блокировать основной поток
		go handler(session, opusData, metadata)
	}
}

// callOutgoingOpusHandler вызывает хендлер исходящих пакетов
func (b *Bridge) callOutgoingOpusHandler(session *BridgeSession, opusData []byte) {
	b.handlersMu.RLock()
	handler := b.onOutgoingOpusPacket
	b.handlersMu.RUnlock()

	if handler != nil {
		seqNum := atomic.AddInt64(&session.outgoingSeqNum, 1)
		metadata := OpusPacketMetadata{
			SessionID:   session.id,
			Timestamp:   time.Now(),
			Direction:   "outgoing",
			PacketSize:  len(opusData),
			SequenceNum: seqNum,
		}

		// Вызываем хендлер в отдельной горутине чтобы не блокировать основной поток
		go handler(session, opusData, metadata)
	}
}

func (b *Bridge) SetContextStore(store *store.ContextStore) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contextStore = store
}

// callSessionEndHandler вызывает хендлер окончания сессии
func (b *Bridge) callSessionEndHandler(session *BridgeSession, reason string) {
	b.handlersMu.RLock()
	handler := b.onSessionEnd
	b.handlersMu.RUnlock()

	if handler != nil {
		// Вызываем хендлер в отдельной горутине
		go handler(session, reason)
	}
}

// CreateSession создает новую bridge сессию с идентификатором
func (b *Bridge) CreateSession(endpoint AudioEndpoint, identifier string, from string) (*BridgeSession, error) {
	if b.apiKey == "" {
		return nil, fmt.Errorf("OpenAI API key is empty")
	}

	b.contextStore.SaveUserSession(identifier, endpoint.GetID(), from, time.Now().UnixNano())

	transcriptController := controllers.NewTranscriptController(b.contextStore)

	ctx, cancel := context.WithCancel(context.Background())

	session := &BridgeSession{
		id:         endpoint.GetID(),
		identifier: identifier, // НОВОЕ: сохраняем идентификатор
		endpoint:   endpoint,
		ctx:        ctx,
		cancel:     cancel,
		opusBuffer: make(chan []byte, 2000), // Буфер для Opus
		isActive:   true,
	}

	customContext := ""
	if b.contextStore != nil && identifier != "" {
		if context, err := b.contextStore.GetContext(identifier); err == nil {
			customContext = context
			log.Printf("Using custom context for %s: %s", identifier, customContext[:min(50, len(customContext))])
		} else {
			log.Printf("No custom context found for %s: %v", identifier, err)
		}
	}
	// Создаем OpenAI сервис
	session.openAI = openai.NewOpenAIService(b.apiKey, customContext)

	// Обработчик входящих Opus frames от OpenAI
	session.openAI.OnOpusData = func(opusFrame []byte) {
		if !session.isActive {
			return
		}

		if session.opusBuffer == nil {
			return
		}

		atomic.StoreInt32(&session.isResponsePlaying, 1)

		// Отслеживаем Opus frames
		atomic.AddInt64(&session.opusFrameCount, 1)

		select {
		case session.opusBuffer <- opusFrame:
			// Frame queued successfully
		case <-time.After(10 * time.Millisecond):
			log.Printf("Audio buffer blocked for session %s", session.id)
		}
	}

	// Обработчик ошибок соединения с OpenAI
	session.openAI.OnError = func(err error) {
		if session.isActive {
			log.Printf("OpenAI connection error for session %s: %v", session.id, err)
			b.callSessionEndHandler(session, fmt.Sprintf("openai_error: %v", err))
		}
	}

	// Обработчик закрытия соединения с OpenAI
	session.openAI.OnClose = func() {
		if session.isActive {
			log.Printf("OpenAI connection closed for session %s", session.id)
			b.callSessionEndHandler(session, "openai_connection_closed")
		}
	}

	session.openAI.OnTextData = func(msg openai.Message, raw []byte) {
		var id = session.id
		log.Printf("--- Received text data for session %s: %s", id, raw)
		transcriptController.AddMessage(id, raw)
	}

	session.openAI.OnSpeechStart = func() {
		// Прерываем текущий ответ если он проигрывается
		if atomic.LoadInt32(&session.isResponsePlaying) == 1 {
			session.currentItemMutex.RLock()
			currentItemID := session.currentItemID
			session.currentItemMutex.RUnlock()

			playedSamples := atomic.LoadInt64(&session.playedSampleCount)

			if err := session.openAI.InterruptResponse(currentItemID, playedSamples); err != nil {
				log.Printf("Error interrupting response: %v", err)
			}

			// Очищаем буферы с небольшой задержкой
			time.AfterFunc(100*time.Millisecond, func() {
				b.clearBuffers(session)
			})
		}
	}

	session.openAI.OnResponseDone = func() {
		atomic.StoreInt32(&session.isResponsePlaying, 0)
		session.currentItemMutex.Lock()
		session.currentItemID = ""
		session.currentItemMutex.Unlock()
	}

	session.openAI.OnResponseCreated = func(itemID string) {
		session.currentItemMutex.Lock()
		session.currentItemID = itemID
		session.currentItemMutex.Unlock()
	}

	// Подключаемся к OpenAI
	if err := session.openAI.Connect(); err != nil {
		log.Printf("Failed to connect to OpenAI for session %s: %v", session.id, err)
		cancel()
		return nil, fmt.Errorf("failed to connect to OpenAI: %w", err)
	}

	// Сохраняем сессию
	b.mu.Lock()
	b.sessions[session.id] = session
	b.mu.Unlock()

	// Запускаем обработку исходящих Opus frames
	go b.processOpusOutput(session)

	log.Printf("Bridge session created for %s with identifier: %s", session.id, identifier)
	return session, nil
}

// ProcessIncomingOpus - прямая обработка входящих Opus frames
func (b *Bridge) ProcessIncomingOpus(session *BridgeSession, opusData []byte) {
	if !session.isActive {
		return
	}

	// Вызываем хендлер входящих пакетов
	b.callIncomingOpusHandler(session, opusData)

	// Прямая отправка в OpenAI без буферизации
	if err := session.openAI.SendOpusAudio(opusData); err != nil {
		log.Printf("Failed to send Opus to OpenAI for session %s: %v", session.id, err)
	} else {
		atomic.AddInt64(&session.inputPacketCount, 1)
	}
}

// processOpusOutput - прямая передача Opus frames без декодирования/обработки
func (b *Bridge) processOpusOutput(session *BridgeSession) {
	for {
		select {
		case <-session.ctx.Done():
			return
		case opusFrame, ok := <-session.opusBuffer:
			if !ok {
				return
			}

			// Проверяем остановлено ли воспроизведение
			isPlaying := atomic.LoadInt32(&session.isResponsePlaying)
			if isPlaying == 0 {
				continue
			}

			if session.endpoint == nil {
				log.Printf("Critical error: session.endpoint is nil for %s", session.id)
				continue
			}

			// Вызываем хендлер исходящих пакетов
			b.callOutgoingOpusHandler(session, opusFrame)

			// Обновляем счетчик воспроизведенных сэмплов (приблизительно)
			// Opus frame 20ms на 24kHz = 480 сэмплов
			atomic.AddInt64(&session.playedSampleCount, 480)

			// Прямая отправка Opus frame через SendOpusFrame
			err := session.endpoint.SendOpusFrame(opusFrame)
			if err != nil {
				log.Printf("Error sending Opus frame to endpoint %s: %v", session.id, err)
			} else {
				atomic.AddInt64(&session.outputPacketCount, 1)
			}
		}
	}
}

func (b *Bridge) clearBuffers(session *BridgeSession) {
	// Очищаем Opus буфер
	cleared := 0
	for {
		select {
		case <-session.opusBuffer:
			cleared++
		default:
			return
		}
	}
}

func (b *Bridge) RemoveSession(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if session, exists := b.sessions[sessionID]; exists {
		session.isActive = false

		// Уведомляем о завершении сессии с причиной "manual_removal"
		b.callSessionEndHandler(session, "manual_removal")

		session.cancel()
		if session.openAI != nil {
			session.openAI.Close()
		}
		delete(b.sessions, sessionID)
		log.Printf("Session %s removed", sessionID)
	}
}

// RemoveSessionWithReason удаляет сессию с указанием конкретной причины
func (b *Bridge) RemoveSessionWithReason(sessionID string, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if session, exists := b.sessions[sessionID]; exists {
		session.isActive = false

		// Уведомляем о завершении сессии с указанной причиной
		b.callSessionEndHandler(session, reason)

		session.cancel()
		if session.openAI != nil {
			session.openAI.Close()
		}
		delete(b.sessions, sessionID)
		log.Printf("Session %s removed with reason: %s", sessionID, reason)
	}
}

func (b *Bridge) Shutdown() {
	b.mu.RLock()
	sessions := make([]*BridgeSession, 0, len(b.sessions))
	for _, s := range b.sessions {
		sessions = append(sessions, s)
	}
	b.mu.RUnlock()

	for _, session := range sessions {
		// Уведомляем о завершении сессии с причиной "bridge_shutdown"
		b.callSessionEndHandler(session, "bridge_shutdown")
		b.RemoveSession(session.id)
	}
}

// Дополнительные методы для отладки
func (b *Bridge) GetSessionStats(sessionID string) (inputPackets, outputPackets, audioSamples, playedSamples, opusFrames int64) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if session, exists := b.sessions[sessionID]; exists {
		return atomic.LoadInt64(&session.inputPacketCount),
			atomic.LoadInt64(&session.outputPacketCount),
			atomic.LoadInt64(&session.audioSampleCount),
			atomic.LoadInt64(&session.playedSampleCount),
			atomic.LoadInt64(&session.opusFrameCount)
	}
	return 0, 0, 0, 0, 0
}

// GetSession возвращает сессию по ID (полезно для хендлеров)
func (b *Bridge) GetSession(sessionID string) (*BridgeSession, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	session, exists := b.sessions[sessionID]
	return session, exists
}
