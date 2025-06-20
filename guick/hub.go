package main

import (
	"errors"
	"log"
	"os"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// TODO s:
//
// - buffered channel for reading and writing messages
// - impl ping-pong and read timeout

// const (
// 	maxMessageSize = 512

// 	pongWait = 30 * time.Second
// )

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

	// Sends clients that were registered in hub.
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
		register:      make(chan *Client),
		unregister:    make(chan *Client),
		sendMessage:   make(chan *Msg),
		recvMessage:   make(chan *Msg),
		OnClientReg:   onClientReg,
		OnClientUnreg: onClientUnreg,
		OnMsgRecv:     onMsgRecv,
		OnMsgSent:     onMsgSent,
	}
}

func (hub *Hub) registerClient(c *Client) error {
	if _, exist := hub.clients[c.PeerId]; exist {
		return errors.New("peer already registered")
	}
	hub.clients[c.PeerId] = c

	return nil
}

func (hub *Hub) RegisterClient(client *Client) {
	hub.register <- client
}

func (hub *Hub) unregisterClient(c *Client) error {
	client, exist := hub.clients[c.PeerId]
	if !exist {
		return errors.New("peer is not registered")
	}
	if client.connT == TypeServer {
		if err := client.Close(); err != nil {
			client.conn.Close()
		}
	}
	client.conn.Close()
	delete(hub.clients, client.PeerId)

	go hub.emitClientUnreg(client)

	return nil
}

func (hub *Hub) UnregisterClient(client *Client) {
	hub.unregister <- client
}

func (hub *Hub) emitClientReg(client *Client) {
	hub.OnClientReg <- client
}

func (hub *Hub) emitClientUnreg(client *Client) {
	hub.OnClientUnreg <- client
}

func (hub *Hub) emitRecvMsg(msg *Msg) {
	hub.OnMsgRecv <- msg
}

func (hub *Hub) emitSentMsg(msg *Msg) {
	hub.OnMsgSent <- msg
}

func (hub *Hub) Run(interrupt <-chan os.Signal) {
	defer close(hub.register)
	defer close(hub.unregister)
	defer close(hub.sendMessage)
	defer close(hub.recvMessage)

	workerDoneOk := make(chan *Client)
	workerDoneErr := make(chan *Client)

	for {
		select {
		case <-interrupt:
			log.Println("[hub] interrupted")
			hub.Shutdown()
			log.Println("[hub] shutdown")
			return
		case client := <-workerDoneErr:
			go hub.unregisterClient(client)
			// TODO start some reconnect goroutine?
		case client := <-workerDoneOk:
			go hub.unregisterClient(client)
		case client := <-hub.register:
			if err := hub.registerClient(client); err != nil {
				log.Printf("[hub] client %s register err: %s", client.PeerId, err)
				continue
			}
			log.Println("[hub] registered client:", client.PeerId)
			go hub.emitClientReg(client)
			go client.readMessages(
				workerDoneOk,
				workerDoneErr,
				hub.recvMessage,
			)
		case client := <-hub.unregister:
			if err := hub.unregisterClient(client); err != nil {
				log.Printf("[hub] client %s unregister err:", err)
				continue
			}
			log.Println("[hub] unregistered client:", client.PeerId)
		case msg := <-hub.sendMessage:
			if client, exist := hub.clients[msg.ToPeerId]; exist {
				if err := client.conn.WriteMessage(websocket.TextMessage, []byte(msg.Txt)); err != nil {
					log.Println("[hub] message write err:", err)
					continue
				}
				go hub.emitSentMsg(msg)
			} else {
				log.Println("[hub] peer unknown:", msg.ToPeerId)
			}
		case msg := <-hub.recvMessage:
			log.Printf("[%s]: %s", msg.FromPeerId, msg.Txt)
			go hub.emitRecvMsg(msg)
		}
	}
}

func (hub *Hub) Shutdown() {
	delKeys := make([]uuid.UUID, len(hub.clients))
	for _, client := range hub.clients {
		client.Close()
		delKeys = append(delKeys, client.PeerId)
	}
	for _, key := range delKeys {
		delete(hub.clients, key)
	}
}
