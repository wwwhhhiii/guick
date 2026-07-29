package main

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

type PeerState = int
type PeerStateFlag = int

const (
	hasCtrlFlag   PeerStateFlag = 2
	connectedFlag PeerStateFlag = 4
	hasMsgFlag    PeerStateFlag = 8
	hasImgFlag    PeerStateFlag = 16
	hasNameFlag   PeerStateFlag = 32
)
const (
	StatePending   PeerState = 0
	StateConnected PeerState = connectedFlag
	StateReady     PeerState = connectedFlag | hasCtrlFlag | hasMsgFlag | hasImgFlag | hasNameFlag
)

type Peer struct {
	id       uuid.UUID
	state    PeerState
	chat     *Chat
	Name     string
	conn     *webrtc.PeerConnection
	CtrlChan *webrtc.DataChannel
	MsgChan  *webrtc.DataChannel
	ImgChan  *webrtc.DataChannel

	readyChans []chan<- struct{}
}

func NewPeer(c *webrtc.PeerConnection) *Peer {
	return &Peer{
		id:         uuid.New(),
		state:      StatePending,
		conn:       c,
		readyChans: make([]chan<- struct{}, 0, 5),
	}
}

func (p *Peer) addFlag(f PeerStateFlag) PeerState {
	p.state |= f
	switch p.state {
	case StateReady:
		//
		for _, ch := range p.readyChans {
			ch <- struct{}{}
			close(ch)
		}
		p.readyChans = make([]chan<- struct{}, 0, 5)
	case StateConnected:
		//
	}
	return p.state
}

func SetupOfferor(
	c *webrtc.PeerConnection,
	name string,
	chatName string,
	onMsgRecv chan<- *Message,
) (*Peer, error) {
	p := NewPeer(c)
	var err error
	if p.CtrlChan, err = c.CreateDataChannel("ctrl", nil); err != nil {
		return nil, err
	}
	if p.MsgChan, err = c.CreateDataChannel("msg", nil); err != nil {
		return nil, err
	}
	if p.ImgChan, err = c.CreateDataChannel("img", nil); err != nil {
		return nil, err
	}

	p.CtrlChan.OnOpen(func() {
		slog.Debug("channel opened", "name", p.CtrlChan.Label())
		p.addFlag(hasCtrlFlag)
		if err := p.CtrlChan.SendText(fmt.Sprintf("0 %s", name)); err != nil {
			slog.Error("error sending name")
		}
		if err := p.CtrlChan.SendText(fmt.Sprintf("1 %s", chatName)); err != nil {
			slog.Error("error sending chat name")
		}
	})
	p.CtrlChan.OnClose(func() {
		slog.Debug("channel closed", "name", p.CtrlChan.Label())
	})
	p.CtrlChan.OnMessage(func(m webrtc.DataChannelMessage) {
		slog.Debug("channel msg recieved", "name", p.CtrlChan.Label(), "text", string(m.Data), "name", p.Name)
		switch string(m.Data)[0] {
		case '0':
			slog.Debug("recv peer name", "name", string(m.Data))
			p.Name = string(m.Data)[2:]
			p.addFlag(hasNameFlag)
		default:
			//
		}
	})

	p.MsgChan.OnOpen(func() {
		slog.Debug("channel opened", "name", p.MsgChan.Label())
		p.addFlag(hasMsgFlag)
	})
	p.MsgChan.OnClose(func() {
		slog.Debug("channel closed", "name", p.MsgChan.Label())
	})
	p.MsgChan.OnMessage(func(m webrtc.DataChannelMessage) {
		slog.Debug("channel msg recieved", "name", p.MsgChan.Label(), "text", string(m.Data), "peer", p.Name)
		msg := &Message{}
		if err := json.Unmarshal(m.Data, msg); err != nil {
			slog.Error("message read", "error", err)
			return
		}
		msg.ChatId = p.chat.id
		onMsgRecv <- msg
		if p.chat.isHosted {
			p.chat.sendMessage(msg)
		}
	})

	p.ImgChan.OnOpen(func() {
		slog.Debug("channel opened", "name", p.ImgChan.Label())
		p.addFlag(hasImgFlag)
	})
	p.ImgChan.OnClose(func() {
		slog.Debug("channel closed", "name", p.ImgChan.Label())
	})
	p.ImgChan.OnMessage(func(msg webrtc.DataChannelMessage) {
		slog.Debug("channel msg recieved", "name", p.ImgChan.Label(), "data", msg.Data, "peer", p.Name)
	})

	return p, nil
}

func SetupOfferee(
	c *webrtc.PeerConnection,
	name string,
	r chan<- *Message,
) (*Peer, error) {
	p := NewPeer(c)
	c.OnDataChannel(func(dc *webrtc.DataChannel) {
		slog.Debug("channel opened", "label", dc.Label())
		switch dc.Label() {
		case "ctrl":
			p.CtrlChan = dc
			p.addFlag(hasCtrlFlag)
			p.CtrlChan.OnOpen(func() {
				if err := p.CtrlChan.SendText(fmt.Sprintf("0 %s", name)); err != nil {
					slog.Error("error sending name")
				}
			})
			p.CtrlChan.OnMessage(func(m webrtc.DataChannelMessage) {
				slog.Debug("channel msg recieved", "name", p.CtrlChan.Label(), "text", string(m.Data))
				switch string(m.Data)[0] {
				case '0':
					slog.Debug("recv peer name", "name", string(m.Data))
					p.Name = string(m.Data)[2:]
					p.addFlag(hasNameFlag)
				case '1':
					slog.Debug("recv chat name", "name", string(m.Data))
					p.chat.name = string(m.Data)[2:]
				}
			})
		case "msg":
			p.MsgChan = dc
			p.addFlag(hasMsgFlag)
			p.MsgChan.OnMessage(func(m webrtc.DataChannelMessage) {
				slog.Debug("channel msg received", "name", p.MsgChan.Label(), "data", m.Data)
				msg := &Message{}
				if err := json.Unmarshal(m.Data, msg); err != nil {
					slog.Error("message read", "error", err)
					return
				}
				// TODO chat id should be separate from message
				msg.ChatId = p.chat.id
				r <- msg
			})
		case "img":
			p.ImgChan = dc
			p.addFlag(hasImgFlag)
		default:
			log.Fatalln("unknown channel label")
		}
	})
	return p, nil
}

func (p *Peer) Disconnect() {
	if p.CtrlChan != nil {
		p.CtrlChan.Close()
	}
	if p.MsgChan != nil {
		p.MsgChan.Close()
	}
	if p.ImgChan != nil {
		p.ImgChan.Close()
	}
	if p.conn != nil {
		p.conn.Close()
	}
}

func (p *Peer) Ready() <-chan struct{} {
	r := make(chan struct{}, 1)
	if p.state == StateReady {
		r <- struct{}{}
		close(r)
	} else {
		p.readyChans = append(p.readyChans, r)
	}
	return r
}
