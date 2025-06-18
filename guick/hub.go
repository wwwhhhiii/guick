package main

import (
	"errors"
	"log"
	"os"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Msg struct {
	fromPeerId uuid.UUID
	toPeerId   uuid.UUID
	txt        string
}

type Hub struct {
	// mapping of peer id to its client representation
	clients map[uuid.UUID]*Client

	// peer to register in a hub
	register chan *Client

	// peer id to unregister and close connection to it
	unregister chan *Client

	// cominf from client
	sendMessage chan *Msg

	// coming from goroutine listening to peer messages
	recvMessage chan *Msg
}

func newHub() *Hub {
	return &Hub{
		clients:     make(map[uuid.UUID]*Client, 100),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		sendMessage: make(chan *Msg),
		recvMessage: make(chan *Msg),
	}
}

// worker function that endlessly read messages from sebsocket connection.
//
// takes doneOk channel to signal that connection was closed normally.
//
// takes doneErr channel to signal that connection was closed unexpectedly.
func readPeerMessages(
	doneOk chan<- *Client,
	doneErr chan<- *Client,
	hubRecv chan<- *Msg,
	peer *Client,
) {
	for {
		_, txt, err := peer.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				log.Println("[read] connection closed:", err)
				doneOk <- peer
				return
			}
			log.Println("[read] connection unexpected close:", err)
			doneErr <- peer
			return
		}
		hubRecv <- &Msg{fromPeerId: peer.PeerId, txt: string(txt)}
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

	return nil
}

func (hub *Hub) UnregisterClient(client *Client) {
	hub.unregister <- client
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
			hub.unregisterClient(client)
			log.Println("[hub] unregistered client:", client.PeerId)
			// TODO start some reconnect goroutine?
		case client := <-workerDoneOk:
			hub.unregisterClient(client)
			log.Println("[hub] unregistered client:", client.PeerId)
		case client := <-hub.register:
			if err := hub.registerClient(client); err != nil {
				log.Printf("[hub] client %s register err: %s", client.PeerId, err)
				continue
			}
			log.Println("[hub] registered client:", client.PeerId)
			go readPeerMessages(
				workerDoneOk,
				workerDoneErr,
				hub.recvMessage,
				client,
			)
		case client := <-hub.unregister:
			if err := hub.unregisterClient(client); err != nil {
				log.Printf("[hub] client %s unregister err:", err)
				continue
			}
			log.Println("[hub] unregistered client:", client.PeerId)
		case msg := <-hub.sendMessage:
			if client, exist := hub.clients[msg.toPeerId]; exist {
				if err := client.conn.WriteMessage(websocket.TextMessage, []byte(msg.txt)); err != nil {
					log.Println("[hub] message write err:", err)
				}
			} else {
				log.Println("[hub] peer unknown:", msg.toPeerId)
			}
		case msg := <-hub.recvMessage:
			log.Printf("[%s]: %s", msg.fromPeerId, msg.txt)
			// 	// TODO push message to client
		}
	}
}

func (hub *Hub) Shutdown() {
	delKeys := make([]uuid.UUID, len(hub.clients))
	for _, client := range hub.clients {
		if err := client.Close(); err != nil {
			// its ok to get here, connection might be closed already
			log.Printf("[hub] client entry clear: %v", err)
		}
		delKeys = append(delKeys, client.PeerId)
	}
	for _, key := range delKeys {
		delete(hub.clients, key)
	}
}
