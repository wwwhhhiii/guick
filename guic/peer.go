package main

import (
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

	ready chan struct{}
}

func NewPeer(c *webrtc.PeerConnection, chat *Chat) *Peer {
	return &Peer{
		id:    uuid.New(),
		state: StatePending,
		conn:  c,
		chat:  chat,
		ready: make(chan struct{}),
	}
}

func (p *Peer) addFlag(f PeerStateFlag) PeerState {
	p.state |= f
	if p.state&StateReady == StateReady {
		close(p.ready)
	}
	return p.state
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
	return p.ready
}
