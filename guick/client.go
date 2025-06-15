package main

import (
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Client struct {
	PeerId uuid.UUID
	conn   *websocket.Conn
}

func NewClient(peerId uuid.UUID, conn *websocket.Conn) *Client {
	return &Client{
		PeerId: peerId,
		conn:   conn,
	}
}

// stops the underlying client worker that works on client websocket,
// closes websocket connection
func (c *Client) Close() error {
	return c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseNormalClosure, ""),
	)
}
