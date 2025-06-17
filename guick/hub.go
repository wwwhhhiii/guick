package main

import (
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
	unregister chan uuid.UUID

	// cominf from client
	sendMessage chan *Msg

	// coming from goroutine listening to peer messages
	recvMessage chan *Msg
}

func newHub() *Hub {
	return &Hub{
		clients:     make(map[uuid.UUID]*Client, 100),
		register:    make(chan *Client),
		unregister:  make(chan uuid.UUID),
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
		log.Println("reading...")
		_, txt, err := peer.conn.ReadMessage()
		log.Println("red", txt)
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
			hub.unregister <- client.PeerId
			log.Println("[hub] stopped errored connection:", client.conn)
			// TODO start some reconnect goroutine?
		case client := <-workerDoneOk:
			hub.unregister <- client.PeerId
			log.Println("[hub] stopped done connection:", client.conn)
		case client := <-hub.register:
			if _, exist := hub.clients[client.PeerId]; exist {
				log.Println("[hub] peer already registered:", client.PeerId)
				continue
			}
			hub.clients[client.PeerId] = client
			log.Printf("[hub] registered peer: %s", client.PeerId)
			go readPeerMessages(
				workerDoneOk,
				workerDoneErr,
				hub.recvMessage,
				client,
			)
		case peerId := <-hub.unregister:
			client, exist := hub.clients[peerId]
			if !exist {
				log.Println("[hub] peer is not registered:", peerId)
				continue
			}
			if client.connT == TypeServer {
				if err := client.Close(); err != nil {
					log.Println("[hub] close client connection:", err)
					client.conn.Close()
				}
			}
			delete(hub.clients, peerId)
			log.Println("[hub] unregistered peer:", peerId)
		case msg := <-hub.sendMessage:
			if client, exist := hub.clients[msg.toPeerId]; exist {
				if err := client.conn.WriteMessage(websocket.TextMessage, []byte(msg.txt)); err != nil {
					log.Println("[hub] message write:", err)
				}
				log.Println("message sent:", msg.txt, client.connT)
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
