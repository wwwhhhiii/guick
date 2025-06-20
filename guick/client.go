package main

import (
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type ConnType int

const (
	TypeServer ConnType = iota
	TypeClient
)

type Client struct {

	// client peer id
	PeerId uuid.UUID

	// connection with the client
	conn *websocket.Conn

	// Type of connection that was created. Either from server side or client side
	connT ConnType
}

func NewClient(peerId uuid.UUID, conn *websocket.Conn, connT ConnType) *Client {
	return &Client{
		PeerId: peerId,
		conn:   conn,
		connT:  connT,
	}
}

// stops the underlying client worker that works on client websocket,
// closes websocket connection
func (c *Client) Close() error {
	return c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseNormalClosure, ""),
	)
}
