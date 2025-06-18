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
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

var addr = flag.String("addr", "localhost:8080", "http server address")
var upgrader = websocket.Upgrader{}
var curPeerId = uuid.Nil

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

	client := &Client{PeerId: peerId, conn: conn, connT: TypeServer}
	if client.PeerId == wsh.peerId {
		log.Println("[serve] tried to connect to self, closing...")
		if err := client.Close(); err != nil {
			log.Println("[serve] client close err:", err)
		}
		client.conn.Close()
		return
	}

	wsh.hub.register <- client
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

	ourPeerId := uuid.New()
	log.Println("[main] our peer id:", ourPeerId)
	hub := newHub()
	defer hub.Shutdown()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	go hub.Run(interrupt)

	wsHandler := &wsServeHandler{hub: hub, peerId: ourPeerId}
	http.HandleFunc("/ws", wsHandler.serveWs)
	log.Println("[main] running server on", *addr)
	go http.ListenAndServe(*addr, nil)

	app := app.New()
	window := app.NewWindow("Guic")
	window.Resize(fyne.NewSize(500, 500))

	peerEntry := widget.NewEntry()
	peerEntry.SetPlaceHolder("Peer IP")

	// just for conversion from fyne list id to peer UUID
	fynePeers := []uuid.UUID{}
	peerList := widget.NewList(
		func() int { return len(fynePeers) },
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(lii widget.ListItemID, co fyne.CanvasObject) {
			peerId := fynePeers[lii]
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
	peerList.OnSelected = func(id widget.ListItemID) {
		peerId := fynePeers[id]
		if peerId == uuid.Nil {
			return
		}
		curPeerId = peerId
		log.Println("current peer id", curPeerId)
	}

	connContainer := container.New(
		layout.NewVBoxLayout(),
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
			client, err := connect(peerEntry.Text, ourPeerId)
			if err != nil {
				showModalPopup(fmt.Sprintf("connection error: %s", err), window.Canvas())
				return
			}
			hub.register <- client
			peerEntry.SetText("")
			fynePeers = append(fynePeers, client.PeerId)
			peerList.Refresh()
			showModalPopup(
				fmt.Sprintf("Client %s connected!", client.conn.RemoteAddr()),
				window.Canvas(),
			)
		}),
		peerList,
	)

	textEntry := widget.NewEntry()
	textEntry.SetPlaceHolder("Enter a message")
	comContainer := container.New(
		layout.NewVBoxLayout(),
		textEntry,
		widget.NewButton("Send", func() {
			if textEntry.Text == "" {
				return
			}
			hub.sendMessage <- &Msg{fromPeerId: ourPeerId, txt: textEntry.Text, toPeerId: curPeerId}
			textEntry.SetText("")
		}),
	)
	content := container.NewBorder(
		nil, nil, connContainer, nil, comContainer,
	)

	window.SetContent(content)
	window.Show()
	app.Run()
}
