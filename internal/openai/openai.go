package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

type OpenAIService struct {
	apiKey string

	// WebRTC components
	peerConnection *webrtc.PeerConnection
	dataChannel    *webrtc.DataChannel
	audioTrack     *webrtc.TrackLocalStaticSample

	// Synchronization
	mu      sync.RWMutex
	writeMu sync.Mutex

	// Handlers
	OnOpusData        func([]byte)
	OnTextData        func(msg Message, raw []byte)
	OnSpeechStart     func()
	OnResponseDone    func()
	OnResponseCreated func(string)
	OnError           func(error) // Новый хендлер для ошибок
	OnClose           func()      // Новый хендлер для закрытия соединения

	// State
	isResponsePlaying int32
	currentItemID     string
	itemMutex         sync.RWMutex
	closed            int32

	// WebRTC specific
	ctx          context.Context
	cancel       context.CancelFunc
	ephemeralKey string
	prompt       string
}

type Message struct {
	Type  string                 `json:"type"`
	Delta string                 `json:"delta,omitempty"`
	Audio string                 `json:"audio,omitempty"`
	Data  map[string]interface{} `json:",inline"`
}

type ResponseCancel struct {
	Type string `json:"type"`
}

type ConversationItemTruncate struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	ContentIndex int    `json:"content_index"`
	AudioEnd     int64  `json:"audio_end"`
}

// Session creation request/response
type SessionRequest struct {
	Model        string `json:"model"`
	Voice        string `json:"voice,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type SessionResponse struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	ExpiresAt    int64  `json:"expires_at"`
	ClientSecret struct {
		Value     string `json:"value"`
		ExpiresAt int64  `json:"expires_at"`
	} `json:"client_secret"`
}

func NewOpenAIService(apiKey string, prompt string) *OpenAIService {
	ctx, cancel := context.WithCancel(context.Background())
	return &OpenAIService{
		apiKey: apiKey,
		ctx:    ctx,
		cancel: cancel,
		prompt: prompt,
	}
}

// notifyError безопасно вызывает OnError хендлер
func (s *OpenAIService) notifyError(err error) {
	if s.OnError != nil {
		// Вызываем в горутине чтобы не блокировать основную логику
		go s.OnError(err)
	}
}

// notifyClose безопасно вызывает OnClose хендлер
func (s *OpenAIService) notifyClose() {
	if s.OnClose != nil {
		// Вызываем в горутине чтобы не блокировать основную логику
		go s.OnClose()
	}
}

func (s *OpenAIService) Connect() error {
	if s.apiKey == "" {
		err := fmt.Errorf("API key is empty")
		s.notifyError(err)
		return err
	}

	// Step 1: Get ephemeral key
	if err := s.getEphemeralKey(); err != nil {
		err = fmt.Errorf("failed to get ephemeral key: %w", err)
		s.notifyError(err)
		return err
	}

	// Step 2: Create WebRTC peer connection
	if err := s.createPeerConnection(); err != nil {
		err = fmt.Errorf("failed to create peer connection: %w", err)
		s.notifyError(err)
		return err
	}

	// Step 3: Setup data channel and audio track
	if err := s.setupDataChannel(); err != nil {
		err = fmt.Errorf("failed to setup data channel: %w", err)
		s.notifyError(err)
		return err
	}

	if err := s.setupAudioTrack(); err != nil {
		err = fmt.Errorf("failed to setup audio track: %w", err)
		s.notifyError(err)
		return err
	}

	// Step 4: Perform WebRTC signaling
	if err := s.performWebRTCSignaling(); err != nil {
		err = fmt.Errorf("failed to perform WebRTC signaling: %w", err)
		s.notifyError(err)
		return err
	}

	log.Printf("OpenAI WebRTC service connected")
	return nil
}

// Step 1: Get ephemeral key from OpenAI
func (s *OpenAIService) getEphemeralKey() error {
	sessionReq := SessionRequest{
		Model:        "gpt-4o-realtime-preview-2024-12-17",
		Voice:        "ballad",
		Instructions: "Говори по-русски, отвечай кратко и естественно, предлагай русский язык в начале, но будь готов переключиться.",
	}

	jsonData, err := json.Marshal(sessionReq)
	if err != nil {
		return fmt.Errorf("failed to marshal session request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/realtime/sessions", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get ephemeral key, status: %d, body: %s", resp.StatusCode, string(body))
	}

	var sessionResp SessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&sessionResp); err != nil {
		return fmt.Errorf("failed to decode session response: %w", err)
	}

	s.ephemeralKey = sessionResp.ClientSecret.Value
	return nil
}

// Step 2: Create WebRTC peer connection
func (s *OpenAIService) createPeerConnection() error {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("failed to create peer connection: %w", err)
	}

	s.mu.Lock()
	s.peerConnection = pc
	s.mu.Unlock()

	// Setup WebRTC event handlers
	s.setupWebRTCHandlers()

	return nil
}

func (s *OpenAIService) setupDataChannel() error {
	s.mu.RLock()
	pc := s.peerConnection
	s.mu.RUnlock()

	if pc == nil {
		return fmt.Errorf("peer connection is nil")
	}

	// Create data channel for text/control messages
	dc, err := pc.CreateDataChannel("oai-events", nil)
	if err != nil {
		return fmt.Errorf("failed to create data channel: %w", err)
	}

	s.dataChannel = dc

	// Setup data channel handlers
	dc.OnOpen(func() {
		// Configure session once data channel is open
		if err := s.configure(); err != nil {
			log.Printf("Failed to configure session: %v", err)
			s.notifyError(fmt.Errorf("failed to configure session: %w", err))
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.handleDataChannelMessage(msg.Data)
	})

	dc.OnClose(func() {
		log.Printf("Data channel closed")
		s.notifyClose()
	})

	dc.OnError(func(err error) {
		log.Printf("Data channel error: %v", err)
		s.notifyError(fmt.Errorf("data channel error: %w", err))
	})

	return nil
}

func (s *OpenAIService) setupAudioTrack() error {
	s.mu.RLock()
	pc := s.peerConnection
	s.mu.RUnlock()

	if pc == nil {
		return fmt.Errorf("peer connection is nil")
	}

	// Create audio track for sending audio - используем Opus напрямую
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "openai-audio",
	)
	if err != nil {
		return fmt.Errorf("failed to create audio track: %w", err)
	}

	s.audioTrack = audioTrack

	// Add track to peer connection
	_, err = pc.AddTrack(audioTrack)
	if err != nil {
		return fmt.Errorf("failed to add audio track: %w", err)
	}

	return nil
}

func (s *OpenAIService) setupWebRTCHandlers() {
	s.mu.RLock()
	pc := s.peerConnection
	s.mu.RUnlock()

	if pc == nil {
		return
	}

	// Handle user audio tracks
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			go s.handleIncomingAudio(track)
		}
	})

	// Handle ICE connection state changes
	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		switch connectionState {
		case webrtc.ICEConnectionStateFailed:
			log.Printf("ICE connection failed")
			s.notifyError(fmt.Errorf("ICE connection failed"))
		case webrtc.ICEConnectionStateDisconnected:
			log.Printf("ICE connection disconnected")
			s.notifyClose()
		case webrtc.ICEConnectionStateClosed:
			log.Printf("ICE connection closed")
			s.notifyClose()
		case webrtc.ICEConnectionStateConnected:
			log.Printf("ICE connection established")
		case webrtc.ICEConnectionStateCompleted:
			log.Printf("ICE connection completed")
		}
	})

	// Handle connection state changes
	pc.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		switch connectionState {
		case webrtc.PeerConnectionStateFailed:
			log.Printf("Peer connection failed")
			s.notifyError(fmt.Errorf("peer connection failed"))
		case webrtc.PeerConnectionStateDisconnected:
			log.Printf("Peer connection disconnected")
			s.notifyClose()
		case webrtc.PeerConnectionStateClosed:
			log.Printf("Peer connection closed")
			s.notifyClose()
		case webrtc.PeerConnectionStateConnected:
			log.Printf("Peer connection established")
		}
	})
}

// Обработка входящего аудио - передаем сырые Opus frames
func (s *OpenAIService) handleIncomingAudio(track *webrtc.TrackRemote) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Panic in handleIncomingAudio: %v", r)
			s.notifyError(fmt.Errorf("panic in audio handler: %v", r))
		}
	}()

	for {
		if atomic.LoadInt32(&s.closed) == 1 {
			return
		}

		packet, _, err := track.ReadRTP()
		if err != nil {
			if err == io.EOF {
				log.Printf("Audio track EOF")
				s.notifyClose()
				return
			}
			log.Printf("Error reading RTP packet: %v", err)
			s.notifyError(fmt.Errorf("error reading RTP packet: %w", err))
			continue
		}

		// Проверяем что payload не пустой
		if len(packet.Payload) == 0 {
			continue
		}

		// Прямая передача Opus frames без декодирования
		if s.OnOpusData != nil {
			s.OnOpusData(packet.Payload)
		}
	}
}

// Step 4: Perform WebRTC signaling with OpenAI
func (s *OpenAIService) performWebRTCSignaling() error {
	s.mu.RLock()
	pc := s.peerConnection
	s.mu.RUnlock()

	if pc == nil {
		return fmt.Errorf("peer connection is nil")
	}

	// Create SDP offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("failed to create offer: %w", err)
	}

	// Set local description
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("failed to set local description: %w", err)
	}

	// Send offer to OpenAI and get answer
	answer, err := s.sendOfferToOpenAI(offer.SDP)
	if err != nil {
		return fmt.Errorf("failed to send offer to OpenAI: %w", err)
	}

	// Set remote description
	remoteDesc := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
	}

	if err := pc.SetRemoteDescription(remoteDesc); err != nil {
		return fmt.Errorf("failed to set remote description: %w", err)
	}

	return nil
}

// Send SDP offer to OpenAI realtime endpoint
func (s *OpenAIService) sendOfferToOpenAI(sdp string) (string, error) {
	model := "gpt-4o-realtime-preview-2024-12-17"
	url := fmt.Sprintf("https://api.openai.com/v1/realtime?model=%s", model)

	req, err := http.NewRequest("POST", url, bytes.NewBufferString(sdp))
	if err != nil {
		return "", fmt.Errorf("failed to create signaling request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.ephemeralKey)
	req.Header.Set("Content-Type", "application/sdp")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send offer: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("signaling failed, status: %d, body: %s", resp.StatusCode, string(body))
	}

	answerSDP, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read answer SDP: %w", err)
	}

	return string(answerSDP), nil
}

func (s *OpenAIService) configure() error {
	cfg := map[string]interface{}{
		"type": "session.update",
		"session": map[string]interface{}{
			"modalities":   []string{"audio", "text"},
			"instructions": s.prompt,
			"voice":        "ballad",
			"turn_detection": map[string]interface{}{
				"type":                "server_vad",
				"threshold":           0.8,
				"prefix_padding_ms":   200,
				"silence_duration_ms": 600,
			},
			"input_audio_transcription": map[string]interface{}{
				"model": "gpt-4o-transcribe",
			},
		},
	}

	// Отправляем конфигурацию
	if err := s.sendMessage(cfg); err != nil {
		return err
	}

	// Автоматически создаем первый ответ для приветствия
	createResponse := map[string]interface{}{
		"type": "response.create",
		"response": map[string]interface{}{
			"modalities": []string{"text", "audio"},
		},
	}

	return s.sendMessage(createResponse)
}

// Отправка готовых Opus frames (например, от другого WebRTC соединения)
func (s *OpenAIService) SendOpusAudio(opusData []byte) error {
	if atomic.LoadInt32(&s.closed) == 1 {
		return fmt.Errorf("service is closed")
	}

	if s.audioTrack == nil {
		return fmt.Errorf("audio track not initialized")
	}

	// Отправляем Opus frame напрямую через WebRTC
	sample := media.Sample{
		Data:     opusData,
		Duration: time.Millisecond * 20, // Предполагаем 20ms frame
	}

	if err := s.audioTrack.WriteSample(sample); err != nil {
		// Не вызываем notifyError здесь, так как это может быть временная проблема
		return fmt.Errorf("failed to write Opus sample: %w", err)
	}

	return nil
}

func (s *OpenAIService) InterruptResponse(currentItemID string, playedSamples int64) error {
	if atomic.LoadInt32(&s.closed) == 1 {
		return fmt.Errorf("service is closed")
	}

	atomic.StoreInt32(&s.isResponsePlaying, 0)

	// Send response.cancel
	cancelPayload := ResponseCancel{
		Type: "response.cancel",
	}
	if err := s.sendMessage(cancelPayload); err != nil {
		return err
	}

	// Truncate if we have current item
	if currentItemID != "" {
		audioEndMs := playedSamples / 24

		truncatePayload := ConversationItemTruncate{
			Type:         "conversation.item.truncate",
			ItemID:       currentItemID,
			ContentIndex: 0,
			AudioEnd:     audioEndMs,
		}

		if err := s.sendMessage(truncatePayload); err != nil {
			return err
		}
	}

	return nil
}

func (s *OpenAIService) sendMessage(msg interface{}) error {
	if atomic.LoadInt32(&s.closed) == 1 {
		return fmt.Errorf("service is closed")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.dataChannel == nil {
		return fmt.Errorf("data channel not available")
	}

	if s.dataChannel.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("data channel not open")
	}

	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	return s.dataChannel.Send(jsonData)
}

func (s *OpenAIService) handleDataChannelMessage(data []byte) {
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("unmarshal error: %v", err)
		s.notifyError(fmt.Errorf("failed to unmarshal message: %w", err))
		return
	}

	//	log.Printf("Received message:\n%s", string(data))

	s.handleMessage(msg, data)
}

func (s *OpenAIService) handleMessage(msg Message, raw []byte) {
	switch msg.Type {
	case "session.created":
		if id, ok := msg.Data["id"].(string); ok {
			log.Printf("OpenAI Session created: %s", id)
		}

	case "input_audio_buffer.speech_started":
		if s.OnSpeechStart != nil {
			s.OnSpeechStart()
		}

	case "input_audio_buffer.speech_stopped":
		// User finished speaking - no log needed

	case "response.created":
		if response, ok := msg.Data["response"].(map[string]interface{}); ok {
			if id, ok := response["id"].(string); ok {
				s.itemMutex.Lock()
				s.currentItemID = id
				s.itemMutex.Unlock()
				if s.OnResponseCreated != nil {
					s.OnResponseCreated(id)
				}
			}
		}

	case "response.output_item.added":
		if item, ok := msg.Data["item"].(map[string]interface{}); ok {
			if id, ok := item["id"].(string); ok {
				s.itemMutex.Lock()
				s.currentItemID = id
				s.itemMutex.Unlock()
				if s.OnResponseCreated != nil {
					s.OnResponseCreated(id)
				}
			}
		}

	case "response.audio.delta":
		// В WebRTC режиме аудио приходит через WebRTC audio track
		atomic.StoreInt32(&s.isResponsePlaying, 1)
		// Игнорируем аудио данные из data channel в WebRTC режиме

	case "response.audio.done":
		// Audio generation completed - no log needed

	case "response.done":
		atomic.StoreInt32(&s.isResponsePlaying, 0)
		if s.OnResponseDone != nil {
			s.OnResponseDone()
		}
		s.itemMutex.Lock()
		s.currentItemID = ""
		s.itemMutex.Unlock()

	case "response.cancelled":
		atomic.StoreInt32(&s.isResponsePlaying, 0)
		s.itemMutex.Lock()
		s.currentItemID = ""
		s.itemMutex.Unlock()

	case "conversation.item.truncated":
		// Conversation item truncated - no log needed

	case "conversation.item.input_audio_transcription.delta":
		if s.OnTextData != nil {
			s.OnTextData(msg, raw)
		}
	case "conversation.item.input_audio_transcription.completed":
		if s.OnTextData != nil {
			s.OnTextData(msg, raw)
		}

	case "response.audio_transcript.delta":
		if s.OnTextData != nil {
			s.OnTextData(msg, raw)
		}
	case "response.audio_transcript.done":
		if s.OnTextData != nil {
			s.OnTextData(msg, raw)
		}

	case "response.content_part.done":
		if s.OnTextData != nil {
			s.OnTextData(msg, raw)
		}
	case "response.output_item.done":
		if s.OnTextData != nil {
			s.OnTextData(msg, raw)
		}

	case "error":
		log.Printf("OpenAI error: %s", string(raw))

		// Создаем структурированную ошибку
		var errorMsg string
		if errMsg, ok := msg.Data["message"].(string); ok {
			errorMsg = errMsg
		} else {
			errorMsg = string(raw)
		}

		err := fmt.Errorf("OpenAI API error: %s", errorMsg)
		s.notifyError(err)

		if errType, ok := msg.Data["type"].(string); ok {
			switch errType {
			case "invalid_request_error":
				log.Printf("Invalid request - check if item exists or already cancelled")
			case "server_error":
				log.Printf("Server error - may need to retry")
			case "authentication_error":
				log.Printf("Authentication error - check API key")
			case "rate_limit_error":
				log.Printf("Rate limit error - reduce request frequency")
			}
		}
	}
}

func (s *OpenAIService) Close() {
	// Предотвращаем повторное закрытие
	if !atomic.CompareAndSwapInt32(&s.closed, 0, 1) {
		return
	}

	log.Printf("Closing OpenAI service...")

	// Отменяем контекст
	s.cancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Закрываем data channel
	if s.dataChannel != nil {
		s.dataChannel.Close()
		s.dataChannel = nil
	}

	// Закрываем peer connection
	if s.peerConnection != nil {
		s.peerConnection.Close()
		s.peerConnection = nil
	}

	// Уведомляем о закрытии
	s.notifyClose()

	log.Printf("OpenAI service closed")
}
