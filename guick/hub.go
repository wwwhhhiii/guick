package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Chat struct {
	mu       sync.RWMutex
	id       uuid.UUID
	peers    map[uuid.UUID]*Peer
	isHosted bool
}

func NewChat(id uuid.UUID, isHosted bool) *Chat {
	return &Chat{
		id:       id,
		peers:    make(map[uuid.UUID]*Peer, 100),
		isHosted: isHosted,
	}
}

func (c *Chat) addPeers(peers ...*Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range peers {
		if _, exist := c.peers[p.PeerId]; !exist {
			c.peers[p.PeerId] = p
		}
	}
}

func (c *Chat) rmPeers(peers ...*Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, peer := range peers {
		delete(c.peers, peer.PeerId)
	}
}

func (c *Chat) sendMessage(m *Message) error {
	if m.ToChatId != c.id {
		return errors.New("message mismatched chat id")
	}
	for id, peer := range c.peers {
		if id == m.FromPeerId {
			continue
		}
		peer.send <- m
	}
	return nil
}

type Hub struct {
	mu sync.RWMutex
	// registry of chats with peers
	chats map[uuid.UUID]*Chat
	// peer to register in a hub
	register chan *Peer
	// peer id to unregister and close connection to it
	unregister chan *Peer
	removeChat chan *Chat
	// coming from peer
	sendMessage chan *Message
	// coming from goroutine listening to peer messages
	recvMessage chan *Message
	// Peers successfuly registered in hub are sent here.
	// Use this to receive registred peers events
	PeerRegistered chan<- *Peer
	// Sends peers that were unregistered in hub.
	// Use this to receive registered peers events
	PeerUnregistered chan<- *Peer
	// Sends messages that were received from peers.
	// Use this to receive messages from peers
	MessageReceived chan<- *Message
	// Sends messages that were sent to peer.
	// Use this to receive messages that you sent
	MessageSent chan<- *Message
}

func newHub(
	onClientReg chan<- *Peer,
	onClientUnreg chan<- *Peer,
	onMsgRecv chan<- *Message,
	onMsgSent chan<- *Message,
) *Hub {
	return &Hub{
		chats:            make(map[uuid.UUID]*Chat, 100),
		register:         make(chan *Peer),
		unregister:       make(chan *Peer),
		removeChat:       make(chan *Chat),
		sendMessage:      make(chan *Message),
		recvMessage:      make(chan *Message),
		PeerRegistered:   onClientReg,
		PeerUnregistered: onClientUnreg,
		MessageReceived:  onMsgRecv,
		MessageSent:      onMsgSent,
	}
}

func (hub *Hub) LockedPeekChat(id uuid.UUID) (*Chat, bool) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	chat, exist := hub.chats[id]
	return chat, exist
}

// use this to add new peers to hub.
// peer should be connected
func (hub *Hub) RegisterPeer(p *Peer) {
	hub.register <- p
}

// use this to delete peer from hub.
// wil disconnect peer and do all necessary cleanups
func (hub *Hub) UnregisterClient(p *Peer) {
	hub.unregister <- p
}

func (hub *Hub) getOrCreateChat(id uuid.UUID, isHosted bool) *Chat {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	chat, exist := hub.chats[id]
	if !exist {
		chat = NewChat(id, isHosted)
		hub.chats[chat.id] = chat
	}
	return chat
}

// closes connection with the peer.
// deletes peer record from a chat.
// emits peer unregistered event.
func (hub *Hub) gracefulDisconnectPeer(p *Peer) error {
	if err := p.GracefulDisconnect(); err != nil {
		p.conn.Close()
	}
	return nil
}

func (hub *Hub) closePeerConnection(p *Peer) {
	p.conn.Close()
}

func (hub *Hub) Run(interrupt <-chan os.Signal) {
	for {
		select {
		case <-interrupt:
			slog.Info("received interrupt signal")
			hub.Shutdown()
			return
		case peer := <-hub.register:
			chatIsHosted := peer.connType == TypeServer
			chat := hub.getOrCreateChat(peer.ChatId, chatIsHosted)
			chat.addPeers(peer)
			slog.Info("peer added to chat", "chatId", chat.id, "peerId", peer.PeerId)
			peer.conn.SetReadLimit(maxMessageSizeBytes)
			peer.conn.SetReadDeadline(time.Now().Add(pongWait))
			peer.conn.SetPongHandler(func(appData string) error {
				peer.conn.SetReadDeadline(time.Now().Add(pongWait))
				return nil
			})
			peerCtx, cancel := context.WithCancel(context.Background())
			peer.cancel = cancel
			go peer.startReader(peerCtx, hub.recvMessage)
			go peer.startWriter(peerCtx)
			hub.PeerRegistered <- peer
		case peer := <-hub.unregister:
			if err := hub.gracefulDisconnectPeer(peer); err != nil {
				slog.Error("peer disconnect", "error", err)
			}
			chat, exist := hub.LockedPeekChat(peer.ChatId)
			if exist {
				chat.rmPeers(peer)
				peer.cancel()
			}
			hub.PeerUnregistered <- peer
			slog.Info("peer removed from chat", "chatId", peer.ChatId, "peerId", peer.PeerId)
		case chat := <-hub.removeChat:
			chat, exist := hub.chats[chat.id]
			if !exist {
				return
			}
			for _, peer := range chat.peers {
				peer.cancel()
				hub.gracefulDisconnectPeer(peer)
				// TODO collect peers first, call otside of for loop
				chat.rmPeers(peer)
			}
		case msg := <-hub.sendMessage:
			chat, exist := hub.LockedPeekChat(msg.ToChatId)
			if !exist {
				log.Fatal("send message", "unknown chat id", msg.ToChatId)
			}
			// TODO probably need buffered channel with goroutine to write messages
			if err := chat.sendMessage(msg); err != nil {
				slog.Error("send message", "error", err)
				continue
			}
			hub.MessageSent <- msg
		case msg := <-hub.recvMessage:
			chat, exist := hub.chats[msg.ToChatId]
			if !exist {
				log.Fatal("broadcast message unknown chat id ", msg.ToChatId)
			}
			if chat.isHosted {
				if err := chat.sendMessage(msg); err != nil {
					slog.Error("broadcast message", "error", err)
					continue
				}
			}
			hub.MessageReceived <- msg
		}
	}
}

func (hub *Hub) Shutdown() {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, chat := range hub.chats {
		for _, peer := range chat.peers {
			hub.gracefulDisconnectPeer(peer)
		}
	}
}
