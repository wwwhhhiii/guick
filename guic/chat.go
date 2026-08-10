package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/google/uuid"
)

var chatsMap = make(map[uuid.UUID]*Chat)
var mu sync.Mutex

type Message struct {
	PeerName string    `json:"peerName"`
	PeerId   uuid.UUID `json:"peerId"`
	ChatId   uuid.UUID `json:"chatId"`
	Text     string    `json:"text"`
}

type Chat struct {
	id         uuid.UUID
	name       string
	mu         sync.RWMutex
	peers      map[uuid.UUID]*Peer
	inMessages chan *Message
	isHosted   bool
}

func NewChat(name string, hosted bool) *Chat {
	return &Chat{
		id:         uuid.New(),
		name:       name,
		peers:      make(map[uuid.UUID]*Peer),
		inMessages: make(chan *Message, 100),
		isHosted:   hosted,
	}
}

func (c *Chat) WritePump(ctx context.Context) {
	defer close(c.inMessages)
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.inMessages:
			data, err := json.Marshal(m)
			if err != nil {
				continue
			}
			for _, p := range c.peers {
				if p.MsgChan != nil && p.Name != m.PeerName {
					if err := p.MsgChan.Send(data); err != nil {
						slog.Error("write pump message send", "error", err)
					}
					slog.Debug("write pump message send", "message", m)
				}
			}
		}
	}
}

func getChat(id uuid.UUID) (*Chat, bool) {
	mu.Lock()
	c, ok := chatsMap[id]
	mu.Unlock()
	return c, ok
}

func addChat(c *Chat) {
	mu.Lock()
	chatsMap[c.id] = c
	mu.Unlock()
}

func rmChat(id uuid.UUID) {
	mu.Lock()
	delete(chatsMap, id)
	mu.Unlock()
}

func (c *Chat) SendMessage(m *Message) {
	c.inMessages <- m
}

func (c *Chat) DisconnectPeer(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, exist := c.peers[id]; exist {
		p.Disconnect()
	}
	delete(c.peers, id)
}

func (c *Chat) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, p := range c.peers {
		p.Disconnect()
		delete(c.peers, k)
	}
}

func (c *Chat) addPeers(peers ...*Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range peers {
		if _, exist := c.peers[p.id]; !exist {
			c.peers[p.id] = p
		}
	}
}

func (c *Chat) rmPeers(peers ...*Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, peer := range peers {
		delete(c.peers, peer.id)
	}
}
