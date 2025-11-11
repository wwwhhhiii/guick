package main

import (
	"errors"
	"log/slog"

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

// TODO add work flag mb
// continiously reads client messages until error occurs or connection is closed
func (c *Client) readMessages(hubRecv chan<- *Msg) error {
	// TODO implement later
	// peer.conn.SetReadLimit(maxMessageSize)
	// // set pong deadline for first message,
	// // because handler starts after reading first message
	// peer.conn.SetReadDeadline(time.Now().Add(pongWait))
	// peer.conn.SetPongHandler(func(appData string) error {
	// 	peer.conn.SetReadDeadline(time.Now().Add(pongWait))
	// 	return nil
	// })
	for {
		_, txt, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return nil
			}
			slog.Error("[read] connection unexpected close", "error", err)
			return errors.New("unexpected client connection close")
		}
		hubRecv <- NewMsg(
			string(txt),
			c.PeerId,
			uuid.Nil, // TODO here should be our uuid
			c.conn.RemoteAddr().String(),
			c.conn.LocalAddr().String(),
		)
	}
}
