package controllers

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"sip-webrtc-openai/internal/store"

	"github.com/at-wat/ebml-go/webm"
)

// OpusPacket представляет Opus пакет с временной меткой
type OpusPacket struct {
	Data      []byte
	Timestamp time.Time
	Source    string
	SeqNum    int64
}

// SessionRecording содержит метаданные записи для одной сессии
type SessionRecording struct {
	SessionID string
	StartTime time.Time
	EndTime   time.Time
	mutex     sync.RWMutex

	// Только статистика, фреймы хранятся в store
	IncomingCount int64
	OutgoingCount int64
}

// OpusPacketMetadata содержит метаданные о пакете
type OpusPacketMetadata struct {
	SessionID   string
	Timestamp   time.Time
	Source      string
	PacketSize  int
	SequenceNum int64
}

// Session интерфейс для получения ID сессии
type Session interface {
	GetID() string
}

// BufferWriteCloser обертка для bytes.Buffer чтобы реализовать io.WriteCloser
type BufferWriteCloser struct {
	*bytes.Buffer
}

// Close реализует метод Close для io.WriteCloser
func (bwc *BufferWriteCloser) Close() error {
	return nil // bytes.Buffer не нуждается в закрытии
}

// OpusRecorderController контроллер для записи Opus пакетов
type OpusRecorderController struct {
	outputDir  string
	recordings map[string]*SessionRecording
	mutex      sync.RWMutex
	store      *store.ContextStore

	// Настройки записи
	recordIncoming bool
	recordOutgoing bool

	// FFmpeg настройки
	enableMerging bool
	ffmpegPath    string
}

// NewOpusRecorderController создает новый контроллер записи
func NewOpusRecorderController(contextStore *store.ContextStore) (*OpusRecorderController, error) {

	return &OpusRecorderController{
		recordings:     make(map[string]*SessionRecording),
		store:          contextStore,
		recordIncoming: true,
		recordOutgoing: true,
		enableMerging:  true,
		ffmpegPath:     "ffmpeg",
	}, nil
}

// SetFFmpegPath устанавливает путь к ffmpeg
func (c *OpusRecorderController) SetFFmpegPath(path string) {
	c.ffmpegPath = path
}

// SetMergingEnabled включает/отключает слияние файлов
func (c *OpusRecorderController) SetMergingEnabled(enabled bool) {
	c.enableMerging = enabled
}

// SetRecordingOptions настраивает что записывать
func (c *OpusRecorderController) SetRecordingOptions(userSource, aiSource bool) {
	c.recordIncoming = userSource
	c.recordOutgoing = aiSource
}

// GetIncomingPacketHandler возвращает хендлер для входящих пакетов
func (c *OpusRecorderController) GetIncomingPacketHandler() func(session Session, opusData []byte, metadata OpusPacketMetadata) {
	return func(session Session, opusData []byte, metadata OpusPacketMetadata) {
		if c.recordIncoming {
			c.addPacket(metadata.SessionID, opusData, UserSource, metadata.Timestamp, metadata.SequenceNum)
		}
	}
}

// GetOutgoingPacketHandler возвращает хендлер для исходящих пакетов
func (c *OpusRecorderController) GetOutgoingPacketHandler() func(session Session, opusData []byte, metadata OpusPacketMetadata) {
	return func(session Session, opusData []byte, metadata OpusPacketMetadata) {
		if c.recordOutgoing {
			c.addPacket(metadata.SessionID, opusData, AiSource, metadata.Timestamp, metadata.SequenceNum)
		}
	}
}

// GetSessionEndHandler возвращает хендлер для окончания сессии
func (c *OpusRecorderController) GetSessionEndHandler() func(session Session, reason string) {
	return func(session Session, reason string) {
		c.finalizeSession(session.GetID(), reason)
	}
}

// addPacket сохраняет пакет напрямую в хранилище
func (c *OpusRecorderController) addPacket(sessionID string, data []byte, direction string, timestamp time.Time, seqNum int64) {
	if len(data) == 0 {
		log.Printf("WARNING: Empty packet received for session %s, direction %s", sessionID, direction)
		return
	}

	log.Printf("DEBUG: Received %s packet for session %s, size: %d bytes", direction, sessionID, len(data))

	// Создаем запись сессии если ее нет
	c.mutex.Lock()
	recording, exists := c.recordings[sessionID]
	if !exists {
		recording = &SessionRecording{
			SessionID: sessionID,
			StartTime: time.Now(),
		}
		c.recordings[sessionID] = recording
		log.Printf("Started recording session %s", sessionID)
	}
	c.mutex.Unlock()

	// Сохраняем фрейм в хранилище используя timestamp в наносекундах как уникальный ключ
	timestampNanos := timestamp.UnixNano()
	key, err := c.store.SaveOpusFrame(sessionID, direction, data, timestampNanos)
	if err != nil {
		log.Printf("ERROR: Failed to save opus frame to store: %v", err)
		return
	}

	// Обновляем статистику
	recording.mutex.Lock()
	if direction == UserSource {
		recording.IncomingCount++
	} else {
		recording.OutgoingCount++
	}
	totalPackets := recording.IncomingCount + recording.OutgoingCount
	recording.mutex.Unlock()

	// Логируем прогресс каждые 10 пакетов
	if totalPackets%10 == 0 {
		log.Printf("Session %s: saved %d packets (IN: %d, OUT: %d), last key: %s",
			sessionID, totalPackets, recording.IncomingCount, recording.OutgoingCount, key)
	}
}

// finalizeSession завершает запись, извлекает фреймы из хранилища и создает WebM файлы
func (c *OpusRecorderController) finalizeSession(sessionID string, reason string) {
	c.mutex.Lock()
	recording, exists := c.recordings[sessionID]
	if !exists {
		c.mutex.Unlock()
		log.Printf("WARNING: Tried to finalize non-existent session %s", sessionID)
		return
	}
	delete(c.recordings, sessionID)
	c.mutex.Unlock()

	recording.mutex.Lock()
	recording.EndTime = time.Now()
	incomingCount := recording.IncomingCount
	outgoingCount := recording.OutgoingCount
	duration := recording.EndTime.Sub(recording.StartTime)
	recording.mutex.Unlock()

	log.Printf("Finalizing session %s (reason: %s): %d UserSource, %d user packets, duration: %v",
		sessionID, reason, incomingCount, outgoingCount, duration)

}

// loadPacketsFromStore загружает все пакеты для сессии из хранилища
func (c *OpusRecorderController) loadPacketsFromStore(sessionID string) ([]OpusPacket, error) {
	var allPackets []OpusPacket

	// Загружаем входящие пакеты
	incomingBatch, err := c.store.GetOpusFrames(sessionID, AiSource)
	if err != nil {
		log.Printf("WARNING: Failed to load AiSource frames for session %s: %v", sessionID, err)
	} else {
		log.Printf("Loaded %d AiSource frames for session %s", len(incomingBatch.Frames), sessionID)
		for timestampNanos, data := range incomingBatch.Frames {
			packet := OpusPacket{
				Data:      data,
				Timestamp: time.Unix(0, timestampNanos),
				Source:    AiSource,
				SeqNum:    timestampNanos, // Используем timestamp как sequence number
			}
			allPackets = append(allPackets, packet)
		}
	}

	// Загружаем исходящие пакеты
	outgoingBatch, err := c.store.GetOpusFrames(sessionID, UserSource)
	if err != nil {
		log.Printf("WARNING: Failed to load user frames for session %s: %v", sessionID, err)
	} else {
		log.Printf("Loaded %d user frames for session %s", len(outgoingBatch.Frames), sessionID)
		for timestampNanos, data := range outgoingBatch.Frames {
			packet := OpusPacket{
				Data:      data,
				Timestamp: time.Unix(0, timestampNanos),
				Source:    UserSource,
				SeqNum:    timestampNanos,
			}
			allPackets = append(allPackets, packet)
		}
	}

	log.Printf("Total loaded %d packets for session %s", len(allPackets), sessionID)
	return allPackets, nil
}

// fileExists проверяет существование файла и его размер
func (c *OpusRecorderController) fileExists(path string) bool {
	if stat, err := os.Stat(path); err == nil && stat.Size() > 0 {
		return true
	}
	return false
}

// GetActiveRecordings возвращает список активных записей
func (c *OpusRecorderController) GetActiveRecordings() []string {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	sessions := make([]string, 0, len(c.recordings))
	for sessionID := range c.recordings {
		sessions = append(sessions, sessionID)
	}
	return sessions
}

// GetRecordingStats возвращает статистику записи для сессии
func (c *OpusRecorderController) GetRecordingStats(sessionID string) (incomingCount, outgoingCount int64, duration time.Duration, exists bool) {
	c.mutex.RLock()
	recording, exists := c.recordings[sessionID]
	c.mutex.RUnlock()

	if !exists {
		return 0, 0, 0, false
	}

	recording.mutex.RLock()
	defer recording.mutex.RUnlock()

	incomingCount = recording.IncomingCount
	outgoingCount = recording.OutgoingCount
	duration = time.Since(recording.StartTime)

	return incomingCount, outgoingCount, duration, true
}

// ForceFinalize принудительно завершает запись для сессии
func (c *OpusRecorderController) ForceFinalize(sessionID string) {
	c.finalizeSession(sessionID, "force_finalized")
}

func (c *OpusRecorderController) CreateWebMData(sessionID string, source string) ([]byte, error) {
	// Извлекаем фреймы из хранилища
	packets, err := c.loadPacketsFromStore(sessionID)
	if err != nil {
		log.Printf("Failed to load packets from store for session %s: %v", sessionID, err)
		return nil, err
	}

	log.Printf("DEBUG: Creating WebM data from %d packets for session %s (source: %s)", len(packets), sessionID, source)

	// Сортируем пакеты по времени
	sort.Slice(packets, func(i, j int) bool {
		return packets[i].Timestamp.Before(packets[j].Timestamp)
	})

	// Фильтруем пакеты по направлению
	var filteredPackets []OpusPacket
	for _, packet := range packets {
		if packet.Source == source {
			filteredPackets = append(filteredPackets, packet)
		}
	}

	log.Printf("DEBUG: Found %d packets for source %s", len(filteredPackets), source)

	// Проверяем, есть ли пакеты для указанного направления
	if len(filteredPackets) == 0 {
		return nil, fmt.Errorf("no packets found for source: %s", source)
	}

	// Создаем WebM данные для указанного направления
	var trackName string
	if source == UserSource {
		trackName = fmt.Sprintf("User#%s", sessionID)
	} else {
		trackName = fmt.Sprintf("Assistant#%s", sessionID)
	}

	webmData, err := c.createSingleTrackWebMData(filteredPackets, trackName)
	if err != nil {
		return nil, fmt.Errorf("failed to create WebM data for %s: %w", source, err)
	}

	return webmData, nil
}

// createSingleTrackWebMData создает WebM данные с одним треком
func (c *OpusRecorderController) createSingleTrackWebMData(packets []OpusPacket, trackName string) ([]byte, error) {
	log.Printf("Creating %s WebM data with %d packets", trackName, len(packets))

	// Создаем буфер для записи данных с обёрткой WriteCloser
	buffer := &BufferWriteCloser{Buffer: &bytes.Buffer{}}

	// Создаем один моно трек
	tracks := []webm.TrackEntry{
		{
			Name:        trackName,
			TrackNumber: 1,
			TrackUID:    1,
			CodecID:     "A_OPUS",
			TrackType:   2, // Audio
			Audio: &webm.Audio{
				SamplingFrequency: 24000,
				Channels:          1,
			},
		},
	}

	// Создаем writer с буфером
	writers, err := webm.NewSimpleBlockWriter(buffer, tracks)
	if err != nil {
		return nil, fmt.Errorf("failed to create writer: %w", err)
	}

	writer := writers[0]

	// Находим базовое время
	var baseTime time.Time
	if len(packets) > 0 {
		baseTime = packets[0].Timestamp
	}

	// Записываем все пакеты
	totalBytes := 0
	for i, packet := range packets {
		timecodeMs := packet.Timestamp.Sub(baseTime).Milliseconds()
		if timecodeMs < 0 {
			timecodeMs = 0
		}

		n, err := writer.Write(true, timecodeMs, packet.Data)
		if err != nil {
			log.Printf("Error writing packet %d: %v", i, err)
		} else {
			totalBytes += n
		}
	}

	// Закрываем writer
	if err := writer.Close(); err != nil {
		log.Printf("Error closing writer: %v", err)
	}

	return buffer.Bytes(), nil
}

// Close завершает все активные записи (хранилище закрывает вызывающий код)
func (c *OpusRecorderController) Close() {
	c.mutex.RLock()
	sessions := make([]string, 0, len(c.recordings))
	for sessionID := range c.recordings {
		sessions = append(sessions, sessionID)
	}
	c.mutex.RUnlock()

	for _, sessionID := range sessions {
		c.finalizeSession(sessionID, "controller_closed")
	}

	log.Printf("OpusRecorderController closed, finalized %d recordings", len(sessions))
}
