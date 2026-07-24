package main

import (
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
	StatePending   PeerState = 1
	StateConnected PeerState = 1 | connectedFlag
	StateReady     PeerState = 1 | connectedFlag | hasCtrlFlag | hasMsgFlag | hasImgFlag | hasNameFlag
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

func SetupOfferor(
	c *webrtc.PeerConnection,
	name string,
	chatName string,
	r chan<- struct {
		string
		*Peer
	}) (*Peer, error) {
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
		p.state |= hasCtrlFlag
		if err := p.CtrlChan.SendText(fmt.Sprintf("0 %s", name)); err != nil {
			slog.Error("error sending name")
		}
		if err := p.CtrlChan.SendText(fmt.Sprintf("1 %s", chatName)); err != nil {
			slog.Error("error sending chat name")
		}
	})
	p.CtrlChan.OnClose(func() {
		p.state &= ^hasCtrlFlag
		slog.Debug("channel closed", "name", p.CtrlChan.Label())
	})
	p.CtrlChan.OnMessage(func(m webrtc.DataChannelMessage) {
		slog.Debug("channel msg recieved", "name", p.CtrlChan.Label(), "text", string(m.Data))
		switch string(m.Data)[0] {
		case '0':
			slog.Debug("recv peer name", "name", string(m.Data))
			p.Name = string(m.Data)[2:]
			p.state |= hasNameFlag
		default:
			//
		}
		p.ifReady()
	})

	p.MsgChan.OnOpen(func() {
		slog.Debug("channel opened", "name", p.MsgChan.Label())
		p.state |= hasMsgFlag
	})
	p.MsgChan.OnClose(func() {
		slog.Debug("channel closed", "name", p.MsgChan.Label())
		p.state &= ^hasMsgFlag
	})
	p.MsgChan.OnMessage(func(m webrtc.DataChannelMessage) {
		slog.Debug("channel msg recieved", "name", p.MsgChan.Label(), "text", string(m.Data))
		r <- struct {
			string
			*Peer
		}{string(m.Data), p}
	})

	p.ImgChan.OnOpen(func() {
		slog.Debug("channel opened", "name", p.ImgChan.Label())
		p.state |= hasImgFlag
	})
	p.ImgChan.OnClose(func() {
		slog.Debug("channel closed", "name", p.ImgChan.Label())
		p.state &= ^hasImgFlag
	})
	p.ImgChan.OnMessage(func(msg webrtc.DataChannelMessage) {
		slog.Debug("channel msg recieved", "name", p.ImgChan.Label(), "data", msg.Data)
	})

	return p, nil
}

func SetupOfferee(
	c *webrtc.PeerConnection,
	name string,
	r chan<- struct {
		string
		*Peer
	}) (*Peer, error) {
	p := NewPeer(c)
	c.OnDataChannel(func(dc *webrtc.DataChannel) {
		slog.Debug("channel opened", "label", dc.Label())
		switch dc.Label() {
		case "ctrl":
			p.CtrlChan = dc
			p.state |= hasCtrlFlag
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
					p.state |= hasNameFlag
				case '1':
					slog.Debug("recv chat name", "name", string(m.Data))
					p.chat.name = string(m.Data)[2:]
				}
				p.ifReady()
			})
		case "msg":
			p.MsgChan = dc
			p.state |= hasMsgFlag
			p.MsgChan.OnMessage(func(m webrtc.DataChannelMessage) {
				slog.Debug("channel msg received", "name", p.MsgChan.Label(), "data", m.Data)
				r <- struct {
					string
					*Peer
				}{string(m.Data), p}
			})
		case "img":
			p.ImgChan = dc
			p.state |= hasImgFlag
		default:
			log.Fatalln("unknown channel label")
		}
		p.ifReady()
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

func (p *Peer) ifReady() {
	if p.state == StateReady {
		for _, ch := range p.readyChans {
			ch <- struct{}{}
			close(ch)
		}
		p.readyChans = make([]chan<- struct{}, 0, 5)
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
