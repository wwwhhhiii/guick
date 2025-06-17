package main

import (
	"flag"
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

	// TODO remove
	curPeerId = peerId

	wsh.hub.register <- &Client{PeerId: peerId, conn: conn, connT: TypeServer}
}

func connect(addr string, ourPeerId uuid.UUID) (*Client, error) {
	url := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	conn, _, err := websocket.DefaultDialer.Dial(url.String(), nil)
	if err != nil {
		log.Println("[connect] peer connect err:", err)
		return nil, err
	}

	err = conn.WriteMessage(websocket.TextMessage, []byte(ourPeerId.String()))
	if err != nil {
		log.Println("[connect] peer id write err:", err)
		return nil, err
	}

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

	// TODO remove
	curPeerId = peerId

	return &Client{PeerId: peerId, conn: conn, connT: TypeClient}, nil
}

func reconnect(oldConn *websocket.Conn) (*websocket.Conn, error) {

	return nil, nil
}

func main() {
	flag.Parse()

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

	clientEntry := widget.NewEntry()
	clientEntry.SetPlaceHolder("Peer IP")

	connContainer := container.New(
		layout.NewVBoxLayout(),
		clientEntry,
		widget.NewButton("Connect", func() {
			if clientEntry.Text == "" {
				return
			}
			host, _, err := net.SplitHostPort(clientEntry.Text)
			if err != nil {
				log.Println("invalid peer address:", clientEntry.Text)
				return
			}
			peerIP := net.ParseIP(host)
			if peerIP == nil {
				log.Println("invalid peer address:", clientEntry.Text)
				return
			}
			req, err := connect(clientEntry.Text, ourPeerId)
			if err != nil {
				// TODO do some error pop-up
				return
			}
			hub.register <- req
			clientEntry.SetText("")
		}),
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
