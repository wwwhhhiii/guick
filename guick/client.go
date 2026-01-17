package main

import (
	"crypto/cipher"
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

type Client struct {

	// client peer id
	PeerId uuid.UUID

	// connection with the client
	conn *websocket.Conn

	// Type of connection that was created. Either from server side or client side
	connType ConnType

	aesgcm cipher.AEAD
}

func NewClient(peerId uuid.UUID, conn *websocket.Conn, connT ConnType, aesgcm cipher.AEAD) *Client {
	return &Client{
		PeerId:   peerId,
		conn:     conn,
		connType: connT,
		aesgcm:   aesgcm,
	}
}

// gracefully closes client connection
func (client *Client) GracefulDisconnect() error {
	return client.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseNormalClosure, ""),
	)
}

// encrypts and writes message to underlying client connection
func (client *Client) SendMessage(text string) error {
	encryptedMessage, err := EncryptMessage(text, client.aesgcm)
	if err != nil {
		return err
	}
	data, err := json.Marshal(encryptedMessage)
	if err != nil {
		return err
	}
	// TODO: change message type to websocket.BinaryMessage
	if err := client.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return err
	}
	return nil
}

func StartConnPing(connection *websocket.Conn, pingInterval time.Duration, stop <-chan struct{}) {
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

func ConfigureClientConnection(connection *websocket.Conn) {
	connection.SetReadLimit(maxMessageSizeBytes)
	connection.SetReadDeadline(time.Now().Add(pongWait))
	connection.SetPongHandler(func(appData string) error {
		connection.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
}

func (client *Client) ReadMessagesGen() <-chan *Message {
	out := make(chan *Message)
	go func() {
		defer close(out)
		for {
			messageType, data, err := client.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					slog.Info("connection closed by peer", "peerId", client.PeerId)
					break
				}
				slog.Error("peer connection read", "peerId", client.PeerId, "error", err)
				break
			}
			if messageType != websocket.TextMessage {
				slog.Error(
					"received unexpected non-text message",
					"peerId", client.PeerId,
					"messageType", messageType,
					"message", data,
				)
				continue
			}
			plaintext, err := DecryptMessageData(data, client.aesgcm)
			if err != nil {
				slog.Error("message data decrypt", "error", err)
				continue
			}
			out <- NewMsg(
				plaintext,
				client.PeerId,
				ourPeerId,
				client.conn.RemoteAddr().String(),
				client.conn.LocalAddr().String(),
			)
		}
	}()
	return out
}
