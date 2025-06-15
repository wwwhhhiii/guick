package main

import (
	"log"
	"os"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Msg struct {
	fromPeerId uuid.UUID
	txt        string
}

type Hub struct {
	// mapping of peer id to its client representation
	clients map[uuid.UUID]*Client

	// request to register a peer in a hub
	register chan *ReqClient

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
		register:    make(chan *ReqClient),
		unregister:  make(chan uuid.UUID),
		sendMessage: make(chan *Msg),
		recvMessage: make(chan *Msg),
	}
}

// a blocking function, should be run in a goroutine
func readPeerMessages(
	done chan<- *websocket.Conn,
	hubRecv chan<- *Msg,
	conn *websocket.Conn,
) {
	for {
		_, txt, err := conn.ReadMessage()
		if err != nil {
			// TODO signal error somehow that connection is bad
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("unexpected socket close: %v", err)
			}
			log.Println("unexpected err:", err)
			done <- conn
			return
		}
		hubRecv <- &Msg{txt: string(txt)}
	}
}

func (hub *Hub) Run(interrupt <-chan os.Signal) {
	defer close(hub.register)
	defer close(hub.unregister)
	defer close(hub.sendMessage)
	defer close(hub.recvMessage)

	workerDone := make(chan *websocket.Conn)

	for {
		select {
		case <-interrupt:
			log.Println("hub interrupted")
			hub.Shutdown()
			return
		case doneConn := <-workerDone:
			// just close, without closing message,
			// because worker asks for it when there is an error

			// TODO do need to unregister? or start some reconnect goroutine?
			doneConn.Close()
			log.Println("closed connection")
		case req := <-hub.register:
			if _, exist := hub.clients[req.peerId]; exist {
				log.Printf("peer %s already registered", req.peerId)
				return
			}
			client := NewClient(req.peerId, req.conn)
			hub.clients[req.peerId] = client
			log.Printf("registered new peer: %s", req.peerId)
			go readPeerMessages(
				workerDone,
				hub.recvMessage,
				req.conn,
			)
		case peerId := <-hub.unregister:
			if client, ok := hub.clients[peerId]; ok {
				if err := client.Close(); err != nil {
					log.Printf("error clearing a client entry: %v", err)
				}
				delete(hub.clients, peerId)
				log.Printf("unregistered peer %s", peerId)
			}
		case msg := <-hub.sendMessage:
			if client, ok := hub.clients[msg.fromPeerId]; ok {
				client.conn.WriteMessage(websocket.TextMessage, []byte(msg.txt))
			} else {
				log.Printf("not found peer %s in hub", msg.fromPeerId)
			}
			// case msg := <-hub.recvMessage:
			// 	log.Println("got message from peer:", msg.txt)
			// 	// TODO push message to client
		}
	}
}

func (hub *Hub) Shutdown() {
	delKeys := make([]uuid.UUID, len(hub.clients))
	for _, client := range hub.clients {
		if err := client.Close(); err != nil {
			// its ok to get here, connection might be closed already
			log.Printf("client entry clear: %v", err)
		}
		delKeys = append(delKeys, client.PeerId)
	}
	for _, key := range delKeys {
		delete(hub.clients, key)
	}
}
