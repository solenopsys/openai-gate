package sip

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/pion/stun"

	"sip-webrtc-openai/internal/bridge"
)

const (
	maxPacketSize = 1500
	rtpTimeout    = 500 * time.Millisecond
	// Opus at 48kHz with 20ms frames = 960 samples per frame
	opusSamplesPerFrame = 960
)

type SIPEndpoint struct {
	id         string
	identifier string // НОВОЕ: номер телефона из SIP заголовков
	callID     string
	remoteAddr *net.UDPAddr
	rtpConn    *net.UDPConn
	bridge     *bridge.Bridge
	session    *bridge.BridgeSession
	server     *SIPServer

	// Atomic flags
	active int32
	closed int32

	// RTP state (atomic)
	seqNum    uint32
	timestamp uint32
	ssrc      uint32

	// Track last received timestamp for proper sequencing
	lastRecvTimestamp uint32
	timestampBase     uint32
	firstPacket       int32

	mu     sync.RWMutex
	cancel context.CancelFunc
}

type SIPServer struct {
	ua         *sipgo.UserAgent
	server     *sipgo.Server
	host       string
	publicHost string
	port       int
	bridge     *bridge.Bridge
	endpoints  sync.Map
	ctx        context.Context
	cancel     context.CancelFunc

	// Simple counters
	packetsProcessed int64
	packetsDropped   int64
	packetsSent      int64
	packetsReceived  int64 // Add received counter
}

func NewSIPServer(host string, publicHost string, port int, bridge *bridge.Bridge) *SIPServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &SIPServer{
		host:       host,
		publicHost: publicHost,
		port:       port,
		bridge:     bridge,
		ctx:        ctx,
		cancel:     cancel,
	}
}

func (s *SIPServer) Start() error {
	ua, err := sipgo.NewUA()
	if err != nil {
		return err
	}
	s.ua = ua

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return err
	}
	s.server = srv

	srv.OnInvite(s.handleInvite)
	srv.OnAck(s.handleAck)
	srv.OnBye(s.handleBye)
	srv.OnCancel(s.handleCancel)
	srv.OnRegister(s.handleRegister)

	log.Printf("SIP Server starting on %s:%d", s.host, s.port)

	// Start stats reporter
	go s.statsReporter()

	return srv.ListenAndServe(s.ctx, "udp4", fmt.Sprintf("%s:%d", s.host, s.port))
}

func (s *SIPServer) Stop() {
	s.cancel()

	// Close all endpoints
	s.endpoints.Range(func(key, value interface{}) bool {
		if ep, ok := value.(*SIPEndpoint); ok {
			s.closeEndpoint(ep)
		}
		return true
	})

	if s.ua != nil {
		s.ua.Close()
	}
}

func (s *SIPServer) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	// НОВОЕ: извлекаем номер телефона из SIP заголовка From
	identifier := ""
	if from := req.From(); from != nil {
		identifier = from.Address.User
		log.Printf("✅ Extracted identifier from SIP From header: %s", identifier)
	}

	// Если identifier пустой, пробуем другие заголовки
	if identifier == "" {
		if contact := req.Contact(); contact != nil {
			identifier = contact.Address.User
			log.Printf("✅ Extracted identifier from SIP Contact header: %s", identifier)
		}
	}

	/* ---------- 1. создаём UDP-сокет для RTP ---------- */
	localPort := s.getPort()
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", localPort))
	if err != nil {
		log.Printf("❌ resolveUDP %s: %v", callID, err)
		tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		return
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Printf("❌ listenUDP %s: %v", callID, err)
		tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		return
	}
	conn.SetReadBuffer(65536)
	conn.SetWriteBuffer(65536)

	/* ---------- 2. узнаём внешний IP:port ЭТОГО сокета ---------- */
	publicIP := s.publicHost
	publicPort := localPort
	if ip, port, perr := discoverPublicMapping(conn); perr == nil {
		publicIP, publicPort = ip, port
		log.Printf("✅ Discovered public mapping for %s: %s:%d", callID, publicIP, publicPort)
	} else {
		log.Printf("⚠️ STUN failed for %s: %v, fallback %s:%d", callID, perr, publicIP, publicPort)
	}

	/* ---------- 3. регистрируем endpoint ---------- */
	ssrc := rand.Uint32()
	endpoint := &SIPEndpoint{
		id:            callID,
		identifier:    identifier, // НОВОЕ: сохраняем идентификатор
		callID:        callID,
		ssrc:          ssrc,
		seqNum:        uint32(rand.Intn(65536)),
		timestampBase: uint32(rand.Intn(90000)),
		rtpConn:       conn,
		server:        s,
		firstPacket:   1,
	}
	s.endpoints.Store(callID, endpoint)

	/* ---------- 4. быстрая сигнализация SIP ---------- */
	trying := sip.NewResponseFromRequest(req, 100, "Trying", nil)
	tx.Respond(trying)

	// SDP клиента → вытащим адрес/порт
	if rtpAddr := s.extractClientRTPAddress(req); rtpAddr != nil {
		endpoint.remoteAddr = rtpAddr
		log.Printf("✅ Client RTP addr %s: %s", callID, rtpAddr)
	}

	/* ---------- 5. отдаём 200 OK с нашим внешним адресом ---------- */
	sdpBody := s.generateOpusOnlySDP(publicIP, publicPort)
	ok := sip.NewResponseFromRequest(req, 200, "OK", []byte(sdpBody))
	contactURI := sip.Uri{User: "gateway", Host: s.publicHost, Port: s.port}
	ok.AppendHeader(&sip.ContactHeader{Address: contactURI})
	tx.Respond(ok)

	/* ---------- 6. запускаем RTP-обработчик ---------- */
	ctx, cancel := context.WithCancel(s.ctx)
	endpoint.cancel = cancel
	go s.processRTP(ctx, endpoint, conn)
}

func (s *SIPServer) handleAck(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	if ep, ok := s.endpoints.Load(callID); ok {
		endpoint := ep.(*SIPEndpoint)

		// Activate
		atomic.StoreInt32(&endpoint.active, 1)

		// Create bridge session с identifier
		session, err := s.bridge.CreateSession(endpoint, endpoint.identifier, "sip") // ИЗМЕНЕНО: передаем identifier
		if err != nil {
			log.Printf("Bridge session failed for %s: %v", callID, err)
			return
		}

		endpoint.session = session
		endpoint.bridge = s.bridge

		log.Printf("✅ Bridge session created for SIP %s with identifier: %s", callID, endpoint.identifier)

		// Send dummy packets if remoteAddr set
		endpoint.mu.RLock()
		conn := endpoint.rtpConn
		remoteAddr := endpoint.remoteAddr
		endpoint.mu.RUnlock()
		if conn != nil && remoteAddr != nil {
			go s.sendDummyRTPPackets(endpoint, conn, remoteAddr, 5)
		}
	}
}

func discoverPublicMapping(conn *net.UDPConn) (ip string, port int, err error) {
	const stunSrv = "stun.l.google.com:19302"

	srvAddr, err := net.ResolveUDPAddr("udp", stunSrv)
	if err != nil {
		return
	}

	// Собираем Binding-request
	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest)

	// Отправляем тем же сокетом
	if _, err = conn.WriteToUDP(req.Raw, srvAddr); err != nil {
		return
	}

	buf := make([]byte, 1500)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		n, addr, rerr := conn.ReadFromUDP(buf) // <-- возвращает *net.UDPAddr
		if rerr != nil {
			err = rerr
			return
		}
		if !addr.IP.Equal(srvAddr.IP) || addr.Port != srvAddr.Port {
			continue // не от STUN-сервера
		}

		var resp stun.Message
		resp.Raw = buf[:n]
		if perr := resp.Decode(); perr != nil {
			continue
		}

		var xor stun.XORMappedAddress
		if perr := xor.GetFrom(&resp); perr != nil {
			continue
		}

		ip = xor.IP.String()
		port = int(xor.Port)
		return
	}
}

func (s *SIPServer) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	if ep, ok := s.endpoints.LoadAndDelete(callID); ok {
		endpoint := ep.(*SIPEndpoint)
		s.closeEndpoint(endpoint)
	}

	ok := sip.NewResponseFromRequest(req, 200, "OK", nil)
	tx.Respond(ok)
}

func (s *SIPServer) handleCancel(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID().Value()

	if ep, ok := s.endpoints.LoadAndDelete(callID); ok {
		endpoint := ep.(*SIPEndpoint)
		s.closeEndpoint(endpoint)
	}

	ok := sip.NewResponseFromRequest(req, 200, "OK", nil)
	tx.Respond(ok)
}

func (s *SIPServer) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	ok := sip.NewResponseFromRequest(req, 200, "OK", nil)
	if contact := req.Contact(); contact != nil {
		ok.AppendHeader(contact)
	}
	expires := sip.NewHeader("Expires", "3600")
	ok.AppendHeader(expires)
	tx.Respond(ok)
}

// RTP processing with enhanced logging
func (s *SIPServer) processRTP(ctx context.Context, endpoint *SIPEndpoint, conn *net.UDPConn) {
	buffer := make([]byte, maxPacketSize)

	endpoint.mu.RLock()
	expectedAddr := endpoint.remoteAddr
	endpoint.mu.RUnlock()

	log.Printf("🎯 RTP processing started for %s on %s (expecting from %s)",
		endpoint.id, conn.LocalAddr(), expectedAddr)

	packetCount := 0

	for {
		select {
		case <-ctx.Done():
			log.Printf("RTP processing stopped for %s (received %d packets)", endpoint.id, packetCount)
			return
		default:
			if atomic.LoadInt32(&endpoint.closed) == 1 {
				return
			}

			conn.SetReadDeadline(time.Now().Add(rtpTimeout))
			n, addr, err := conn.ReadFromUDP(buffer)

			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Send silence dummy every timeout to keep NAT open
					endpoint.mu.RLock()
					remote := endpoint.remoteAddr
					endpoint.mu.RUnlock()
					if remote != nil && atomic.LoadInt32(&endpoint.active) == 1 {
						endpoint.sendSilenceDummy(conn, remote)
					}
					continue
				}
				if atomic.LoadInt32(&endpoint.closed) == 1 {
					return
				}
				log.Printf("❌ RTP read error for %s: %v", endpoint.id, err)
				continue
			}

			packetCount++
			atomic.AddInt64(&s.packetsReceived, 1)

			// Log first packet and every 100th
			if packetCount == 1 {
				log.Printf("🎉 First RTP packet received for %s from %s: %d bytes", endpoint.id, addr, n)
				// Log hex dump of first packet for debugging
				if n >= 12 {
					packet := &rtp.Packet{}
					if err := packet.Unmarshal(buffer[:n]); err == nil {
						log.Printf("📦 First packet details: PT=%d, Seq=%d, TS=%d, SSRC=%x",
							packet.PayloadType, packet.SequenceNumber, packet.Timestamp, packet.SSRC)
					}
				}
			} else if packetCount%100 == 0 {
				log.Printf("📥 Received RTP packet #%d for %s from %s: %d bytes", packetCount, endpoint.id, addr, n)
			}

			// Update remoteAddr if different (NAT detection)
			endpoint.mu.Lock()
			if endpoint.remoteAddr == nil || !endpoint.remoteAddr.IP.Equal(addr.IP) || endpoint.remoteAddr.Port != addr.Port {
				oldAddr := endpoint.remoteAddr
				endpoint.remoteAddr = addr
				log.Printf("🔄 Remote RTP address updated for %s: %s -> %s (NAT detected)", endpoint.id, oldAddr, addr)
			}
			endpoint.mu.Unlock()

			s.handleRTPPacketPassthrough(endpoint, buffer[:n])
		}
	}
}

// Discover public address via STUN
func (s *SIPServer) discoverPublicAddress(localAddr string) (string, error) {
	stunServer := "stun.l.google.com:19302"
	c, err := stun.Dial("udp", stunServer)
	if err != nil {
		return "", err
	}
	defer c.Close()

	message, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return "", err
	}

	var resp *stun.Message
	err = c.Do(message, func(res stun.Event) {
		if res.Error != nil {
			err = res.Error
			return
		}
		resp = res.Message
	})
	if err != nil {
		return "", err
	}

	var xorAddr stun.XORMappedAddress
	if err := xorAddr.GetFrom(resp); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d", xorAddr.IP, xorAddr.Port), nil
}

// Extract client RTP address from SDP
func (s *SIPServer) extractClientRTPAddress(req *sip.Request) *net.UDPAddr {
	body := req.Body()
	if len(body) == 0 {
		log.Printf("No SDP body in request")
		return nil
	}

	sdp := string(body)
	log.Printf("Parsing SDP: %s", sdp)

	var clientIP string
	var clientPort int

	lines := strings.Split(sdp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "c=") {
			parts := strings.Split(line, " ")
			if len(parts) >= 3 {
				clientIP = parts[2]
			}
		} else if strings.HasPrefix(line, "m=audio ") {
			parts := strings.Split(line, " ")
			if len(parts) >= 2 {
				if port, err := strconv.Atoi(parts[1]); err == nil {
					clientPort = port
				}
			}
		}
	}

	if clientIP != "" && clientPort > 0 {
		addrStr := fmt.Sprintf("%s:%d", clientIP, clientPort)
		addr, err := net.ResolveUDPAddr("udp", addrStr)
		if err == nil {
			return addr
		}
		log.Printf("Failed to resolve client RTP addr: %s (%v)", addrStr, err)
	} else {
		log.Printf("No valid c= or m= in SDP")
	}

	return nil
}

// Send multiple dummy RTP packets to create NAT hole punching
func (s *SIPServer) sendDummyRTPPackets(endpoint *SIPEndpoint, conn *net.UDPConn, clientAddr *net.UDPAddr, count int) {
	// Initial delay
	time.Sleep(100 * time.Millisecond)

	for i := 0; i < count; i++ {
		// Check if endpoint is still active
		if atomic.LoadInt32(&endpoint.active) == 0 || atomic.LoadInt32(&endpoint.closed) == 1 {
			return
		}

		// Random delay between packets (0-200ms)
		time.Sleep(time.Duration(rand.Intn(200)) * time.Millisecond)

		// Get current sequence and timestamp
		seq := atomic.AddUint32(&endpoint.seqNum, 1)
		ts := endpoint.timestampBase + uint32(i)*opusSamplesPerFrame

		// Create minimal RTP packet for NAT punching
		dummyRTP := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111, // Opus
				SequenceNumber: uint16(seq),
				Timestamp:      ts,
				SSRC:           endpoint.ssrc,
			},
			Payload: []byte{}, // Empty payload for NAT punching
		}

		data, err := dummyRTP.Marshal()
		if err != nil {
			log.Printf("Failed to marshal dummy RTP %d: %v", i, err)
			continue
		}

		_, err = conn.WriteToUDP(data, clientAddr)
		if err != nil {
			log.Printf("❌ Failed to send dummy RTP %d to %s: %v", i, clientAddr, err)
		} else {
			log.Printf("✅ Sent dummy RTP %d to %s for NAT traversal", i, clientAddr)
		}
	}
}

// Pure passthrough - no transcoding (like WebRTC)
func (s *SIPServer) handleRTPPacketPassthrough(endpoint *SIPEndpoint, data []byte) {
	log.Printf("📦 handleRTPPacketPassthrough called for %s with %d bytes", endpoint.id, len(data))

	if len(data) < 12 {
		log.Printf("❌ Packet too small for %s: %d bytes", endpoint.id, len(data))
		atomic.AddInt64(&s.packetsDropped, 1)
		return
	}

	packet := &rtp.Packet{}
	if err := packet.Unmarshal(data); err != nil {
		log.Printf("❌ Failed to unmarshal RTP packet for %s: %v", endpoint.id, err)
		atomic.AddInt64(&s.packetsDropped, 1)
		return
	}

	atomic.AddInt64(&s.packetsProcessed, 1)

	// Initialize timestamp tracking on first packet
	if atomic.CompareAndSwapInt32(&endpoint.firstPacket, 1, 0) {
		endpoint.lastRecvTimestamp = packet.Timestamp
		log.Printf("🎯 First RTP packet for %s: seq=%d, ts=%d", endpoint.id, packet.SequenceNumber, packet.Timestamp)
	}

	if len(packet.Payload) == 0 {
		log.Printf("⚠️ Empty payload for %s", endpoint.id)
		return
	}

	// Support both PT=96 and PT=111 for better compatibility
	if endpoint.session != nil {
		switch packet.PayloadType {
		case 96, 111: // Opus (both variants)
			log.Printf("🎤 Received Opus frame from SIP %s: %d bytes (PT=%d, seq=%d, ts=%d)",
				endpoint.id, len(packet.Payload), packet.PayloadType, packet.SequenceNumber, packet.Timestamp)
			s.bridge.ProcessIncomingOpus(endpoint.session, packet.Payload)
		default:
			// Log unsupported payload type but don't process
			if atomic.LoadInt64(&s.packetsProcessed)%100 == 1 {
				log.Printf("⚠️ Unsupported payload type for %s: %d (expected Opus PT=96/111)", endpoint.id, packet.PayloadType)
			}
		}
	} else {
		log.Printf("⚠️ No session for endpoint %s", endpoint.id)
	}
}

// AudioEndpoint interface - minimal implementation
func (e *SIPEndpoint) GetID() string {
	return e.id
}

// GetIdentifier возвращает номер телефона/идентификатор
func (e *SIPEndpoint) GetIdentifier() string {
	return e.identifier
}

func (e *SIPEndpoint) SendOpusFrame(opusData []byte) error {
	if atomic.LoadInt32(&e.active) == 0 || atomic.LoadInt32(&e.closed) == 1 || len(opusData) < 5 {
		return nil
	}

	for attempt := 0; attempt < 5; attempt++ {
		e.mu.RLock()
		conn := e.rtpConn
		remoteAddr := e.remoteAddr
		e.mu.RUnlock()

		if conn == nil || remoteAddr == nil {
			log.Printf("⚠️ Connection not ready for %s (attempt %d), waiting...", e.id, attempt+1)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Calculate proper timestamp
		seq := atomic.AddUint32(&e.seqNum, 1)
		// For Opus, increment by 960 samples (20ms at 48kHz) per frame
		ts := atomic.AddUint32(&e.timestamp, opusSamplesPerFrame)
		// Add base to avoid starting from 0
		actualTs := e.timestampBase + ts

		rtpPacket := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: uint16(seq),
				Timestamp:      actualTs,
				SSRC:           e.ssrc,
			},
			Payload: opusData,
		}

		data, err := rtpPacket.Marshal()
		if err != nil {
			return err
		}

		_, err = conn.WriteToUDP(data, remoteAddr)
		if err == nil {
			log.Printf("✅ Sent Opus frame to SIP %s: %d bytes (seq=%d, ts=%d)",
				e.id, len(opusData), uint16(seq), actualTs)
			atomic.AddInt64(&e.server.packetsSent, 1)
			return nil
		}
		log.Printf("❌ Send error for %s: %v (attempt %d)", e.id, err, attempt+1)
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("failed after retries: connection not ready")
}

// SendAudio remains for backward compatibility
func (e *SIPEndpoint) SendAudio(audioData []byte) error {
	log.Printf("⚠️ SendAudio called on SIP endpoint %s - consider using SendOpusFrame for better performance", e.id)

	if len(audioData) > 20 && len(audioData) < 1000 {
		return e.SendOpusFrame(audioData)
	}

	return fmt.Errorf("PCM to Opus encoding not implemented in SIP endpoint")
}

// Send silence dummy (small Opus frame) to keep NAT and provoke client
func (e *SIPEndpoint) sendSilenceDummy(conn *net.UDPConn, clientAddr *net.UDPAddr) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return
	}

	silenceOpus := []byte{0xf8, 0xff, 0xfe} // Minimal silence Opus frame (3 bytes)

	seq := atomic.AddUint32(&e.seqNum, 1)
	ts := atomic.AddUint32(&e.timestamp, opusSamplesPerFrame)
	actualTs := e.timestampBase + ts

	rtpPacket := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111,
			SequenceNumber: uint16(seq),
			Timestamp:      actualTs,
			SSRC:           e.ssrc,
		},
		Payload: silenceOpus,
	}

	data, err := rtpPacket.Marshal()
	if err != nil {
		return
	}

	_, err = conn.WriteToUDP(data, clientAddr)
	if err == nil {
		log.Printf("✅ Sent silence dummy to %s for endpoint %s (seq=%d, ts=%d)",
			clientAddr, e.id, uint16(seq), actualTs)
		atomic.AddInt64(&e.server.packetsSent, 1)
	} else {
		log.Printf("⚠️ Failed to send silence dummy for %s: %v", e.id, err)
	}
}

func (e *SIPEndpoint) Close() {
	// Prevent double close
	if !atomic.CompareAndSwapInt32(&e.closed, 0, 1) {
		return
	}

	atomic.StoreInt32(&e.active, 0)

	if e.cancel != nil {
		e.cancel()
	}

	// Give time for RTP processing to exit cleanly
	time.Sleep(50 * time.Millisecond)

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.rtpConn != nil {
		e.rtpConn.Close()
		e.rtpConn = nil
	}
}

func (s *SIPServer) closeEndpoint(endpoint *SIPEndpoint) {
	// Mark as inactive first
	atomic.StoreInt32(&endpoint.active, 0)

	// Remove from bridge first
	if endpoint.session != nil {
		s.bridge.RemoveSession(endpoint.id)
	}

	// Then close the endpoint
	endpoint.Close()
}

func (s *SIPServer) getPort() int {
	minPort := getEnvAsInt("WEBRTC_PORT_MIN", 10000)
	maxPort := getEnvAsInt("WEBRTC_PORT_MAX", 65000)

	for port := minPort; port < maxPort; port += 2 {
		addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			continue
		}
		conn.Close()
		return port
	}
	return minPort
}

func getEnvAsInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return fallback
}

// Opus-only SDP generation with publicIP and publicPort
func (s *SIPServer) generateOpusOnlySDP(publicIP string, publicPort int) string {
	sessionID := time.Now().Unix()

	return fmt.Sprintf(`v=0
o=openai %d %d IN IP4 %s
s=OpenAI
c=IN IP4 %s
t=0 0
m=audio %d RTP/AVP 111 96
a=rtpmap:111 opus/48000/2
a=rtpmap:96 opus/48000/2
a=fmtp:111 minptime=10;useinbandfec=1
a=fmtp:96 minptime=10;useinbandfec=1
a=sendrecv
a=ptime:20
`, sessionID, sessionID, publicIP, publicIP, publicPort)
}

// Stats reporter
func (s *SIPServer) statsReporter() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			processed := atomic.LoadInt64(&s.packetsProcessed)
			dropped := atomic.LoadInt64(&s.packetsDropped)
			sent := atomic.LoadInt64(&s.packetsSent)
			received := atomic.LoadInt64(&s.packetsReceived)
			if processed > 0 || dropped > 0 || sent > 0 || received > 0 {
				log.Printf("📊 RTP Stats: %d received, %d processed, %d sent, %d dropped",
					received, processed, sent, dropped)
			}
		}
	}
}
