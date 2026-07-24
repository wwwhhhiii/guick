package main

import (
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

func (c *Chat) sendMessage(m string) error {
	// TODO make parallel
	for _, p := range c.peers {
		if p.MsgChan != nil {
			if err := p.MsgChan.SendText(m); err != nil {
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
