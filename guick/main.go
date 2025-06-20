package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

var addr = flag.String("addr", "0.0.0.0:8080", "http server address")
var upgrader = websocket.Upgrader{}

var ourPeerId = uuid.New()

// current peer to send messages to
var curPeerId = uuid.Nil
var curPeerScroll *container.Scroll = nil

// just for conversion from fyne list id to peer UUID
var fyneListPeers = []uuid.UUID{}

// peer chat containers to switch when switching current peer
var peerScrollWindows = make(map[uuid.UUID]*container.Scroll)

type wsServeHandler struct {
	hub    *Hub
	peerId uuid.UUID
}

func (wsh *wsServeHandler) serveWs(w http.ResponseWriter, r *http.Request) {
	// if we are here, it means we got a request to connect to our websocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[serve] upgrade:", err)
	}

	// we receive their peer id
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Println("[serve] read peer id err:", err)
		return
	}
	log.Println("[serve] incoming connection from peer:", string(msg))
	// we send our peer id
	err = conn.WriteMessage(websocket.TextMessage, []byte(wsh.peerId.String()))
	if err != nil {
		log.Println("[serve] error sending peer id:", err)
		return
	}

	peerId, err := uuid.ParseBytes(msg)
	if err != nil {
		log.Println("[serve] error parsing UUID", err)
		return
	}

	// TODO should be NewClient constructor
	client := &Client{PeerId: peerId, conn: conn, connT: TypeServer}
	if client.PeerId == wsh.peerId {
		log.Println("[serve] tried to connect to self, closing...")
		if err := client.Close(); err != nil {
			log.Println("[serve] client close err:", err)
		}
		client.conn.Close()
		return
	}

	wsh.hub.RegisterClient(client)
}

func connect(addr string, ourPeerId uuid.UUID) (*Client, error) {
	url := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	log.Printf("[connect] connecting to %s...", addr)
	conn, _, err := websocket.DefaultDialer.Dial(url.String(), nil)
	if err != nil {
		log.Println("[connect] peer connect err:", err)
		return nil, err
	}

	log.Println("[connect] sending out peer id...")
	err = conn.WriteMessage(websocket.TextMessage, []byte(ourPeerId.String()))
	if err != nil {
		log.Println("[connect] peer id write err:", err)
		return nil, err
	}

	log.Println("[connect] waiting for their peer id...")
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Println("[connect] read peer id err:", err)
		return nil, err
	}

	peerId, err := uuid.ParseBytes(msg)
	if err != nil {
		log.Printf("[connect] error parsing UUID: %v (%b)", err, msg)
		return nil, err
	}
	client := &Client{PeerId: peerId, conn: conn, connT: TypeClient}
	if client.PeerId == ourPeerId {
		log.Println("[connect] tried to connect to self, closing...")
		if err := client.Close(); err != nil {
			log.Println("[connect] client close err:", err)
			return nil, err
		}
		client.conn.Close()
		return nil, errors.New("self connection")
	}

	return client, nil
}

func showModalPopup(txt string, onCanvas fyne.Canvas) {
	var modal *widget.PopUp
	popupContent := container.NewVBox(
		widget.NewLabel(txt),
		widget.NewButton("Close", func() {
			modal.Hide()
		}),
	)
	modal = widget.NewModalPopUp(
		popupContent,
		onCanvas,
	)
	modal.Show()
}

func main() {
	flag.Parse()
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Printf("invalid listen address: %s, expected <host>:<port>", *addr)
		return
	}
	peerIP := net.ParseIP(host)
	if peerIP == nil {
		log.Println("invalid listen address:", host)
		return
	}

	onClientRegistered := make(chan *Client)
	onClientUnregistered := make(chan *Client)
	onRecvMessage := make(chan *Msg)
	onSentMessage := make(chan *Msg)
	hub := newHub(onClientRegistered, onClientUnregistered, onRecvMessage, onSentMessage)
	defer hub.Shutdown()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go hub.Run(interrupt)

	log.Println("[main] our peer id:", ourPeerId)
	wsHandler := &wsServeHandler{hub: hub, peerId: ourPeerId}
	http.HandleFunc("/ws", wsHandler.serveWs)
	log.Println("[main] running server on", *addr)
	go http.ListenAndServe(*addr, nil)

	app := app.New()
	window := app.NewWindow("Guic")
	window.Resize(fyne.NewSize(500, 500))

	peerEntry := widget.NewEntry()
	peerEntry.SetPlaceHolder("Peer IP")

	peerList := widget.NewList(
		func() int { return len(fyneListPeers) },
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(lii widget.ListItemID, co fyne.CanvasObject) {
			peerId := fyneListPeers[lii]
			if peerId == uuid.Nil {
				return
			}
			client, exist := hub.clients[peerId]
			if !exist {
				log.Fatalf("not found client by selected peer id: %s", peerId)
			}
			co.(*widget.Label).SetText(client.conn.RemoteAddr().String())
		},
	)

	connEntry := container.NewVBox(
		peerEntry,
		widget.NewButton("Connect", func() {
			if peerEntry.Text == "" {
				return
			}
			host, _, err := net.SplitHostPort(peerEntry.Text)
			if err != nil {
				errTxt := fmt.Sprintf("invalid peer address: %s", err)
				log.Println(errTxt)
				showModalPopup(errTxt, window.Canvas())
				return
			}
			peerIP := net.ParseIP(host)
			if peerIP == nil {
				errTxt := "incorrect IP address format"
				log.Println(errTxt)
				showModalPopup(errTxt, window.Canvas())
				return
			}
			// TODO also need to check here if already connected
			client, err := connect(peerEntry.Text, ourPeerId)
			if err != nil {
				showModalPopup(fmt.Sprintf("connection error: %s", err), window.Canvas())
				return
			}
			hub.RegisterClient(client)
			peerEntry.SetText("")
			showModalPopup(
				fmt.Sprintf("Client %s connected!", client.conn.RemoteAddr()),
				window.Canvas(),
			)
		}),
	)
	connContainer := container.NewBorder(
		connEntry, nil, nil, nil, peerList,
	)

	textEntry := widget.NewEntry()
	textEntry.SetPlaceHolder("Enter a message")
	sendMessage := func(t string) {
		if t == "" {
			return
		}
		if curPeerId == uuid.Nil {
			showModalPopup("Select peer", window.Canvas())
			return
		}
		hub.sendMessage <- NewMsg(
			t,
			ourPeerId,
			curPeerId,
			hub.clients[curPeerId].conn.LocalAddr().String(),
			hub.clients[curPeerId].conn.RemoteAddr().String(),
		)
		textEntry.SetText("")
	}
	textEntry.OnSubmitted = sendMessage
	textSendEntry := container.NewVBox(
		textEntry,
		widget.NewButton("Send", func() {
			if textEntry.Text == "" {
				return
			}
			sendMessage(textEntry.Text)
		}),
	)
	placeholderScroll := container.NewScroll(widget.NewTextGrid())
	chatBorder := container.NewBorder(
		nil, textSendEntry, nil, nil, placeholderScroll,
	)
	content := container.NewHSplit(connContainer, chatBorder)

	peerList.OnSelected = func(id widget.ListItemID) {
		peerId := fyneListPeers[id]
		if peerId == uuid.Nil {
			return
		}
		curPeerId = peerId
		if _, exist := peerScrollWindows[curPeerId]; !exist {
			peerScrollWindows[curPeerId] = container.NewScroll(widget.NewTextGrid())
		}
		curPeerScroll = peerScrollWindows[curPeerId]
		chatBorder.Objects[0].(*container.Scroll).Hide()
		// Here we reassigning inner object of chat, but not deleting the hidden element,
		// because we keep reference to in in peerScrollWindows map
		chatBorder.Objects[0] = curPeerScroll
		curPeerScroll.Show()
	}

	// UI async reactor
	go func() {
		for {
			select {
			case client := <-onClientRegistered:
				log.Println("[UI reactor] client registered")
				fyneListPeers = append(fyneListPeers, client.PeerId)
				fyne.Do(func() {
					peerList.Refresh()
				})
				peerScrollWindows[client.PeerId] = container.NewScroll(widget.NewTextGrid())
			case client := <-onClientUnregistered:
				log.Println("[UI reactor] client unregistered")
				deleteIdx := -1
				for i, peerId := range fyneListPeers {
					if peerId == client.PeerId {
						deleteIdx = i
						break
					}
				}
				clientFound := deleteIdx != -1
				if clientFound {
					fyneListPeers = append(fyneListPeers[:deleteIdx], fyneListPeers[deleteIdx+1:]...)
				}
				fyne.Do(func() {
					peerList.Refresh()
				})
				delete(peerScrollWindows, client.PeerId)
				// replace with placeholder to delete reference for current peer scroll from UI
				chatBorder.Objects[0] = container.NewScroll(widget.NewTextGrid())
			case msg := <-onRecvMessage:
				log.Println("[UI reactor] message received")
				fyne.Do(func() {
					if scroll, exist := peerScrollWindows[msg.FromPeerId]; exist {
						scroll.Content.(*widget.TextGrid).Append(fmt.Sprintf("[%s]: %s", msg.FromPeerAddr, msg.Txt))
					} else {
						log.Fatalf("[UI reactor] error, no scroll peer found for %s", msg.FromPeerId)
					}
				})
			case msg := <-onSentMessage:
				log.Println("[UI reactor] message sent")
				fyne.Do(func() {
					if scroll, exist := peerScrollWindows[msg.ToPeerId]; exist {
						scroll.Content.(*widget.TextGrid).Append(fmt.Sprintf("[me]: %s", msg.Txt))
					} else {
						log.Fatalf("[UI reactor] error, no scroll peer found for %s", msg.ToPeerAddr)
					}
				})
			}
		}
	}()

	window.SetContent(content)
	window.Show()
	app.Run()
}
