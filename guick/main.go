package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"

	"github.com/google/uuid"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// TODOS
// - prevent connection to self
// - add submit functionality for modal popup

var addr = flag.String("addr", "0.0.0.0", "http server address")
var port = flag.String("port", "8080", "http server port")
var debug = flag.Bool("debug", false, "debug mode")

var programLevel = slog.LevelInfo

var ourPeerId = uuid.New()

// current peer to send messages to
var selectedPeerId = uuid.Nil

// current peer text grid to show and append sent/recv messages to
var curPeerGrid *widget.TextGrid = nil

// a slice just for conversion between fyne list id to app peer UUID
var fyneListPeers = []uuid.UUID{}

// peer chat containers to select from when selecting current peer in UI
var peerTextGrids = make(map[uuid.UUID]*widget.TextGrid)

var sentConnectRequests = make(map[string]struct{})

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

func createPeerRequestElement(text string, accepted chan<- bool) *fyne.Container {
	return container.NewHBox(
		widget.NewLabel(text),
		widget.NewButton("✔", func() { accepted <- true }),
		widget.NewButton("✖", func() { accepted <- false }),
	)
}

func main() {
	flag.Parse()

	if *debug {
		programLevel = slog.LevelDebug
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel})
	slog.SetDefault(slog.New(h))

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

	var application fyne.App
	var mainWindow fyne.Window
	var requestsContainer *fyne.Container

	// UI confirmation for incoming connections
	acceptConnection := func(r *http.Request) (<-chan bool, func()) {
		acceptChan := make(chan bool)
		// accept chan awaits button press inside element
		requestElement := createPeerRequestElement(r.RemoteAddr, acceptChan)
		fyne.Do(func() { requestsContainer.Add(requestElement) })
		fyne.Do(requestsContainer.Refresh)
		closer := func() {
			close(acceptChan)
			fyne.Do(func() { requestsContainer.Remove(requestElement) })
		}
		return acceptChan, closer
	}

	wsHandler := &wsServeHandler{
		hub:           hub,
		peerId:        ourPeerId,
		privateKey:    privateKey,
		requestAccept: acceptConnection,
	}
	http.HandleFunc("/ws", wsHandler.serveWs)
	slog.Info("running server", "address", serverAddr)
	go http.ListenAndServe(serverAddr, nil)

	application = app.New()
	mainWindow = application.NewWindow("Guic")
	mainWindow.Resize(fyne.NewSize(800, 600))

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
			showModalPopup(errTxt, mainWindow.Canvas())
			return
		}
		peerIP := net.ParseIP(host)
		if peerIP == nil {
			errTxt := "incorrect IP address format"
			log.Println(errTxt)
			showModalPopup(errTxt, mainWindow.Canvas())
			return
		}
		peerAddress := peerEntry.Text
		if _, requestSent := sentConnectRequests[peerAddress]; requestSent {
			showModalPopup("request already sent", mainWindow.Canvas())
			return
		}
		// TODO also need to check here if already connected
		go func() {
			sentConnectRequests[peerAddress] = struct{}{}
			defer func() { delete(sentConnectRequests, peerAddress) }()
			client, err := ConnectToPeer(peerAddress, ourPeerId, privateKey)
			if err != nil {
				showModalPopup(fmt.Sprintf("connection error: %s", err), mainWindow.Canvas())
				return
			}
			hub.RegisterClient(client)
			showModalPopup(
				fmt.Sprintf("Client %s connected!", client.conn.RemoteAddr()),
				mainWindow.Canvas(),
			)
		}()
		showModalPopup("Request sent", mainWindow.Canvas())
		peerEntry.SetText("")
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
				showModalPopup(err.Error(), mainWindow.Canvas())
			}
		}
		dialog.NewConfirm("Confirm", "Disconnect client?", removeClient, mainWindow).Show()
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
			showModalPopup("Select peer", mainWindow.Canvas())
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
	placeholderTextGrid := widget.NewTextGrid()
	chatBorder := container.NewBorder(
		nil, textSendEntry, nil, nil, placeholderTextGrid,
	)
	requestsContainer = container.NewVBox()
	content := container.NewHSplit(
		container.NewAppTabs(
			container.NewTabItem("Peers", connContainer),
			container.NewTabItem("Requests", requestsContainer),
		),
		chatBorder,
	)
	content.SetOffset(0.3)

	peerList.OnSelected = func(id widget.ListItemID) {
		peerId := fyneListPeers[id]
		if peerId == uuid.Nil {
			return
		}
		selectedPeerId = peerId
		prevPeerGrid := curPeerGrid
		if _, exist := peerTextGrids[selectedPeerId]; !exist {
			textGrid := widget.NewTextGrid()
			textGrid.Scroll = fyne.ScrollVerticalOnly
			peerTextGrids[selectedPeerId] = textGrid
		}
		curPeerGrid = peerTextGrids[selectedPeerId]
		if prevPeerGrid != nil {
			prevPeerGrid.Hide()
		}
		// Here we reassigning inner object of chat, but keep reference to it in peers scroll map
		// because we still want to show it later when client is selected again
		chatBorder.Objects[0] = curPeerGrid
		curPeerGrid.Show()
		textEntry.Enable()
		textEntryBtn.Enable()
		removePeerBtn.Enable()
	}
	// remove peer widgets from app window, disble control buttons
	unselectPeer := func(peerId uuid.UUID) {
		delete(peerTextGrids, peerId)
		// replace with placeholder to delete reference for current peer scroll from UI
		chatBorder.Objects[0] = widget.NewTextGrid()
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
				fyneListPeers = append(fyneListPeers, client.PeerId)
				fyne.Do(func() {
					peerList.Refresh()
				})
				textGrid := widget.NewTextGrid()
				textGrid.Scroll = fyne.ScrollVerticalOnly
				peerTextGrids[client.PeerId] = textGrid
			case client := <-onClientUnregistered:
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
				fyne.Do(func() {
					if grid, exist := peerTextGrids[msg.FromPeerId]; exist {
						grid.Append(fmt.Sprintf("[%s]: %s", msg.FromPeerAddr, msg.Txt))
					} else {
						log.Fatalf("error, no scroll peer found for %s", msg.FromPeerId)
					}
				})
			case msg := <-onSentMessage:
				fyne.Do(func() {
					if grid, exist := peerTextGrids[msg.ToPeerId]; exist {
						grid.Append(fmt.Sprintf("[me]: %s", msg.Txt))
					} else {
						log.Fatalf("error, no scroll peer found for %s", msg.ToPeerAddr)
					}
				})
			}
		}
	}()

	mainWindow.SetContent(content)
	mainWindow.Show()
	application.Run()
}
