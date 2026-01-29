package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type ConnType int

const (
	TypeServer ConnType = iota
	TypeClient
)

const (
	maxMessageSizeBytes = 512

	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
	writeWait  = 10 * time.Second
)

type Peer struct {
	// chat peer belongs to
	ChatId uuid.UUID
	PeerId uuid.UUID
	Name   string
	// peer connection
	conn *websocket.Conn
	// who is this peer: client or a server
	connType ConnType
	cancel   context.CancelFunc
	send     chan *Message
	key      []byte
}

func NewPeer(chatId uuid.UUID, peerId uuid.UUID, name string, conn *websocket.Conn, connT ConnType, key []byte) *Peer {
	return &Peer{
		ChatId:   chatId,
		PeerId:   peerId,
		Name:     name,
		conn:     conn,
		connType: connT,
		key:      key,
		// aesgcm:   aesgcm,
	}
}

// gracefully closes peer connection
func (p *Peer) GracefulDisconnect() error {
	return p.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(
		websocket.CloseNormalClosure, ""),
	)
}

// encrypts and writes message to underlying peer connection
func (p *Peer) sendMessage(m *Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	cipherdata, err := Encrypt(data, p.key)
	if err != nil {
		return err
	}
	p.conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := p.conn.WriteMessage(websocket.BinaryMessage, cipherdata); err != nil {
		return err
	}
	return nil
}

func (p *Peer) startReader(ctx context.Context, onStop func(), readinto chan<- *Message) {
	defer func() {
		p.conn.Close()
	}()
	defer onStop()
	p.conn.SetReadLimit(maxMessageSizeBytes)
	p.conn.SetReadDeadline(time.Now().Add(pongWait))
	p.conn.SetPongHandler(func(appData string) error {
		p.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		mtype, data, err := p.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				p.GracefulDisconnect()
				break
			}
			p.conn.Close()
			break
		}
		if mtype != websocket.BinaryMessage {
			continue
		}
		plaindata, err := Decrypt(data, p.key)
		if err != nil {
			continue
		}
		m := &Message{}
		if err = json.Unmarshal(plaindata, m); err != nil {
			continue
		}
		readinto <- &Message{
			FromPeerId:   p.PeerId,
			FromPeerName: m.FromPeerName,
			FromPeerAddr: p.conn.RemoteAddr().String(),
			ToChatId:     p.ChatId,
			Txt:          m.Txt,
		}
	}
}

func (p *Peer) startWriter(ctx context.Context, onStop func()) {
	p.send = make(chan *Message, 100)
	ticker := time.NewTicker(pingPeriod)
	defer close(p.send)
	defer ticker.Stop()
	defer onStop()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-p.send:
			if err := p.sendMessage(m); err != nil {
				slog.Error("peer write message", "error", err)
				return
			}
		case <-ticker.C:
			err := p.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
			if err != nil {
				slog.Error("write ping message", "error", err)
				return
			}
			slog.Debug("ping", "who", p.conn.RemoteAddr())
		}
	}
}
