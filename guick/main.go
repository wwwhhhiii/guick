package main

import (
	"flag"
	"log"
	"net/http"
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

type wsServeHandler struct {
	hub    *Hub
	peerId uuid.UUID
}

type ReqClient struct {
	peerId uuid.UUID
	conn   *websocket.Conn
}

func (wsh *wsServeHandler) serveWs(w http.ResponseWriter, r *http.Request) {
	// if we are here, it means we got a request to connect to our websocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
	}

	// we receive their peer id
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Println("read peer id err:", err)
		return
	}
	log.Println("read peer id:", string(msg))
	// we send our peer id
	conn.WriteMessage(websocket.TextMessage, []byte(wsh.peerId.String()))

	// might panic here, need validation that msg is uuid
	peerId := uuid.UUID(msg)
	wsh.hub.register <- &ReqClient{peerId: peerId, conn: conn}
}

func main() {
	flag.Parse()

	clientId := uuid.New()
	log.Println("client id:", clientId)
	hub := newHub()
	defer hub.Shutdown()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	go hub.Run(interrupt)

	wsHandler := &wsServeHandler{hub: hub, peerId: clientId}
	http.HandleFunc("/ws", wsHandler.serveWs)
	log.Println("running server on", *addr)
	go http.ListenAndServe(*addr, nil)

	app := app.New()
	window := app.NewWindow("Guick")
	window.Resize(fyne.NewSize(500, 500))

	clientEntry := widget.NewEntry()
	clientEntry.SetPlaceHolder("Your message")

	comWindow := container.New(
		layout.NewVBoxLayout(),
		clientEntry,
		widget.NewButton("Clear", func() {
			if clientEntry.Text == "" {
				return
			}
			log.Print(clientEntry.Text)
			clientEntry.SetText("")
		}),
	)
	content := container.NewBorder(
		nil, nil, nil, nil, comWindow,
	)

	go func() {
		for msg := range hub.recvMessage {
			fyne.DoAndWait(func() { clientEntry.SetText(msg.txt) })
		}
	}()

	window.SetContent(content)
	window.Show()
	app.Run()
}
