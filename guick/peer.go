package main

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type ConnType int

const (
	TypeServer ConnType = iota
	TypeClient
)

const (
	maxMessageSizeBytes = 512

	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
	writeWait  = 10 * time.Second
)

type Peer struct {
	// chat peer belongs to
	ChatId uuid.UUID
	PeerId uuid.UUID
	Name   string
	// peer connection
	conn *websocket.Conn
	// who is this peer: client or a server
	connType ConnType
	// encryption block to communicate with peer
	// aesgcm cipher.AEAD
	key []byte
}

func NewPeer(chatId uuid.UUID, peerId uuid.UUID, name string, conn *websocket.Conn, connT ConnType, key []byte) *Peer {
	return &Peer{
		ChatId:   chatId,
		PeerId:   peerId,
		Name:     name,
		conn:     conn,
		connType: connT,
		key:      key,
		// aesgcm:   aesgcm,
	}
}

// gracefully closes peer connection
func (p *Peer) GracefulDisconnect() error {
	return p.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseNormalClosure, ""),
	)
}

// encrypts and writes message to underlying peer connection
func (p *Peer) SendMessage(text string) error {
	encryptedMessage, err := EncryptMessage([]byte(text), p.key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(encryptedMessage)
	if err != nil {
		return err
	}
	// TODO: change message type to websocket.BinaryMessage
	if err := p.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return err
	}
	return nil
}

func StartConnPing(connection *websocket.Conn, pingInterval time.Duration, stop <-chan struct{}) {
	connection.SetReadLimit(maxMessageSizeBytes)
	connection.SetReadDeadline(time.Now().Add(pongWait))
	connection.SetPongHandler(func(appData string) error {
		connection.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			err := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
			if err != nil {
				slog.Error("write ping message", "error", err)
				return
			}
			slog.Debug("ping", "who", connection.RemoteAddr())
		case <-stop:
			return
		}
	}
}

func (p *Peer) ReadMessagesGen() <-chan *Message {
	out := make(chan *Message)
	go func() {
		defer close(out)
		for {
			messageType, data, err := p.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					slog.Info("connection closed by peer", "peerId", p.PeerId)
					break
				}
				slog.Error("peer connection read", "peerId", p.PeerId, "error", err)
				break
			}
			if messageType != websocket.TextMessage {
				slog.Error(
					"received unexpected non-text message",
					"peerId", p.PeerId,
					"messageType", messageType,
					"message", data,
				)
				continue
			}
			decryptedData, err := DecryptMessageData(data, p.key)
			if err != nil {
				slog.Error("message data decrypt", "error", err)
				continue
			}
			out <- &Message{
				FromPeerId:   p.PeerId,
				FromPeerName: p.Name,
				FromPeerAddr: p.conn.RemoteAddr().String(),
				ToChatId:     p.ChatId,
				Txt:          string(decryptedData),
			}
		}
	}()
	return out
}
