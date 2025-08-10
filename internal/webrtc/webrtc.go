package webrtc

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"sip-webrtc-openai/internal/bridge"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

type WebRTCEndpoint struct {
	id               string
	identifier       string // НОВОЕ: номер телефона/идентификатор
	conn             *websocket.Conn
	peerConnection   *webrtc.PeerConnection
	localAudioTrack  *webrtc.TrackLocalStaticSample // Поле для отправки аудио
	remoteAudioTrack *webrtc.TrackRemote            // Поле для получения аудио
	bridge           *bridge.Bridge
	session          *bridge.BridgeSession
	mu               sync.RWMutex
	isClosed         bool
	ctx              context.Context
	cancel           context.CancelFunc
}

type SignalingMessage struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // В продакшене добавьте проверку origin
	},
}

func NewWebRTCService(bridge *bridge.Bridge) *WebRTCService {
	return &WebRTCService{
		bridge:    bridge,
		endpoints: make(map[string]*WebRTCEndpoint),
	}
}

type WebRTCService struct {
	bridge    *bridge.Bridge
	endpoints map[string]*WebRTCEndpoint
	mu        sync.RWMutex
}

func (s *WebRTCService) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	identifier := r.URL.Query().Get("user")

	ctx, cancel := context.WithCancel(context.Background())
	endpoint := &WebRTCEndpoint{
		id:         fmt.Sprintf("webrtc-%d", time.Now().UnixNano()),
		identifier: identifier,
		conn:       conn,
		bridge:     s.bridge,
		ctx:        ctx,
		cancel:     cancel,
	}

	s.mu.Lock()
	s.endpoints[endpoint.id] = endpoint
	s.mu.Unlock()

	log.Printf("WebRTC endpoint connected: %s with identifier: %s", endpoint.id, identifier)

	// Создаем bridge сессию с identifier
	session, err := s.bridge.CreateSession(endpoint, identifier, "webrtc") // ИЗМЕНЕНО: передаем identifier
	if err != nil {
		log.Printf("Failed to create bridge session: %v", err)
		endpoint.Close()
		return
	}
	endpoint.session = session

	// Обрабатываем сообщения
	go endpoint.handleMessages()

	// Ждем закрытия соединения
	<-ctx.Done()
	s.removeEndpoint(endpoint.id)
}

func (s *WebRTCService) removeEndpoint(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if endpoint, exists := s.endpoints[id]; exists {
		endpoint.Close()
		delete(s.endpoints, id)
		log.Printf("WebRTC endpoint removed: %s", id)
	}
}

func (e *WebRTCEndpoint) GetID() string {
	return e.id
}

// GetIdentifier возвращает номер телефона/идентификатор
func (e *WebRTCEndpoint) GetIdentifier() string {
	return e.identifier
}

// SendOpusFrame отправляет сырой Opus frame через WebRTC
func (e *WebRTCEndpoint) SendOpusFrame(opusData []byte) error {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.isClosed {
		return fmt.Errorf("endpoint closed")
	}

	if e.localAudioTrack == nil {
		return fmt.Errorf("local audio track not initialized")
	}

	// Создаем media.Sample для Opus frame
	sample := media.Sample{
		Data:     opusData,
		Duration: time.Millisecond * 20, // Opus frame = 20ms
	}

	// Отправляем через WebRTC local audio track
	if err := e.localAudioTrack.WriteSample(sample); err != nil {
		return fmt.Errorf("failed to write Opus sample: %w", err)
	}

	return nil
}

func (e *WebRTCEndpoint) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.isClosed {
		return
	}
	e.isClosed = true

	if e.session != nil {
		e.bridge.RemoveSession(e.session.GetID())
	}

	if e.peerConnection != nil {
		e.peerConnection.Close()
	}

	if e.conn != nil {
		e.conn.Close()
	}

	e.cancel()
	log.Printf("WebRTC endpoint closed: %s", e.id)
}

func (e *WebRTCEndpoint) handleMessages() {
	defer e.Close()

	for {
		select {
		case <-e.ctx.Done():
			return
		default:
			var msg SignalingMessage
			err := e.conn.ReadJSON(&msg)
			if err != nil {
				log.Printf("Error reading WebSocket message: %v", err)
				return
			}

			if err := e.handleSignalingMessage(msg); err != nil {
				log.Printf("Error handling signaling message: %v", err)
				return
			}
		}
	}
}

func (e *WebRTCEndpoint) handleSignalingMessage(msg SignalingMessage) error {
	switch msg.Type {
	case "offer":
		return e.handleOffer(msg.Data)
	case "ice-candidate":
		return e.handleICECandidate(msg.Data)
	default:
		return nil
	}
}

func (e *WebRTCEndpoint) handleOffer(data interface{}) error {
	offerData, ok := data.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid offer data")
	}

	sdpStr, ok := offerData["sdp"].(string)
	if !ok {
		return fmt.Errorf("invalid SDP in offer")
	}

	typeStr, ok := offerData["type"].(string)
	if !ok {
		return fmt.Errorf("invalid type in offer")
	}

	// Создаем peer connection
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("failed to create peer connection: %w", err)
	}

	e.mu.Lock()
	e.peerConnection = pc
	e.mu.Unlock()

	// Создаем локальный аудио трек для отправки
	localAudioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "webrtc-audio",
	)
	if err != nil {
		return fmt.Errorf("failed to create local audio track: %w", err)
	}

	e.localAudioTrack = localAudioTrack

	// Добавляем трек к peer connection
	_, err = pc.AddTrack(localAudioTrack)
	if err != nil {
		return fmt.Errorf("failed to add local audio track: %w", err)
	}

	// Обрабатываем входящие треки
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			e.remoteAudioTrack = track
			go e.handleIncomingAudio(track)
		}
	})

	// Обрабатываем ICE кандидатов
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			e.sendSignalingMessage("ice-candidate", candidate.ToJSON())
		}
	})

	// Устанавливаем remote description
	offer := webrtc.SessionDescription{
		Type: webrtc.NewSDPType(typeStr),
		SDP:  sdpStr,
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		return fmt.Errorf("failed to set remote description: %w", err)
	}

	// Создаем ответ
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("failed to create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("failed to set local description: %w", err)
	}

	// Отправляем ответ
	return e.sendSignalingMessage("answer", answer)
}

func (e *WebRTCEndpoint) handleICECandidate(data interface{}) error {
	candidateData, ok := data.(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid ice candidate data")
	}

	// Правильная обработка типов для ICE candidate
	candidateStr, ok := candidateData["candidate"].(string)
	if !ok {
		return fmt.Errorf("invalid candidate string")
	}

	// sdpMid может быть string или nil
	var sdpMid *string
	if mid, exists := candidateData["sdpMid"]; exists && mid != nil {
		if midStr, ok := mid.(string); ok {
			sdpMid = &midStr
		}
	}

	// sdpMLineIndex может быть number или nil
	var sdpMLineIndex *uint16
	if lineIndex, exists := candidateData["sdpMLineIndex"]; exists && lineIndex != nil {
		if indexFloat, ok := lineIndex.(float64); ok {
			index := uint16(indexFloat)
			sdpMLineIndex = &index
		}
	}

	candidate := webrtc.ICECandidateInit{
		Candidate:     candidateStr,
		SDPMid:        sdpMid,
		SDPMLineIndex: sdpMLineIndex,
	}

	e.mu.RLock()
	pc := e.peerConnection
	e.mu.RUnlock()

	if pc != nil {
		return pc.AddICECandidate(candidate)
	}

	return fmt.Errorf("peer connection not available")
}

func (e *WebRTCEndpoint) handleIncomingAudio(track *webrtc.TrackRemote) {
	for {
		select {
		case <-e.ctx.Done():
			return
		default:
			packet, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("Error reading RTP packet: %v", err)
				return
			}

			if len(packet.Payload) == 0 {
				continue
			}

			// ПРЯМАЯ ПЕРЕДАЧА: Отправляем сырой Opus frame без декодирования
			if e.session != nil {
				e.bridge.ProcessIncomingOpus(e.session, packet.Payload)
			}
		}
	}
}

func (e *WebRTCEndpoint) sendSignalingMessage(msgType string, data interface{}) error {
	e.mu.RLock()
	conn := e.conn
	closed := e.isClosed
	e.mu.RUnlock()

	if closed || conn == nil {
		return fmt.Errorf("connection not available")
	}

	msg := SignalingMessage{
		Type: msgType,
		Data: data,
	}

	return conn.WriteJSON(msg)
}
