package main

import (
	"log"

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

// worker function that endlessly read messages from sebsocket connection.
//
// takes doneOk channel to signal that connection was closed normally.
//
// takes doneErr channel to signal that connection was closed unexpectedly.
func (c *Client) readMessages(
	doneOk chan<- *Client,
	doneErr chan<- *Client,
	hubRecv chan<- *Msg,
) {
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
				log.Println("[read] connection closed:", err)
				doneOk <- c
				return
			}
			log.Println("[read] connection unexpected close:", err)
			doneErr <- c
			return
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
