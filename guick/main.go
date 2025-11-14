package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/hkdf"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// TODOS
//
// - add submit functionality for modal popup

var addr = flag.String("addr", "0.0.0.0", "http server address")
var port = flag.String("port", "8080", "http server port")
var upgrader = websocket.Upgrader{}

var ourPeerId = uuid.New()

// current peer to send messages to
var selectedPeerId = uuid.Nil

// current peer scroll to show and append sent/recv messages to
var curPeerScroll *container.Scroll = nil

// a slice just for conversion between fyne list id to app peer UUID
var fyneListPeers = []uuid.UUID{}

// peer chat containers to select from when selecting current peer in UI
var peerScrollWindows = make(map[uuid.UUID]*container.Scroll)

type wsServeHandler struct {
	hub        *Hub
	privateKey *ecdh.PrivateKey
	peerId     uuid.UUID
}

// serve incoming peers connections. register connected peers in a hub
func (wsh *wsServeHandler) serveWs(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("ws upgrade error", "error", err)
		return
	}

	_, data, err := conn.ReadMessage()
	if err != nil {
		slog.Error("peer public key read", "error", err)
		return
	}
	peerParsedKey, err := x509.ParsePKIXPublicKey(data)
	if err != nil {
		slog.Error("peer public key parse", "error", err)
		// TODO: close connection
		return
	}
	peerPublicKey, ok := peerParsedKey.(*ecdh.PublicKey)
	if !ok {
		slog.Error("peer public key type assertion", "error", err)
		// TODO: close connection
		return
	}

	publicKeyData, err := x509.MarshalPKIXPublicKey(wsh.privateKey.PublicKey())
	if err != nil {
		slog.Error("public key marshal", "error", err)
		// TODO: close connection
		return
	}
	if err = conn.WriteMessage(websocket.TextMessage, publicKeyData); err != nil {
		slog.Error("public key send", "error", err)
	}
	sharedSecret, err := wsh.privateKey.ECDH(peerPublicKey)
	if err != nil {
		slog.Error("shared secret derivation", "error", err)
		return
	}
	hkdf := hkdf.New(sha256.New, sharedSecret, nil, nil)
	encryptionKey := make([]byte, 16) // AES-128
	if _, err := io.ReadFull(hkdf, encryptionKey); err != nil {
		// TODO: close connection
		return
	}
	aesgcm, err := AesGCM(encryptionKey)
	if err != nil {
		slog.Error("aesgcm generation", "error", err)
		// TODO: close connection
		return
	}

	// now exchange user IDs
	_, msg, err := conn.ReadMessage()
	if err != nil {
		slog.Error("peer UUID read error", "error", err)
		return
	}
	peerId, err := uuid.ParseBytes(msg)
	if err != nil {
		slog.Error("UUID parse error", "error", err)
		return
	}

	slog.Info("incoming peer connection", "peer", string(msg))

	if peerId == wsh.peerId {
		slog.Error("tried to connect to self, closing connection...")
		conn.Close()
		return
	}

	// then we respond with our peer id
	err = conn.WriteMessage(websocket.TextMessage, []byte(wsh.peerId.String()))
	if err != nil {
		slog.Error("peer UUID send error", "error", err)
		return
	}

	// TODO here we assuming that we are ok after sending our peer id,
	// but we may be not ok if the client did not recv our peer id, (or some other err occured)
	// so we need some response that he registered us?
	wsh.hub.RegisterClient(NewClient(peerId, conn, TypeServer, aesgcm))
}

// connect to peers. returns peer as client struct
func connect(
	addr string,
	ourPeerId uuid.UUID,
	privateKey *ecdh.PrivateKey,
) (*Client, error) {
	url := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	slog.Debug("connecting to peer", "addr", addr)
	conn, _, err := websocket.DefaultDialer.Dial(url.String(), nil)
	if err != nil {
		slog.Error("error during connection to peer", "error", err)
		return nil, err
	}

	// ecdh key exchange
	publicKeyData, err := x509.MarshalPKIXPublicKey(privateKey.PublicKey())
	if err != nil {
		slog.Error("public key marshal", "error", err)
		return nil, err
	}
	if err = conn.WriteMessage(websocket.TextMessage, publicKeyData); err != nil {
		slog.Error("public key send", "error", err)
		return nil, err
	}
	_, peerPublicKeyData, err := conn.ReadMessage()
	if err != nil {
		slog.Error("peer public key receive", "error", err)
		return nil, err
	}
	peerParsedKey, err := x509.ParsePKIXPublicKey(peerPublicKeyData)
	if err != nil {
		slog.Error("peer public key parse", "error", err)
		return nil, err
	}
	peerPublicKey, ok := peerParsedKey.(*ecdh.PublicKey)
	if !ok {
		slog.Error("peer public key type assertion error")
		return nil, errors.New("peer public key type assertion error")
	}
	sharedSecret, err := privateKey.ECDH(peerPublicKey)
	if err != nil {
		slog.Error("shared secret derivation", "error", err)
		return nil, err
	}
	hkdf := hkdf.New(sha256.New, sharedSecret, nil, nil)
	encryptionKey := make([]byte, 16) // AES-128
	if _, err := io.ReadFull(hkdf, encryptionKey); err != nil {
		return nil, err
	}
	aesgcm, err := AesGCM(encryptionKey)
	if err != nil {
		slog.Error("aesgcm generation", "error", err)
		return nil, err
	}

	err = conn.WriteMessage(websocket.TextMessage, []byte(ourPeerId.String()))
	if err != nil {
		slog.Error("error sending peer UUID", "error", err)
		return nil, err
	}

	_, msg, err := conn.ReadMessage()
	if err != nil {
		slog.Error("server peer UUID read error", "error", err)
		return nil, err
	}

	serverPeerId, err := uuid.ParseBytes(msg)
	if err != nil {
		slog.Error("error parsing server peer UUID", "error", err, "message", msg)
		return nil, err
	}

	if serverPeerId == ourPeerId {
		slog.Error("tried to connect to self, closing...")
		conn.Close()
		return nil, errors.New("self connection")
	}

	return NewClient(serverPeerId, conn, TypeClient, aesgcm), nil
}

func showModalPopup(txt string, onCanvas fyne.Canvas) {
	// TODO maybe make this a temporary modal that is overwritten
	var modal *widget.PopUp
	closeBtn := widget.NewButton("Close", func() {
		modal.Hide()
	})
	popupContent := container.NewVBox(
		widget.NewLabel(txt),
		closeBtn,
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
		slog.Error("Invalid listen addres:", "address", *addr)
		return
	}
	serverAddr := fmt.Sprintf("%s:%s", *addr, *port)

	onClientRegistered := make(chan *Client)
	onClientUnregistered := make(chan *Client)
	onRecvMessage := make(chan *Message)
	onSentMessage := make(chan *Message)
	hub := newHub(onClientRegistered, onClientUnregistered, onRecvMessage, onSentMessage)
	defer hub.Shutdown()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go hub.Run(interrupt)

	// generate ecdh key-pair
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}

	wsHandler := &wsServeHandler{
		hub:        hub,
		peerId:     ourPeerId,
		privateKey: privateKey,
	}
	http.HandleFunc("/ws", wsHandler.serveWs)
	slog.Info("running server", "address", serverAddr)
	go http.ListenAndServe(serverAddr, nil)

	app := app.New()
	window := app.NewWindow("Guic")
	window.Resize(fyne.NewSize(800, 600))

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
		client, err := connect(peerEntry.Text, ourPeerId, privateKey)
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
	removePeerBtn := widget.NewButton("Remove", func() {
		removeClient := func(remove bool) {
			if !remove {
				return
			}
			if selectedPeerId == uuid.Nil {
				return
			}
			if err := hub.UnregisterClientByUUID(selectedPeerId); err != nil {
				showModalPopup(err.Error(), window.Canvas())
			}
		}
		dialog.NewConfirm("Confirm", "Disconnect client?", removeClient, window).Show()
	})
	removePeerBtn.Disable()
	connContainer := container.NewBorder(
		connEntry,
		nil, nil, nil,
		container.NewBorder(nil, removePeerBtn, nil, nil, peerList),
	)

	textEntry := widget.NewEntry()
	textEntry.SetPlaceHolder("Enter a message")
	sendMessage := func(text string) {
		if text == "" {
			return
		}
		if selectedPeerId == uuid.Nil {
			showModalPopup("Select peer", window.Canvas())
			return
		}
		if _, exist := hub.clients[selectedPeerId]; !exist {
			log.Fatalf("[ERROR] cur selected peer not found in hub: %s", selectedPeerId)
		}
		hub.sendMessage <- NewMsg(
			text,
			ourPeerId,
			selectedPeerId,
			hub.clients[selectedPeerId].conn.LocalAddr().String(),
			hub.clients[selectedPeerId].conn.RemoteAddr().String(),
		)
		textEntry.SetText("")
	}
	textEntry.OnSubmitted = sendMessage
	textEntryBtn := widget.NewButton("Send", func() {
		if textEntry.Text == "" {
			return
		}
		sendMessage(textEntry.Text)
	})
	textEntry.Disable()
	textEntryBtn.Disable()
	textSendEntry := container.NewVBox(
		textEntry,
		textEntryBtn,
	)
	placeholderScroll := container.NewScroll(widget.NewTextGrid())
	chatBorder := container.NewBorder(
		nil, textSendEntry, nil, nil, placeholderScroll,
	)
	content := container.NewHSplit(connContainer, chatBorder)
	content.SetOffset(0.3)

	peerList.OnSelected = func(id widget.ListItemID) {
		peerId := fyneListPeers[id]
		if peerId == uuid.Nil {
			return
		}
		selectedPeerId = peerId
		if _, exist := peerScrollWindows[selectedPeerId]; !exist {
			peerScrollWindows[selectedPeerId] = container.NewScroll(widget.NewTextGrid())
		}
		curPeerScroll = peerScrollWindows[selectedPeerId]
		chatBorder.Objects[0].(*container.Scroll).Hide()
		// Here we reassigning inner object of chat, but keep reference to it in peers scroll map
		// because we still want to show it later when client is selected again
		chatBorder.Objects[0] = curPeerScroll
		curPeerScroll.Show()
		textEntry.Enable()
		textEntryBtn.Enable()
		removePeerBtn.Enable()
	}
	// remove peer widgets from app window, disble control buttons
	unselectPeer := func(peerId uuid.UUID) {
		delete(peerScrollWindows, peerId)
		// replace with placeholder to delete reference for current peer scroll from UI
		chatBorder.Objects[0] = container.NewScroll(widget.NewTextGrid())
		unregisteredSelectedPeer := peerId == selectedPeerId
		if unregisteredSelectedPeer {
			selectedPeerId = uuid.Nil
			fyne.Do(func() {
				textEntry.Disable()
				textEntryBtn.Disable()
				removePeerBtn.Disable()
			})
		}
	}

	// UI reactor
	go func() {
		for {
			select {
			case client := <-onClientRegistered:
				slog.Debug("[UI reactor] registered", "client", client.PeerId)
				fyneListPeers = append(fyneListPeers, client.PeerId)
				fyne.Do(func() {
					peerList.Refresh()
				})
				peerScrollWindows[client.PeerId] = container.NewScroll(widget.NewTextGrid())
			case client := <-onClientUnregistered:
				slog.Debug("[UI reactor] unregistered", "client", client.PeerId)
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
					fyne.Do(func() {
						peerList.Unselect(widget.ListItemID(deleteIdx))
					})
				}
				unselectPeer(client.PeerId)
			case msg := <-onRecvMessage:
				slog.Debug("[UI reactor] message received", "from", msg.FromPeerAddr)
				fyne.Do(func() {
					if scroll, exist := peerScrollWindows[msg.FromPeerId]; exist {
						scroll.Content.(*widget.TextGrid).Append(fmt.Sprintf("[%s]: %s", msg.FromPeerAddr, msg.Txt))
					} else {
						log.Fatalf("[UI reactor] error, no scroll peer found for %s", msg.FromPeerId)
					}
				})
			case msg := <-onSentMessage:
				slog.Debug("[UI reactor] message sent", "to", msg.ToPeerAddr)
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
