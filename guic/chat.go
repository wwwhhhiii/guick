package main

import (
	"encoding/json"
	"sync"

	"github.com/google/uuid"
)

type Chat struct {
	id       uuid.UUID
	name     string
	mu       sync.RWMutex
	peers    map[uuid.UUID]*Peer
	isHosted bool
}

var chatsMap = make(map[uuid.UUID]*Chat)
var mu sync.Mutex

type Message struct {
	PeerName string    `json:"peerName"`
	PeerId   uuid.UUID `json:"peerId"`
	ChatId   uuid.UUID `json:"chatId"`
	Text     string    `json:"text"`
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

// TODO make trySendMessage and do not return errors
func (c *Chat) sendMessage(m *Message) error {
	// TODO impl message write pump
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	for _, p := range c.peers {
		if p.MsgChan != nil && p.Name != m.PeerName {
			if err := p.MsgChan.Send(data); err != nil {
				return err
			}
		}
	}
	return nil
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
		p.chat = c
	}
}

func (c *Chat) rmPeers(peers ...*Peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, peer := range peers {
		delete(c.peers, peer.id)
	}
}
