package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image/color"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"

	"github.com/google/uuid"
	"golang.design/x/clipboard"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

// BUG client will create many instances of the same chat if connecting to the same server

// TODO change constant addition of border containers to two VBoxes for each peer
// TODO add send of images and gifs
// TODO add calls (audio, video, personal, group)
// TODO add headless mode

var appHost = flag.String("host", "0.0.0.0", "http server host")
var appPort = flag.String("port", "8080", "http server port")
var debug = flag.Bool("debug", false, "debug mode")

var programLevel = slog.LevelInfo

var ourPeerId = uuid.New()

// current chat to send messages to
var selectedChatId = uuid.Nil

// current chat text grid to show and append sent/recv messages to
var curChatContainer *fyne.Container = nil

// a slice just for conversion between fyne list id to app chat UUID
var fyneChatList = []uuid.UUID{}

// chat containers to select from when selecting current chat in UI
var chatsMap = make(map[uuid.UUID]*fyne.Container)

var sentConnectRequests = make(map[string]struct{})

var localAddrs = make(map[string]struct{}, 100)

func main() {
	flag.Parse()

	if *debug {
		programLevel = slog.LevelDebug
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel})
	slog.SetDefault(slog.New(h))

	serverHost := net.ParseIP(*appHost)
	serverAddr := net.JoinHostPort(*appHost, *appPort)
	if serverHost == nil {
		slog.Error("Invalid listen host", "host", *appHost)
		return
	}
	if serverHost.IsUnspecified() {
		// we don't know which iface will be used for accepting the connection.
		// So need to parse all interfaces addresses to prevent self-connection

		// hack to add 0.0.0.0 address
		localAddrs[net.JoinHostPort(net.IPv4zero.String(), *appPort)] = struct{}{}
		ifaces, err := net.Interfaces()
		if err != nil {
			slog.Error("net interface read", "error", err)
			return
		}
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, netaddr := range addrs {
				ip, _, err := net.ParseCIDR(netaddr.String())
				if err != nil {
					continue
				}
				localAddrs[net.JoinHostPort(ip.String(), *appPort)] = struct{}{}
			}
		}
	} else {
		localAddrs[serverAddr] = struct{}{}
	}

	onPeerRegistered := make(chan *Peer)
	onPeerUnregistered := make(chan *Peer)
	onRecvMessage := make(chan *Message)
	onSentMessage := make(chan *Message)
	hub := newHub(onPeerRegistered, onPeerUnregistered, onRecvMessage, onSentMessage)
	defer hub.Shutdown()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go hub.Run(interrupt)

	// generate ecdh key-pair
	// used to derive shared secret and then transient encryption key to communicate
	// with each peer
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	// generate a persistent encryption key for signing server provided data
	signKey, err := NewKey()
	if err != nil {
		panic(err)
	}

	var application fyne.App
	var mainWindow fyne.Window
	var requestsContainer *fyne.Container

	// UI confirmation for incoming connections
	acceptConnection := func(r *http.Request) (<-chan bool, func()) {
		acceptChan := make(chan bool)
		displayName := r.RemoteAddr
		cookies := r.Cookies()
		if len(cookies) > 0 {
			if cookies[0].Name == "nickname" {
				displayName = cookies[0].Value
			}
		}
		// accept chan awaits button press inside element
		requestElement := NewPeerRequestElement(displayName, acceptChan)
		fyne.Do(func() { requestsContainer.Add(requestElement) })
		fyne.Do(requestsContainer.Refresh)
		closer := func() {
			close(acceptChan)
			fyne.Do(func() { requestsContainer.Remove(requestElement) })
		}
		return acceptChan, closer
	}

	nickname := GenRandNickname()
	wsHandler := &wsServeHandler{
		hub:           hub,
		peerInfo:      &PeerInfo{ourPeerId, nickname},
		privateKey:    privateKey,
		_key:          signKey,
		requestAccept: acceptConnection,
	}
	http.HandleFunc("/ws", wsHandler.serveWs)
	slog.Info("application is running", "address", serverAddr, "name", nickname)
	go http.ListenAndServe(serverAddr, nil)

	application = app.New()
	mainWindow = application.NewWindow("Guic")
	mainWindow.Resize(fyne.NewSize(800, 600))

	peerAddressEntry := widget.NewEntry()
	peerAddressEntry.SetPlaceHolder("Peer IP / Connection String")

	chatList := widget.NewList(
		func() int { return len(fyneChatList) },
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(lii widget.ListItemID, co fyne.CanvasObject) {
			chatId := fyneChatList[lii]
			if chatId == uuid.Nil {
				return
			}
			chat, exist := hub.chats[chatId]
			if !exist {
				log.Fatalf("chat not found: %s", chatId)
			}
			co.(*widget.Label).SetText(chat.id.String())
		},
	)

	uiOnConnect := func() {
		if peerAddressEntry.Text == "" {
			return
		}
		connCreds := &ConnectionCredentials{}
		// treat it as connection string first
		conndata, err := base64.StdEncoding.DecodeString(peerAddressEntry.Text)
		if err == nil {
			if err = json.Unmarshal(conndata, connCreds); err != nil {
				NewModalPopup("invalid connection credentials format", mainWindow.Canvas()).Show()
				return
			}
		} else {
			// if connection string parse failed - parse ipv4 address
			host, port, err := net.SplitHostPort(peerAddressEntry.Text)
			if err != nil {
				NewModalPopup(fmt.Sprintf("invalid peer address: %s", err), mainWindow.Canvas()).Show()
				return
			}
			if peerHost := net.ParseIP(host); peerHost == nil {
				NewModalPopup("incorrect IP address format", mainWindow.Canvas()).Show()
				return
			}
			connCreds.ServerAddress = net.JoinHostPort(host, port)
			// tell server-peer that we don't have a connection string and want a fresh chat
			// by providing empty cipherdata
			connCreds.Cipherdata = ""
		}

		if _, exist := localAddrs[connCreds.ServerAddress]; exist {
			NewModalPopup("can't connect to self", mainWindow.Canvas()).Show()
			return
		}
		if _, requestSent := sentConnectRequests[connCreds.ServerAddress]; requestSent {
			NewModalPopup("request already sent", mainWindow.Canvas()).Show()
			return
		}
		go func() {
			sentConnectRequests[connCreds.ServerAddress] = struct{}{}
			defer func() { delete(sentConnectRequests, connCreds.ServerAddress) }()
			peer, err := ConnectToPeer(
				privateKey,
				&ConnectionInfo{PeerInfo{ourPeerId, nickname}, *connCreds},
			)
			if err != nil {
				slog.Error("connect to server-peer", "error", err)
				NewModalPopup(fmt.Sprintf("connection error: %s", err), mainWindow.Canvas()).Show()
				return
			}
			hub.RegisterPeer(peer)
			NewModalPopup(
				fmt.Sprintf("Client %s connected!", peer.Name),
				mainWindow.Canvas(),
			).Show()
		}()
		NewModalPopup("Request sent", mainWindow.Canvas()).Show()
		peerAddressEntry.SetText("")
	}
	peerAddressEntry.OnSubmitted = func(s string) { uiOnConnect() }
	connEntry := container.NewVBox(
		peerAddressEntry,
		widget.NewButton("Connect", uiOnConnect),
	)
	rmChatBtn := widget.NewButton("Remove", func() {
		rmChat := func(remove bool) {
			if !remove {
				return
			}
			if selectedChatId == uuid.Nil {
				return
			}
			chat, exist := hub.LockedPeekChat(selectedChatId)
			if exist {
				hub.removeChat <- chat
			}
		}
		dialog.NewConfirm("Confirm", "Remove chat?", rmChat, mainWindow).Show()
	})
	rmChatBtn.Disable()
	cpyConnStringBtn := widget.NewButton("Copy connection string", func() {
		if err := clipboard.Init(); err != nil {
			NewModalPopup("clipboard not available", mainWindow.Canvas()).Show()
			return
		}
		chatId := uuid.New()
		if selectedChatId != uuid.Nil {
			chat, exist := hub.LockedPeekChat(selectedChatId)
			if !exist {
				panic("selected chat does not exist")
			}
			if !chat.isHosted {
				NewModalPopup("you are not chat host", mainWindow.Canvas()).Show()
				return
			}
			chatId = selectedChatId
		}
		encChatId, err := Encrypt(chatId[:], signKey)
		if err != nil {
			slog.Error("connection data creation", "error", err)
			NewModalPopup("connection data creation error", mainWindow.Canvas()).Show()
			return
		}
		b64chat := base64.StdEncoding.EncodeToString(encChatId)
		condata, err := json.Marshal(&ConnectionCredentials{serverAddr, b64chat})
		if err != nil {
			slog.Error("connection data creation", "error", err)
			NewModalPopup("connection data creation error", mainWindow.Canvas()).Show()
			return
		}
		clipboard.Write(clipboard.FmtText, []byte(base64.StdEncoding.EncodeToString(condata)))
		NewModalPopup("copied to clipboard", mainWindow.Canvas()).Show()
	})
	connContainer := container.NewBorder(
		container.NewVBox(connEntry, cpyConnStringBtn),
		nil, nil, nil,
		container.NewBorder(nil, rmChatBtn, nil, nil, chatList),
	)

	textEntry := widget.NewEntry()
	textEntry.SetPlaceHolder("Enter a message")
	sendMessage := func(text string) {
		if text == "" {
			return
		}
		if selectedChatId == uuid.Nil {
			NewModalPopup("Select chat first", mainWindow.Canvas()).Show()
			return
		}
		if _, exist := hub.chats[selectedChatId]; !exist {
			log.Fatalf("selected chat not found in hub: %s", selectedChatId)
		}
		hub.sendMessage <- &Message{
			FromPeerId:   ourPeerId,
			FromPeerName: nickname,
			ToChatId:     selectedChatId,
			Type:         TypeText,
			Data:         []byte(text),
		}
		textEntry.SetText("")
	}
	textEntry.OnSubmitted = sendMessage
	textEntryBtn := widget.NewButton("Send", func() {
		if textEntry.Text == "" {
			return
		}
		sendMessage(textEntry.Text)
	})
	clipFileBtn := widget.NewButton("📎", func() {
		onSelect := func(r fyne.URIReadCloser, err error) {
			if r == nil {
				return
			}
			data, err := io.ReadAll(r)
			if err != nil {
				// TODO some notification if file cant be processed
				return
			}
			hub.sendMessage <- &Message{
				FromPeerId:   ourPeerId,
				FromPeerName: nickname,
				ToChatId:     selectedChatId,
				Type:         TypeImg,
				Data:         data,
			}
		}
		dialog.NewFileOpen(onSelect, mainWindow).Show()
	})
	textEntry.Disable()
	textEntryBtn.Disable()
	clipFileBtn.Disable()
	textSendEntry := container.NewVBox(
		textEntry,
		container.NewBorder(nil, nil, clipFileBtn, nil, textEntryBtn),
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

	chatList.OnSelected = func(id widget.ListItemID) {
		chatId := fyneChatList[id]
		if chatId == uuid.Nil {
			return
		}
		selectedChatId = chatId
		prevChatGrid := curChatContainer
		if _, exist := chatsMap[selectedChatId]; !exist {
			chatsMap[selectedChatId] = container.NewVBox() // NewChatTextGrid()
		}
		curChatContainer = chatsMap[selectedChatId]
		if prevChatGrid != nil {
			prevChatGrid.Hide()
		}
		// Here we reassigning inner object of chat, but keep reference to it in peers scroll map
		// because we still want to show it later when client is selected again
		chatBorder.Objects[0] = curChatContainer
		curChatContainer.Show()
		textEntry.Enable()
		textEntryBtn.Enable()
		clipFileBtn.Enable()
		rmChatBtn.Enable()
	}
	// remove chat widgets from app window, disble control buttons
	unselectChat := func(chatId uuid.UUID) {
		delete(chatsMap, chatId)
		// replace with placeholder to delete reference for current peer scroll from UI
		chatBorder.Objects[0] = widget.NewTextGrid()
		if chatId == selectedChatId {
			selectedChatId = uuid.Nil
			fyne.Do(func() {
				textEntry.Disable()
				textEntryBtn.Disable()
				clipFileBtn.Disable()
				rmChatBtn.Disable()
			})
		}

	}
	rmChatFromList := func(chatId uuid.UUID, chatList *[]uuid.UUID, chatListWdg *widget.List) {
		deleteIdx := -1
		for i, id := range *chatList {
			if id == chatId {
				deleteIdx = i
				break
			}
		}
		if deleteIdx != -1 {
			*chatList = append((*chatList)[:deleteIdx], (*chatList)[deleteIdx+1:]...)
			fyne.Do(func() {
				chatListWdg.Unselect(widget.ListItemID(deleteIdx))
			})
		}
	}

	shiftCtrlV := &desktop.CustomShortcut{
		KeyName:  fyne.KeyV,
		Modifier: fyne.KeyModifierShift | fyne.KeyModifierControl,
	}
	mainWindow.Canvas().AddShortcut(shiftCtrlV, func(shortcut fyne.Shortcut) {
		if err := clipboard.Init(); err != nil {
			slog.Error("clipboard not available")
			return
		}
		data := clipboard.Read(clipboard.FmtImage)
		hub.sendMessage <- &Message{
			FromPeerId:   ourPeerId,
			FromPeerName: nickname,
			ToChatId:     selectedChatId,
			Type:         TypeImg,
			Data:         data,
		}
	})

	// a UI reactor
	// essentially reads events from hub channels and updates relevant UI components
	go func() {
		for {
			select {
			case peer := <-onPeerRegistered:
				if _, exist := chatsMap[peer.ChatId]; !exist {
					fyneChatList = append(fyneChatList, peer.ChatId)
				}
				fyne.Do(chatList.Refresh)
				if _, exist := chatsMap[peer.ChatId]; !exist {
					chatsMap[peer.ChatId] = container.NewVBox()
				}
			case chat := <-hub.ChatRemoved:
				rmChatFromList(chat.id, &fyneChatList, chatList)
				unselectChat(chat.id)
				fyne.Do(chatList.Refresh)
			case msg := <-onRecvMessage:
				chat, exist := chatsMap[msg.ToChatId]
				if !exist {
					log.Fatalf("error, no chat window found for %s", msg.ToChatId)
				}
				switch msg.Type {
				case TypeText:
					m := fmt.Sprintf("[%s]: %s", msg.FromPeerName, msg.Data)
					t := canvas.NewText(m, color.White)
					fyne.Do(func() {
						chat.Add(container.NewBorder(nil, nil, t, nil))
						chat.Refresh()
					})
				case TypeImg:
					// read image
					img := canvas.NewImageFromReader(bytes.NewReader(msg.Data), uuid.New().String())
					img.FillMode = canvas.ImageFillOriginal
					m := fmt.Sprintf("[%s]:", msg.FromPeerName)
					t := canvas.NewText(m, color.White)
					fyne.Do(func() {
						chat.Add(container.NewBorder(nil, nil, t, nil))
						chat.Add(container.NewBorder(nil, nil, img, nil))
						chat.Refresh()
					})
				}
			case msg := <-onSentMessage:
				chat, exist := chatsMap[msg.ToChatId]
				if !exist {
					log.Fatalf("error, no chat window found for %s", msg.ToChatId)
				}
				switch msg.Type {
				case TypeText:
					m := fmt.Sprintf("%s  ", msg.Data)
					t := canvas.NewText(m, color.White)
					fyne.Do(func() {
						chat.Add(container.NewBorder(nil, nil, nil, t))
						chat.Refresh()
					})
				case TypeImg:
					img := canvas.NewImageFromReader(bytes.NewReader(msg.Data), uuid.New().String())
					img.FillMode = canvas.ImageFillOriginal
					fyne.Do(func() {
						chat.Add(container.NewBorder(nil, nil, nil, img))
						chat.Refresh()
					})
				}
			}
		}
	}()

	mainWindow.SetContent(content)
	mainWindow.Show()
	application.Run()
}
