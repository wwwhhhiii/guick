package main

import (
	"crypto/cipher"
	"encoding/json"
	"errors"
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
	if err := client.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return err
	}
	return nil
}

func pingConnection(connection *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			err := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
			if err != nil {
				slog.Error("write ping message", "error", err)
				return
			}
			slog.Info("ping", "who", connection.RemoteAddr())
		case <-stop:
			return
		}
	}
}

// continiously reads client messages until error occurs or connection is closed
func (client *Client) readMessages(hubRecv chan<- *Message) error {
	client.conn.SetReadLimit(maxMessageSizeBytes)

	// for server connection type default ping sender is used.
	// for client connection - set pong handler and start ping sender goroutine
	if client.connType == TypeClient {
		// set read deadline for first message
		client.conn.SetReadDeadline(time.Now().Add(pongWait))
		client.conn.SetPongHandler(func(appData string) error {
			client.conn.SetReadDeadline(time.Now().Add(pongWait))
			return nil
		})
		stopPing := make(chan struct{})
		defer func() {
			stopPing <- struct{}{}
			close(stopPing)
		}()
		go pingConnection(client.conn, stopPing)
	}
	for {
		messageType, data, err := client.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				slog.Info("connection closed by peer", "peerId", client.PeerId)
				return nil
			}
			slog.Error("peer connection read", "peerId", client.PeerId, "error", err)
			return errors.New("error reading peer message")
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
		hubRecv <- NewMsg(
			plaintext,
			client.PeerId,
			ourPeerId,
			client.conn.RemoteAddr().String(),
			client.conn.LocalAddr().String(),
		)
	}
}
