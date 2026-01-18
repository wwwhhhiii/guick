package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"sync"

	"github.com/google/uuid"
)

type Message struct {
	FromPeerId   uuid.UUID
	ToChatId     uuid.UUID
	FromPeerAddr string
	ToPeerAddr   string
	Txt          string `json:"text"`
}

func NewMsg(
	txt string,
	fromPeerId uuid.UUID,
	toChatId uuid.UUID,
	fromPeerAddr string,
	toPeerAddr string,
) *Message {
	return &Message{
		Txt:          txt,
		FromPeerId:   fromPeerId,
		ToChatId:     toChatId,
		FromPeerAddr: fromPeerAddr,
		ToPeerAddr:   toPeerAddr,
	}
}

type EncryptedMessage struct {
	Ciphertext []byte `json:"ciphertext"`
	Nonce      []byte `json:"nonce"`
}

func NewEncryptedMessage(ciphertext []byte, nonce []byte) *EncryptedMessage {
	return &EncryptedMessage{
		Ciphertext: ciphertext,
		Nonce:      nonce,
	}
}

// decrypts structured peer message data into plaintext
func DecryptMessageData(data []byte, aesgcm cipher.AEAD) ([]byte, error) {
	message := &EncryptedMessage{}
	if err := json.Unmarshal(data, message); err != nil {
		return nil, err
	}
	return DecryptMessage(message.Ciphertext, message.Nonce, aesgcm)
}

func AesGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func EncryptMessage(plaintext []byte, aesgcm cipher.AEAD) (*EncryptedMessage, error) {
	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := aesgcm.Seal(nil, nonce, plaintext, nil)
	return NewEncryptedMessage(ciphertext, nonce), nil
}

func DecryptMessage(ciphertext []byte, nonce []byte, aesgcm cipher.AEAD) ([]byte, error) {
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}

type Chat struct {
	mu       sync.RWMutex
	id       uuid.UUID
	peers    map[uuid.UUID]*Peer
	isHosted bool
}

func NewChat(id uuid.UUID, isHosted bool) *Chat {
	return &Chat{
		id:       id,
		peers:    make(map[uuid.UUID]*Peer, 100),
		isHosted: isHosted,
	}
}

func (c *Chat) addPeers(peers ...*Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range peers {
		if _, exist := c.peers[p.PeerId]; !exist {
			c.peers[p.PeerId] = p
		}
	}
}

func (c *Chat) rmPeers(peers ...*Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, peer := range peers {
		delete(c.peers, peer.PeerId)
	}
}

func (c *Chat) sendMessage(m *Message) error {
	if m.ToChatId != c.id {
		return errors.New("message mismatched chat id")
	}
	for id, peer := range c.peers {
		if id == m.FromPeerId {
			continue
		}
		peer.SendMessage(m.Txt)
	}
	return nil
}

type Hub struct {
	mu sync.RWMutex
	// registry of chats with peers
	chats map[uuid.UUID]*Chat
	// peer to register in a hub
	register chan *Peer
	// peer id to unregister and close connection to it
	unregister chan *Peer
	// coming from peer
	sendMessage chan *Message
	// coming from goroutine listening to peer messages
	recvMessage chan *Message
	// Peers successfuly registered in hub are sent here.
	// Use this to receive registred peers events
	OnPeerRegister chan<- *Peer
	// Sends peers that were unregistered in hub.
	// Use this to receive registered peers events
	OnPeerUnregister chan<- *Peer
	// Sends messages that were received from peers.
	// Use this to receive messages from peers
	OnMsgRecv chan<- *Message
	// Sends messages that were sent to peer.
	// Use this to receive messages that you sent
	OnMsgSent chan<- *Message
}

func newHub(
	onClientReg chan<- *Peer,
	onClientUnreg chan<- *Peer,
	onMsgRecv chan<- *Message,
	onMsgSent chan<- *Message,
) *Hub {
	return &Hub{
		chats:            make(map[uuid.UUID]*Chat, 100),
		register:         make(chan *Peer),
		unregister:       make(chan *Peer),
		sendMessage:      make(chan *Message),
		recvMessage:      make(chan *Message),
		OnPeerRegister:   onClientReg,
		OnPeerUnregister: onClientUnreg,
		OnMsgRecv:        onMsgRecv,
		OnMsgSent:        onMsgSent,
	}
}

func (hub *Hub) LockedPeekChat(id uuid.UUID) (*Chat, bool) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	chat, exist := hub.chats[id]
	return chat, exist
}

// use this to add new peers to hub.
// peer should be connected
func (hub *Hub) RegisterPeer(p *Peer) {
	hub.register <- p
}

// use this to delete peer from hub.
// wil disconnect peer and do all necessary cleanups
func (hub *Hub) UnregisterClient(p *Peer) {
	hub.unregister <- p
}

func (hub *Hub) getOrCreateChat(id uuid.UUID, isHosted bool) *Chat {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	chat, exist := hub.chats[id]
	if !exist {
		chat = NewChat(id, isHosted)
		hub.chats[chat.id] = chat
	}
	return chat
}

// closes connection with the peer.
// deletes peer record from a chat.
// emits peer unregistered event.
func (hub *Hub) disconnectPeer(p *Peer) error {
	if err := p.GracefulDisconnect(); err != nil {
		p.conn.Close()
	}
	return nil
}

func (hub *Hub) closePeerConnection(p *Peer) {
	p.conn.Close()
}

func (hub *Hub) Run(interrupt <-chan os.Signal) {
	for {
		select {
		case <-interrupt:
			slog.Info("received interrupt signal")
			hub.Shutdown()
			return
		case peer := <-hub.register:
			chatIsHosted := peer.connType == TypeServer
			chat := hub.getOrCreateChat(peer.ChatId, chatIsHosted)
			chat.addPeers(peer)
			slog.Info("peer added to chat", "chatId", chat.id, "peerId", peer.PeerId)
			pingStop := make(chan struct{})
			go StartConnPing(peer.conn, pingPeriod, pingStop)
			go func() {
				defer close(pingStop)
				for msg := range peer.ReadMessagesGen() {
					hub.recvMessage <- msg
					// reading stops when peer connection is closed by peer or broken
				}
				pingStop <- struct{}{}
				hub.closePeerConnection(peer)
				slog.Info("closed peer connection", "peerId", peer.PeerId)
				chat.rmPeers(peer)
				slog.Info("peer removed from chat", "chatId", peer.ChatId, "peerId", peer.PeerId)
				hub.OnPeerUnregister <- peer
			}()
			hub.OnPeerRegister <- peer
		case peer := <-hub.unregister:
			// TODO mb chat.disconnectPeer?
			if err := hub.disconnectPeer(peer); err != nil {
				slog.Error("peer disconnect", "error", err)
			}
			// TODO remove chat if no peers left?
			chat, exist := hub.LockedPeekChat(peer.ChatId)
			if exist {
				chat.rmPeers(peer)
			}
			hub.OnPeerUnregister <- peer
			slog.Info("peer removed from chat", "chatId", peer.ChatId, "peerId", peer.PeerId)
		case msg := <-hub.sendMessage:
			chat, exist := hub.chats[msg.ToChatId]
			if !exist {
				log.Fatal("send message", "unknown chat id", msg.ToChatId)
			}
			if err := chat.sendMessage(msg); err != nil {
				slog.Error("send message", "error", err)
				continue
			}
			hub.OnMsgSent <- msg
		case msg := <-hub.recvMessage:
			chat, exist := hub.chats[msg.ToChatId]
			if !exist {
				log.Fatal("broadcast message", "unknown chat id", msg.ToChatId)
			}
			// broadcas messages if is chat host
			if chat.isHosted {
				if err := chat.sendMessage(msg); err != nil {
					slog.Error("broadcast message", "error", err)
					continue
				}
			}
			hub.OnMsgRecv <- msg
		}
	}
}

func (hub *Hub) Shutdown() {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, chat := range hub.chats {
		for _, peer := range chat.peers {
			hub.disconnectPeer(peer)
		}
	}
}
