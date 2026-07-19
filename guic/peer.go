package main

import (
	"fmt"
	"log"
	"log/slog"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
)

type PeerState int

const (
	StatePending PeerState = iota
	StateConnected
	StateReady
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
}

func SetupOfferor(
	c *webrtc.PeerConnection,
	name string,
	r chan<- struct {
		string
		*Peer
	}) (*Peer, error) {
	p := &Peer{
		id:    uuid.New(),
		state: StatePending,
		conn:  c,
	}
	var err error
	p.CtrlChan, err = c.CreateDataChannel("ctrl", nil)
	if err != nil {
		return nil, err
	}
	p.MsgChan, err = c.CreateDataChannel("msg", nil)
	if err != nil {
		return nil, err
	}
	p.ImgChan, err = c.CreateDataChannel("img", nil)
	if err != nil {
		return nil, err
	}

	p.CtrlChan.OnOpen(func() {
		slog.Debug("channel opened", "name", p.CtrlChan.Label())
		if err := p.CtrlChan.SendText(fmt.Sprintf("0 %s", name)); err != nil {
			slog.Error("error sending name")
		}
	})
	p.CtrlChan.OnClose(func() {
		slog.Debug("channel closed", "name", p.CtrlChan.Label())
	})
	p.CtrlChan.OnMessage(func(m webrtc.DataChannelMessage) {
		slog.Debug("channel msg recieved", "name", p.CtrlChan.Label(), "text", string(m.Data))
		if string(m.Data)[0] == '0' {
			slog.Debug("recv peer name", "name", string(m.Data))
			p.Name = string(m.Data)[2:]
		}
		if p.MsgChan != nil && p.ImgChan != nil {
			p.state = StateReady
		}
	})

	p.MsgChan.OnOpen(func() {
		slog.Debug("channel opened", "name", p.MsgChan.Label())
	})
	p.MsgChan.OnClose(func() {
		slog.Debug("channel closed", "name", p.MsgChan.Label())
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
	})
	p.ImgChan.OnClose(func() {
		slog.Debug("channel closed", "name", p.ImgChan.Label())
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
	p := &Peer{
		id:    uuid.New(),
		state: StatePending,
		conn:  c,
	}
	c.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "ctrl":
			p.CtrlChan = dc
			p.CtrlChan.OnOpen(func() {
				slog.Debug("ctrl dc opened")
				if err := p.CtrlChan.SendText(fmt.Sprintf("0 %s", name)); err != nil {
					slog.Error("error sending name")
				}
			})
			p.CtrlChan.OnMessage(func(m webrtc.DataChannelMessage) {
				slog.Debug("channel msg recieved", "name", p.CtrlChan.Label(), "text", string(m.Data))
				if string(m.Data)[0] == '0' {
					slog.Debug("recv peer name", "name", string(m.Data))
					p.Name = string(m.Data)[2:]
				}
				if p.MsgChan != nil && p.ImgChan != nil {
					p.state = StateReady
				}
			})
		case "msg":
			p.MsgChan = dc
			p.MsgChan.OnMessage(func(m webrtc.DataChannelMessage) {
				slog.Debug("channel msg received", "name", p.MsgChan.Label(), "data", m.Data)
				r <- struct {
					string
					*Peer
				}{string(m.Data), p}
			})
		case "img":
			p.ImgChan = dc
		default:
			log.Fatalln("unknown channel label")
		}
		if p.CtrlChan != nil && p.MsgChan != nil && p.ImgChan != nil && p.Name != "" {
			p.state = StateReady
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
