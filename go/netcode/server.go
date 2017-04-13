package netcode

import (
	"log"
	"net"
	"time"
)

type Server struct {
	serverConn       *NetcodeConn
	serverAddr       *net.UDPAddr
	shutdownCh       chan struct{}
	serverTime       int64
	running          bool
	maxClients       int
	connectedClients int

	clientManager  *ClientManager
	globalSequence uint64

	ignoreRequests  bool
	ignoreResponses bool
	allowedPackets  []byte
	protocolId      uint64

	privateKey   []byte
	challengeKey []byte

	challengeSequence uint64

	recvBytes int
}

func NewServer(serverAddress *net.UDPAddr, privateKey []byte, protocolId uint64, maxClients int) *Server {
	s := &Server{}
	s.serverAddr = serverAddress
	s.protocolId = protocolId
	s.privateKey = privateKey
	s.maxClients = maxClients

	s.globalSequence = uint64(1) << 63
	s.clientManager = NewClientManager(maxClients)
	s.shutdownCh = make(chan struct{})

	// set allowed packets for this server
	s.allowedPackets = make([]byte, ConnectionNumPackets)
	s.allowedPackets[ConnectionRequest] = 1
	s.allowedPackets[ConnectionResponse] = 1
	s.allowedPackets[ConnectionKeepAlive] = 1
	s.allowedPackets[ConnectionPayload] = 1
	s.allowedPackets[ConnectionDisconnect] = 1
	return s
}

func (s *Server) SetAllowedPackets(allowedPackets []byte) {
	s.allowedPackets = allowedPackets
}

func (s *Server) SetIgnoreRequests(val bool) {
	s.ignoreRequests = val
}

func (s *Server) SetIgnoreResponses(val bool) {
	s.ignoreResponses = val
}

func (s *Server) Init() error {
	var err error

	s.challengeKey, err = GenerateKey()
	if err != nil {
		return err
	}
	s.serverConn = NewNetcodeConn()
	s.serverConn.SetRecvHandler(s.onPacketData)
	return nil
}

func (s *Server) Listen() error {
	s.running = true

	if err := s.serverConn.Listen(s.serverAddr); err != nil {
		return err
	}
	return nil
}

func (s *Server) onPacketData(packetData []byte, addr *net.UDPAddr) {
	var readPacketKey []byte
	var replayProtection *ReplayProtection

	if !s.running {
		return
	}

	encryptionIndex := -1

	clientIndex := s.clientManager.FindClientIndexByAddress(addr)
	if clientIndex != -1 {
		encryptionIndex = s.clientManager.FindEncryptionIndexByClientIndex(clientIndex)
	} else {
		encryptionIndex = s.clientManager.FindEncryptionEntryIndex(addr, s.serverTime)
	}

	size := len(packetData)
	if len(packetData) == 0 {
		log.Printf("unable to read from socket, 0 bytes returned")
		return
	}

	log.Printf("net client connected")

	timestamp := uint64(time.Now().Unix())
	log.Printf("read %d from socket\n", len(packetData))

	packet := NewPacket(packetData)
	packetBuffer := NewBufferFromBytes(packetData)

	if clientIndex != -1 {
		client := s.clientManager.instances[clientIndex]
		readPacketKey = client.connectToken.ClientKey
		replayProtection = client.replayProtection
	}

	if err := packet.Read(packetBuffer, size, s.protocolId, timestamp, readPacketKey, s.privateKey, s.allowedPackets, replayProtection); err != nil {
		log.Printf("error reading packet: %s from %s\n", err, addr)
		return
	}

	s.processPacket(clientIndex, encryptionIndex, packet, addr, s.allowedPackets, timestamp)
}

func (s *Server) processPacket(clientIndex, encryptionIndex int, packet Packet, addr *net.UDPAddr, allowedPackets []byte, timestamp uint64) {

	switch packet.GetType() {
	case ConnectionRequest:
		if s.ignoreRequests {
			return
		}
		log.Printf("server received connection request from %s\n", addr.String())
		s.processConnectionRequest(packet, addr)
	case ConnectionResponse:
		if s.ignoreResponses {
			return
		}
		log.Printf("server received connection response from %s\n", addr.String())
		s.processConnectionResponse(clientIndex, encryptionIndex, packet, addr)
	case ConnectionKeepAlive:
		if clientIndex == -1 {
			return
		}
		client := s.clientManager.instances[clientIndex]
		client.lastRecvTime = s.serverTime

		if !client.confirmed {
			client.confirmed = true
			log.Printf("server confirmed connection to client %d:%s\n", client.clientId, client.address.String())
		}
	case ConnectionPayload:
		if clientIndex == -1 {
			return
		}
		client := s.clientManager.instances[clientIndex]
		client.lastRecvTime = s.serverTime

		if !client.confirmed {
			client.confirmed = true
			log.Printf("server confirmed connection to client %d:%s\n", client.clientId, client.address.String())
		}

		client.packetQueue.Push(packet)
	case ConnectionDisconnect:
		if clientIndex == -1 {
			return
		}
		client := s.clientManager.instances[clientIndex]
		log.Printf("server received disconnect packet from client %d:%s\n", client.clientId, client.address.String())
	}
}

func (s *Server) processConnectionRequest(packet Packet, addr *net.UDPAddr) {
	requestPacket, ok := packet.(*RequestPacket)
	if !ok {
		return
	}

	if len(requestPacket.Token.ServerAddrs) == 0 {
		log.Printf("server ignored connection request. server address not in connect token whitelist\n")
		return
	}

	for _, addr := range requestPacket.Token.ServerAddrs {
		if !addressEqual(s.serverAddr, &addr) {
			log.Printf("server ignored connection request. server address not in connect token whitelist\n")
			return
		}
	}

	clientIndex := s.clientManager.FindClientIndexByAddress(addr)
	if clientIndex != -1 {
		log.Printf("server ignored connection request. a client with this address is already connected\n")
	}

	clientIndex = s.clientManager.FindClientIndexById(requestPacket.Token.ClientId)
	if clientIndex != -1 {
		log.Printf("server ignored connection request. a client with this id has already been used\n")
	}

	if !s.clientManager.FindOrAddTokenEntry(requestPacket.ConnectTokenData[CONNECT_TOKEN_PRIVATE_BYTES-MAC_BYTES:], addr, s.serverTime) {
		log.Printf("server ignored connection request. connect token has already been used\n")
	}

	if s.clientManager.ConnectedClientCount() == s.maxClients {
		log.Printf("server denied connection request. server is full\n")
		// send denied packet
		return
	}

	if !s.clientManager.AddEncryptionMapping(requestPacket.Token, addr, s.serverTime, s.serverTime+TIMEOUT_SECONDS) {
		log.Printf("server ignored connection request. failed to add encryption mapping\n")
		return
	}

	s.sendChallengePacket(requestPacket, addr)
}

func (s *Server) sendChallengePacket(requestPacket *RequestPacket, addr *net.UDPAddr) {
	challenge := NewChallengeToken(requestPacket.Token.ClientId)
	challengeBuf := challenge.Write(requestPacket.Token.UserData[:USER_DATA_BYTES])
	challengeSequence := s.challengeSequence

	s.challengeSequence++

	if err := EncryptChallengeToken(&challengeBuf, challengeSequence, s.challengeKey); err != nil {
		log.Printf("server ignored connection request. failed to encrypt challenge token\n")
		return
	}
	challengePacket := &ChallengePacket{}
	challengePacket.ChallengeTokenData = challengeBuf
	challengePacket.ChallengeTokenSequence = challengeSequence

	buffer := NewBuffer(MAX_PACKET_BYTES)
	if _, err := challengePacket.Write(buffer, s.protocolId, s.globalSequence, requestPacket.Token.ServerKey); err != nil {
		log.Printf("server error while writing challenge packet\n")
		return
	}
	s.globalSequence++

	log.Printf("server sent connection challenge packet\n")
	s.sendGlobalPacket(buffer.Bytes(), addr)
}

func (s *Server) sendGlobalPacket(packetBuffer []byte, addr *net.UDPAddr) {
	if _, err := s.serverConn.WriteTo(packetBuffer, addr); err != nil {
		log.Printf("error sending packet to %s\n", addr.String())
	}
}

func (s *Server) processConnectionResponse(clientIndex, encryptionIndex int, packet Packet, addr *net.UDPAddr) {
	var err error
	var tokenBuffer []byte
	var challengeToken *ChallengeToken

	responsePacket, ok := packet.(*ResponsePacket)
	if !ok {
		return
	}

	if tokenBuffer, err = DecryptChallengeToken(responsePacket.ChallengeTokenData, responsePacket.ChallengeTokenSequence, s.challengeKey); err != nil {
		log.Printf("failed to decrypt challenge token: %s\n", err)
		return
	}

	if challengeToken, err = ReadChallengeToken(tokenBuffer); err != nil {
		log.Printf("failed to read challenge token: %s\n", err)
		return
	}

	sendKey := s.clientManager.GetEncryptionEntrySendKey(encryptionIndex)
	if sendKey == nil {
		log.Printf("server ignored connection response. no packet send key\n")
	}

	if s.clientManager.FindClientIndexByAddress(addr) != -1 {
		log.Printf("server ignored connection response. a client with this address is already connected")
	}

	if s.clientManager.FindClientIndexById(challengeToken.ClientId) != -1 {
		log.Printf("server ignored connection response. a client with this id is already connected")
	}

	if s.clientManager.ConnectedClientCount() == s.maxClients {
		log.Printf("server denied connection response. server is full\n")
		deniedPacket := &DeniedPacket{}
		packetBuffer := NewBuffer(MAX_PACKET_BYTES)
		if _, err := deniedPacket.Write(packetBuffer, s.protocolId, s.globalSequence, sendKey); err != nil {
			log.Printf("error creating denied packet: %s\n", err)
			return
		}
		s.globalSequence++
		s.sendGlobalPacket(packetBuffer.Bytes(), addr)
		return
	}

	s.connectClient(clientIndex, encryptionIndex, challengeToken, addr)
	return

}

func (s *Server) connectClient(clientIndex, encryptionIndex int, challengeToken *ChallengeToken, addr *net.UDPAddr) {

	if s.clientManager.ConnectedClientCount() > s.maxClients {
		log.Printf("maxium number of clients reached")
		return
	}

	s.clientManager.SetEncryptionEntryExpiration(encryptionIndex, -1)
	client := s.clientManager.instances[clientIndex]
	client.connected = true
	client.clientId = challengeToken.ClientId
	client.sequence = 0
	client.address = addr
	client.lastSendTime = s.serverTime
	client.lastRecvTime = s.serverTime
	copy(client.userData, challengeToken.UserData.Bytes())
	log.Printf("server accepted client %d from %s in slot: %d\n", client.clientId, addr.String())
	// SEND PACKET client.SendPacket(...)
}

func (s *Server) Update(time int64) error {
	s.serverTime = time

	if err := s.sendPackets(); err != nil {
		return err
	}

	if err := s.checkTimeouts(); err != nil {
		return err
	}
	return nil
}

func (s *Server) checkTimeouts() error {
	return nil
}

func (s *Server) recvPackets() error {
	return nil
}

func (s *Server) sendPackets() error {
	return nil
}

func (s *Server) sendClientPacket(packet Packet, client *ClientInstance) error {
	return nil
}

func (s *Server) disconnectClient(client *ClientInstance) error {
	return nil
}

func (s *Server) disconnectAll() error {
	return nil
}

func (s *Server) Stop() error {
	if s.running {
		close(s.shutdownCh)
		s.serverConn.Close()
		s.running = false
	}
	return nil
}

func addressEqual(addr1, addr2 *net.UDPAddr) bool {
	return addr1.IP.Equal(addr2.IP) && addr1.Port == addr2.Port
}
