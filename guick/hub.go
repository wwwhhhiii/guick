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
	ToPeerId     uuid.UUID
	FromPeerAddr string
	ToPeerAddr   string
	Txt          string `json:"text"`
}

func NewMsg(
	txt string,
	fromPeerId uuid.UUID,
	toPeerId uuid.UUID,
	fromPeerAddr string,
	toPeerAddr string,
) *Message {
	return &Message{
		Txt:          txt,
		FromPeerId:   fromPeerId,
		ToPeerId:     toPeerId,
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
func DecryptMessageData(data []byte, aesgcm cipher.AEAD) (string, error) {
	message := &EncryptedMessage{}
	if err := json.Unmarshal(data, message); err != nil {
		return "", err
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

func EncryptMessage(plaintext string, aesgcm cipher.AEAD) (*EncryptedMessage, error) {
	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := aesgcm.Seal(nil, nonce, []byte(plaintext), nil)
	return NewEncryptedMessage(ciphertext, nonce), nil
}

func DecryptMessage(ciphertext []byte, nonce []byte, aesgcm cipher.AEAD) (string, error) {
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

type Hub struct {
	mu sync.RWMutex

	// mapping of peer id to its client representation
	clients map[uuid.UUID]*Client

	// peer to register in a hub
	register chan *Client

	// peer id to unregister and close connection to it
	unregister chan *Client

	// coming from client
	sendMessage chan *Message

	// coming from goroutine listening to peer messages
	recvMessage chan *Message

	// Clients successfuly registered in hub are sent here.
	// Use this to receive registred clients events
	OnClientReg chan<- *Client

	// Sends clients that were unregistered in hub.
	// Use this to receive registered clients events
	OnClientUnreg chan<- *Client

	// Sends messages that were received from peers.
	// Use this to receive messages from peers
	OnMsgRecv chan<- *Message

	// Sends messages that were sent to peer.
	// Use this to receive messages that you sent
	OnMsgSent chan<- *Message
}

func newHub(
	onClientReg chan<- *Client,
	onClientUnreg chan<- *Client,
	onMsgRecv chan<- *Message,
	onMsgSent chan<- *Message,
) *Hub {
	return &Hub{
		clients:       make(map[uuid.UUID]*Client, 100),
		register:      make(chan *Client, 100),
		unregister:    make(chan *Client, 100),
		sendMessage:   make(chan *Message, 100),
		recvMessage:   make(chan *Message, 100),
		OnClientReg:   onClientReg,
		OnClientUnreg: onClientUnreg,
		OnMsgRecv:     onMsgRecv,
		OnMsgSent:     onMsgSent,
	}
}

// use this to add new clients to hub.
// client should be connected
func (hub *Hub) RegisterClient(client *Client) {
	hub.register <- client
}

// use this to delete clients from hub.
// wil disconnect client and do all necessary cleanups
func (hub *Hub) UnregisterClient(client *Client) {
	hub.unregister <- client
}

// shorthand for searching and removing client from hub by UUID.
// returns error if client with such UUID is not found in hub
func (hub *Hub) UnregisterClientByUUID(clientUUID uuid.UUID) error {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	client, exist := hub.clients[clientUUID]
	if !exist {
		return errors.New("client not registered")
	}
	hub.unregister <- client
	return nil
}

// adds client record to hub registry
func (hub *Hub) registerClient(c *Client) error {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if _, exist := hub.clients[c.PeerId]; exist {
		return errors.New("peer already registered")
	}
	hub.clients[c.PeerId] = c
	return nil
}

// closes connection with the client.
// deletes client record from a hub.
// emits client unregistered event.
func (hub *Hub) disconnectClient(client *Client) error {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if err := client.GracefulDisconnect(); err != nil {
		client.conn.Close()
	}
	delete(hub.clients, client.PeerId)
	hub.OnClientUnreg <- client
	return nil
}

func (hub *Hub) closeClientConnection(client *Client) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	client.conn.Close()
	delete(hub.clients, client.PeerId)
	hub.OnClientUnreg <- client
}

func (hub *Hub) Run(interrupt <-chan os.Signal) {
	for {
		select {
		case <-interrupt:
			slog.Info("hub interrupted")
			hub.Shutdown()
			return
		case client := <-hub.register:
			if err := hub.registerClient(client); err != nil {
				slog.Error("client register error", "error", err, "location", "hub")
				continue
			}
			slog.Info("client registered", "client", client.PeerId, "location", "hub")
			go func() {
				pingStop := ConfigureClientConnection(client.conn)
				defer close(pingStop)
				for msg := range client.ReadMessagesGen() {
					hub.recvMessage <- msg
					// reading stops when peer connection is closed or broken
				}
				pingStop <- struct{}{}
				hub.closeClientConnection(client)
				slog.Info("connection with peer closed", "peerId", client.PeerId)
			}()
			hub.OnClientReg <- client
		case client := <-hub.unregister:
			if err := hub.disconnectClient(client); err != nil {
				slog.Error("client unregister error", "error", err, "location", "hub")
				continue
			}
			slog.Info("client unregistered", "client", client.PeerId, "location", "hub")
		case msg := <-hub.sendMessage:
			client, exist := hub.clients[msg.ToPeerId]
			if !exist {
				log.Fatal("unknown peer", msg.ToPeerId)
			}
			if err := client.SendMessage(msg.Txt); err != nil {
				slog.Error("client send message", "error", err)
				continue
			}
			hub.OnMsgSent <- msg
		case msg := <-hub.recvMessage:
			hub.OnMsgRecv <- msg
		}
	}
}

func (hub *Hub) Shutdown() {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	delKeys := make([]uuid.UUID, len(hub.clients))
	for _, client := range hub.clients {
		client.GracefulDisconnect()
		delKeys = append(delKeys, client.PeerId)
	}
	for _, key := range delKeys {
		delete(hub.clients, key)
	}
}
