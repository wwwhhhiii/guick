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

// gracefully closes client websocket connection
func (c *Client) GracefulDisconnect() error {
	return c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseNormalClosure, ""),
	)
}

// continiously reads client messages until error occurs or connection is closed
func (c *Client) readMessages(hubRecv chan<- *Msg) error {
	c.conn.SetReadLimit(maxMessageSizeBytes)

	// for server connection type default ping sender is used.
	// for client connection - set pong handler and start ping sender goroutine
	if c.connType == TypeClient {
		// set read deadline for first message
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		c.conn.SetPongHandler(func(appData string) error {
			c.conn.SetReadDeadline(time.Now().Add(pongWait))
			return nil
		})
		ticker := time.NewTicker(pingPeriod)
		stopPing := make(chan struct{})
		defer func() {
			ticker.Stop()
			stopPing <- struct{}{}
			close(stopPing)
		}()
		// ping sender
		go func() {
			for {
				select {
				case <-ticker.C:
					err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
					if err != nil {
						slog.Error("write ping message", "error", err)
						return
					}
					slog.Info("ping", "who", c.conn.RemoteAddr())
				case <-stopPing:
					return
				}
			}
		}()
	}
	for {
		messageType, data, err := c.conn.ReadMessage()
		slog.Info("recv message", "type", messageType)
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return nil
			}
			slog.Error("[read] connection unexpected close", "error", err)
			return errors.New("unexpected client connection close")
		}
		if messageType != websocket.TextMessage {
			slog.Error("unexpected non-text message", "message", data)
			continue
		}
		// decrypt message
		message := &EncryptedMessage{}
		if err := json.Unmarshal(data, message); err != nil {
			slog.Error("message unmarshal", "error", err)
			continue
		}
		slog.Warn("recv message", "ciphertext", message.Ciphertext, "nonce", message.Nonce)
		plaintext, err := DecryptMessage(message.Ciphertext, message.Nonce, c.aesgcm)
		if err != nil {
			slog.Error("message decrypt", "error", err)
			continue
		}
		hubRecv <- NewMsg(
			plaintext,
			c.PeerId,
			uuid.Nil, // TODO here should be our uuid
			c.conn.RemoteAddr().String(),
			c.conn.LocalAddr().String(),
		)
	}
}
