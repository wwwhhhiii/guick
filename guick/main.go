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

// TODOS
//
// - add submit event to connection entry
// - add submit functionality for modal popup

var addr = flag.String("addr", "0.0.0.0", "http server address")
var port = flag.String("port", "8080", "http server port")
var upgrader = websocket.Upgrader{}

var ourPeerId = uuid.New()

// current peer to send messages to
var curPeerId = uuid.Nil

// current peer scroll to show and append sent/recv messages to
var curPeerScroll *container.Scroll = nil

// a slice just for conversion between fyne list id to app peer UUID
var fyneListPeers = []uuid.UUID{}

// peer chat containers to select from when selecting current peer in UI
var peerScrollWindows = make(map[uuid.UUID]*container.Scroll)

type wsServeHandler struct {
	hub    *Hub
	peerId uuid.UUID
}

// serve incoming peers connections. register connected peers in a hub
func (wsh *wsServeHandler) serveWs(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("[serve] upgrade:", err)
	}

	// if we recv client connection,
	// firstly we wait for peer id from the client
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Println("[serve] read peer id err:", err)
		return
	}
	peerId, err := uuid.ParseBytes(msg)
	if err != nil {
		log.Println("[serve] error parsing UUID", err)
		return
	}

	log.Println("[serve] incoming connection from peer:", string(msg))

	if peerId == wsh.peerId {
		log.Println("[serve] tried to connect to self, closing connection...")
		conn.Close()
		return
	}

	// then we respond with our peer id
	err = conn.WriteMessage(websocket.TextMessage, []byte(wsh.peerId.String()))
	if err != nil {
		log.Println("[serve] error sending peer id:", err)
		return
	}

	// TODO here we assuming that we are ok after sending our peer id,
	// but we may be not ok if the client did not recv our peer id, (or some other err occured)
	// so we need some response that he registered us?
	wsh.hub.RegisterClient(NewClient(peerId, conn, TypeServer))
}

// connect to peers. returns peer as client struct
func connect(addr string, ourPeerId uuid.UUID) (*Client, error) {
	url := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	log.Printf("[connect] connecting to %s...", addr)
	conn, _, err := websocket.DefaultDialer.Dial(url.String(), nil)
	if err != nil {
		log.Println("[connect] peer connect err:", err)
		return nil, err
	}

	log.Println("[connect] sending our peer id...")
	err = conn.WriteMessage(websocket.TextMessage, []byte(ourPeerId.String()))
	if err != nil {
		log.Println("[connect] peer id send err:", err)
		return nil, err
	}

	log.Println("[connect] waiting for server peer id...")
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Println("[connect] read server peer id err:", err)
		return nil, err
	}

	serverPeerId, err := uuid.ParseBytes(msg)
	if err != nil {
		log.Printf("[connect] server peer id parse err: %v (%b)", err, msg)
		return nil, err
	}

	if serverPeerId == ourPeerId {
		log.Println("[connect] tried to connect to self, closing...")
		conn.Close()
		return nil, errors.New("self connection")
	}

	return NewClient(serverPeerId, conn, TypeClient), nil
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
	peerIP := net.ParseIP(*addr)
	if peerIP == nil {
		log.Println("invalid listen address:", *addr)
		return
	}
	serverAddr := fmt.Sprintf("%s:%s", *addr, *port)

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
	log.Println("[main] running server on", serverAddr)
	go http.ListenAndServe(serverAddr, nil)

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

	uiOnConnect := func() {
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
	}
	peerEntry.OnSubmitted = func(s string) { uiOnConnect() }
	connEntry := container.NewVBox(
		peerEntry,
		widget.NewButton("Connect", uiOnConnect),
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
		// Here we reassigning inner object of chat, but keep reference to it in peers scroll map
		// because we still want to show it later
		chatBorder.Objects[0] = curPeerScroll
		curPeerScroll.Show()
	}

	// UI reactor
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
