package main

import (
	"errors"
	"log"
	"log/slog"
	"os"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// TODO s:
//
// - buffered channel for reading and writing messages
// - impl ping-pong and read timeout

type Msg struct {
	FromPeerId   uuid.UUID
	ToPeerId     uuid.UUID
	FromPeerAddr string
	ToPeerAddr   string
	Txt          string
}

func NewMsg(
	txt string,
	fromPeerId uuid.UUID,
	toPeerId uuid.UUID,
	fromPeerAddr string,
	toPeerAddr string,
) *Msg {
	return &Msg{
		Txt:          txt,
		FromPeerId:   fromPeerId,
		ToPeerId:     toPeerId,
		FromPeerAddr: fromPeerAddr,
		ToPeerAddr:   toPeerAddr,
	}
}

type Hub struct {
	// mapping of peer id to its client representation
	clients map[uuid.UUID]*Client

	// peer to register in a hub
	register chan *Client

	// peer id to unregister and close connection to it
	unregister chan *Client

	// coming from client
	sendMessage chan *Msg

	// coming from goroutine listening to peer messages
	recvMessage chan *Msg

	// Clients successfuly registered in hub are sent here.
	// Use this to receive registred clients events
	OnClientReg chan<- *Client

	// Sends clients that were unregistered in hub.
	// Use this to receive registered clients events
	OnClientUnreg chan<- *Client

	// Sends messages that were received from peers.
	// Use this to receive messages from peers
	OnMsgRecv chan<- *Msg

	// Sends messages that were sent to peer.
	// Use this to receive messages that you sent
	OnMsgSent chan<- *Msg
}

func newHub(
	onClientReg chan<- *Client,
	onClientUnreg chan<- *Client,
	onMsgRecv chan<- *Msg,
	onMsgSent chan<- *Msg,
) *Hub {
	return &Hub{
		clients:       make(map[uuid.UUID]*Client, 100),
		register:      make(chan *Client, 100),
		unregister:    make(chan *Client, 100),
		sendMessage:   make(chan *Msg, 100),
		recvMessage:   make(chan *Msg, 100),
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

// adds client record to hub registry
func (hub *Hub) registerClient(c *Client) error {
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
	if err := client.GracefulDisconnect(); err != nil {
		client.conn.Close()
	}
	delete(hub.clients, client.PeerId)
	hub.OnClientUnreg <- client
	return nil
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
				err := client.readMessages(hub.recvMessage)
				if err != nil {
					// TODO start some reconnect goroutine?
				}
				// connection is useless by now, so just disconnect and cleanup client
				if err := hub.disconnectClient(client); err != nil {
					slog.Error(err.Error(), "location", "hub")
				}
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
			// TODO add write deadline
			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				log.Fatalf("writer get err: %s", err)
			}
			w.Write([]byte(msg.Txt))
			if err := w.Close(); err != nil {
				log.Fatalf("writer close err: %s", err)
			}
			hub.OnMsgSent <- msg
		case msg := <-hub.recvMessage:
			hub.OnMsgRecv <- msg
		}
	}
}

func (hub *Hub) Shutdown() {
	delKeys := make([]uuid.UUID, len(hub.clients))
	for _, client := range hub.clients {
		client.GracefulDisconnect()
		delKeys = append(delKeys, client.PeerId)
	}
	for _, key := range delKeys {
		delete(hub.clients, key)
	}
}
