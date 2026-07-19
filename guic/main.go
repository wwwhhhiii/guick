package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image/color"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"golang.design/x/clipboard"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
)

var debug = flag.Bool("debug", false, "debug mode")

var programLevel = slog.LevelInfo

var ourPeerId = uuid.New()

// current chat to send messages to
var selectedChatId = uuid.Nil

// current chat text grid to show and append sent/recv messages to
var currentChatWindow *container.Scroll = nil

// a slice just for conversion between fyne list id to app chat UUID
var fyneChatList = []uuid.UUID{}

// chat containers to select from when selecting current chat in UI
var chatsMapUI = make(map[uuid.UUID]*container.Scroll)

var chatsMap = make(map[uuid.UUID]*Chat)

// pending peer yet to be connected
type PendingPeer struct {
	conn     *webrtc.PeerConnection
	dataChan *webrtc.DataChannel
	toChat   *Chat
}

func (p *PendingPeer) close() {
	if p.dataChan != nil {
		p.dataChan.Close()
	}
	if p.conn != nil {
		p.conn.Close()
	}
}

var localAddrs = make(map[string]struct{}, 100)

func main() {
	flag.Parse()
	if *debug {
		programLevel = slog.LevelDebug
	}

	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel})
	slog.SetDefault(slog.New(h))

	// webrtc
	webrtcConf := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// app
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	peerConnected := make(chan *Peer, 50)
	peerDisconnected := make(chan *Peer, 50)
	messageReceived := make(chan struct {
		string
		*Peer
	}, 100)
	// TODO messageSent
	_ = make(chan struct {
		string
		*Chat
	}, 100)
	ctrlMessage := make(chan struct {
		string
		*Peer
	}, 10)
	imgMessage := make(chan struct {
		byte
		*Peer
	}, 10)

	var application fyne.App
	var mainWindow fyne.Window
	var pendingPeersContainer *fyne.Container

	nickname := GenRandNickname()
	slog.Info("app is running", "name", nickname)

	application = app.New()
	mainWindow = application.NewWindow("Guic")
	mainWindow.Resize(fyne.NewSize(800, 600))

	peerOfferEntry := widget.NewEntry()
	peerOfferEntry.SetPlaceHolder("Paste peer offer here")

	chatList := widget.NewList(
		func() int { return len(fyneChatList) },
		func() fyne.CanvasObject {
			chatIdLabel := widget.NewLabel("")
			return container.NewHBox(chatIdLabel)
		},
		func(lii widget.ListItemID, co fyne.CanvasObject) {
			chatId := fyneChatList[lii]
			if chatId == uuid.Nil {
				return
			}
			chat, exist := chatsMap[chatId]
			if !exist {
				log.Fatalf("chat not found: %s", chatId)
			}
			co.(*fyne.Container).Objects[0].(*widget.Label).SetText(chat.id.String())
		},
	)

	uiOnOffer := func() {
		if peerOfferEntry.Text == "" {
			return
		}
		encsdpstr := peerOfferEntry.Text
		sdpData, err := base64.StdEncoding.DecodeString(encsdpstr)
		if err != nil {
			slog.Error("peer offer SDP decode", "error", err)
			return
		}
		sdp := webrtc.SessionDescription{}
		if err := json.Unmarshal(sdpData, &sdp); err != nil {
			slog.Error("error sdp unmarshal")
			return
		}
		conn, err := webrtc.NewPeerConnection(webrtcConf)
		if err != nil {
			slog.Error("error opening connection")
		}
		chat := &Chat{id: uuid.New(), peers: make(map[uuid.UUID]*Peer), isHosted: false}
		chatsMap[chat.id] = chat
		peer, err := SetupOfferee(conn, nickname, messageReceived)
		if err != nil {
			log.Fatalln(err)
		}
		chat.addPeers(peer)

		if err := conn.SetRemoteDescription(sdp); err != nil {
			log.Fatalln(err)
		}

		answer, err := conn.CreateAnswer(nil)
		if err != nil {
			log.Fatalln(err)
		}

		if err = conn.SetLocalDescription(answer); err != nil {
			log.Fatalln(err)
		}

		// TODO async this
		iceGatherComplete := webrtc.GatheringCompletePromise(conn)
		<-iceGatherComplete

		pConnected := make(chan struct{})
		conn.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			slog.Debug("connection state", "peer", "", "state", state.String())
			if state == webrtc.PeerConnectionStateFailed {
				chat.DisconnectPeer(peer.id)
			}
			if state == webrtc.PeerConnectionStateClosed {
				chat.DisconnectPeer(peer.id)
			}
			if state == webrtc.PeerConnectionStateConnected {
				peer.state = StateConnected
				peerConnected <- peer
				pConnected <- struct{}{}
			}
			if state == webrtc.PeerConnectionStateDisconnected {
				peerDisconnected <- peer
				delete(chatsMap, peer.chat.id)
			}
		})

		ssdp, err := conn.RemoteDescription().Unmarshal()
		if err != nil {
			// TODO upanic
			panic(err)
		}
		for _, d := range ssdp.MediaDescriptions {
			for _, a := range d.Attributes {
				if a.IsICECandidate() {
					err := conn.AddICECandidate(webrtc.ICECandidateInit{Candidate: a.String()})
					if err != nil {
						slog.Error("add ICE candidate", "error", err)
					} else {
						slog.Debug("add ICE candidate", "candidate", a.String())
					}
				}
			}
		}

		peerOfferEntry.SetText("")

		sdpdata, err := json.Marshal(answer)
		if err != nil {
			slog.Error("peer answer marshal", "error", err)
			NewModalPopup(fmt.Sprintf("%s", err), mainWindow.Canvas()).Show()
			return
		}
		sdpstr := base64.StdEncoding.EncodeToString(sdpdata)
		CpyPopup("Share this with your peer", sdpstr, mainWindow.Canvas()).Show()
	}

	peerOfferEntry.OnSubmitted = func(s string) { uiOnOffer() }

	uiOnGenerateOffer := func() {
		conn, err := webrtc.NewPeerConnection(webrtcConf)
		if err != nil {
			log.Fatalln(err)
		}

		// TODO add currently opened chat or create new if no chat opened
		// TODO also delete it after peer disconnect if chat was created if error occurs
		chat := &Chat{id: uuid.New(), peers: make(map[uuid.UUID]*Peer), isHosted: true}
		// TODO maybe it is already present
		chatsMap[chat.id] = chat
		peer, err := SetupOfferor(conn, nickname, messageReceived)
		if err != nil {
			log.Fatalln(err)
		}
		chat.addPeers(peer)

		offer, err := conn.CreateOffer(nil)
		if err != nil {
			slog.Error("create peer connection", "error", err)
			return
		}

		if err = conn.SetLocalDescription(offer); err != nil {
			slog.Error("set local SDP", "error", err)
			return
		}

		// TODO async this
		iceGatherComplete := webrtc.GatheringCompletePromise(conn)
		<-iceGatherComplete

		conn.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			slog.Debug("connection state", "peer", "", "state", state.String())
			if state == webrtc.PeerConnectionStateFailed {
				chat.DisconnectPeer(peer.id)
			}
			if state == webrtc.PeerConnectionStateClosed {
				chat.DisconnectPeer(peer.id)
			}
			if state == webrtc.PeerConnectionStateConnected {
				peer.state = StateConnected
				peerConnected <- peer
			}
			if state == webrtc.PeerConnectionStateDisconnected {
				peerDisconnected <- peer
				delete(chatsMap, peer.chat.id)
			}
		})

		sdpdata, err := json.Marshal(*conn.LocalDescription())
		if err != nil {
			slog.Error("peer offer marshal", "id", peer.id, "error", err)
			chat.DisconnectPeer(peer.id)
			return
		}
		sdpstr := base64.StdEncoding.EncodeToString(sdpdata)
		if err = clipboard.Init(); err != nil {
			NewModalPopup(
				fmt.Sprintf(
					"Clipboard is not available, share the following string with your peer:\n%s",
					sdpstr,
				),
				mainWindow.Canvas(),
			).Show()
		}
		clipboard.Write(clipboard.FmtText, []byte(sdpstr))
		NewModalPopup("Copied to clipboard, share it with your peer", mainWindow.Canvas()).Show()

		// add pending peer
		answerCh := make(chan string)
		cancelCh := make(chan struct{})
		pendingPeerElement := NewPendingPeerElement(GenRandNickname(), answerCh, cancelCh)
		pendingPeersContainer.Add(pendingPeerElement)

		// TODO possibly read errors and log to logs window
		// pending peer handler
		go func() {
			defer func() {
				pendingPeersContainer.Remove(pendingPeerElement)
			}()
			for {
				select {
				case <-time.After(60 * time.Second):
					slog.Error("pending peer answer wait timeout", "id", peer.id)
					chat.DisconnectPeer(peer.id)
					return
				case answer := <-answerCh:
					sdpdata, err := base64.StdEncoding.DecodeString(answer)
					if err != nil {
						slog.Error("pending peer SDP answer decode", "id", peer.id, "error", err)
						chat.DisconnectPeer(peer.id)
						return
					}
					sdpanswer := webrtc.SessionDescription{}
					if err := json.Unmarshal(sdpdata, &sdpanswer); err != nil {
						slog.Error("pending peer SDP answer read", "id", peer.id, "error", err)
						chat.DisconnectPeer(peer.id)
						return
					}
					if err := peer.conn.SetRemoteDescription(sdpanswer); err != nil {
						slog.Error("set remote peer SDP", "id", peer.id, "error", err)
						chat.DisconnectPeer(peer.id)
						return
					}
					remoteSDP, err := conn.RemoteDescription().Unmarshal()
					if err != nil {
						slog.Error("read remote peer SDP", "id", peer.id, "error", err)
						chat.DisconnectPeer(peer.id)
						return
					}
					for _, d := range remoteSDP.MediaDescriptions {
						for _, a := range d.Attributes {
							if a.IsICECandidate() {
								err := conn.AddICECandidate(webrtc.ICECandidateInit{Candidate: a.String()})
								if err != nil {
									slog.Error("add ICE candidate", "id", peer.id, "error", err)
								} else {
									slog.Debug("add ICE candidate", "id", peer.id, "candidate", a.String())
								}
							}
						}
					}
					return
				case <-cancelCh:
					slog.Info("pending peer cancelled", "id", peer.id)
					chat.DisconnectPeer(peer.id)
					return
				}
			}
		}()
	}

	offerEntry := container.NewVBox(
		peerOfferEntry,
		widget.NewButton("Answer", uiOnOffer),
		widget.NewButton("Generate offer", uiOnGenerateOffer),
	)
	rmChatBtn := widget.NewButton("Remove", func() {
		rmChat := func(remove bool) {
			if !remove {
				return
			}
			if selectedChatId == uuid.Nil {
				return
			}
			// TODO mux
			chat, exist := chatsMap[selectedChatId]
			if exist {
				chat.Close()
			}
			delete(chatsMap, chat.id)
		}
		dialog.NewConfirm("Confirm", "Remove chat?", rmChat, mainWindow).Show()
	})
	rmChatBtn.Disable()
	connContainer := container.NewBorder(
		container.NewVBox(offerEntry),
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
		chat, exist := chatsMap[selectedChatId]
		if !exist {
			log.Fatalf("selected chat %s not found in global chats map", selectedChatId)
		}

		chatUI, exist := chatsMapUI[chat.id]
		if !exist {
			slog.Error("chat not found in UI chats", "id", chat.id)
			return
		}
		content := chatUI.Content.(*fyne.Container)
		t := canvas.NewText(fmt.Sprintf("%s ", textEntry.Text), color.White)
		content.Add(container.NewBorder(nil, nil, nil, t))
		content.Refresh()
		chatUI.ScrollToBottom()

		if err := chat.sendMessage(text); err != nil {
			slog.Error("send message", "error", err)
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
			// TODO
			_, err = io.ReadAll(r)
			if err != nil {
				// TODO some notification if file cant be processed
				return
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
	pendingPeersContainer = container.NewVBox()
	content := container.NewHSplit(
		container.NewAppTabs(
			container.NewTabItem("Peers", connContainer),
			container.NewTabItem("Pending", pendingPeersContainer),
		),
		chatBorder,
	)
	content.SetOffset(0.3)

	getOrCreateChatWindow := func(chatId uuid.UUID) *container.Scroll {
		if _, exist := chatsMapUI[chatId]; !exist {
			w := container.NewVScroll(container.NewVBox())
			w.SetMinSize(fyne.NewSize(200, 50))
			chatsMapUI[chatId] = w
		}
		return chatsMapUI[chatId]
	}

	chatList.OnSelected = func(lii widget.ListItemID) {
		chatId := fyneChatList[lii]
		if chatId == uuid.Nil {
			return
		}
		// TODO this condition does not wooooork
		if chatId == selectedChatId {
			chatList.Unselect(lii)
			return
		}
		selectedChatId = chatId
		prevChatWindow := currentChatWindow
		currentChatWindow = getOrCreateChatWindow(selectedChatId)
		if prevChatWindow != nil {
			prevChatWindow.Hide()
		}
		// Here we reassigning inner object of chat, but keep reference to it in peers scroll map
		// because we still want to show it later when client is selected again
		chatBorder.Objects[0] = currentChatWindow
		currentChatWindow.Show()
		textEntry.Enable()
		textEntryBtn.Enable()
		clipFileBtn.Enable()
		rmChatBtn.Enable()
	}
	chatList.OnUnselected = func(lii widget.ListItemID) {
		if lii < len(fyneChatList) {
			chatId := fyneChatList[lii]
			// replace with placeholder to delete reference for current peer scroll from UI
			chatBorder.Objects[0] = container.NewVScroll(container.NewVBox())
			if chatId == selectedChatId {
				selectedChatId = uuid.Nil
				textEntry.Disable()
				textEntryBtn.Disable()
				clipFileBtn.Disable()
				rmChatBtn.Disable()
			}
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
		// TODO
		// data := clipboard.Read(clipboard.FmtImage)
	})

	// a UI reactor
	go func() {
		for {
			select {
			case peer := <-peerConnected:
				if _, exist := chatsMapUI[peer.chat.id]; !exist {
					fyneChatList = append(fyneChatList, peer.chat.id)
				}
				fyne.Do(chatList.Refresh)
				getOrCreateChatWindow(peer.chat.id)
			case peer := <-peerDisconnected:
				// TODO review this case
				rmChatFromList(peer.chat.id, &fyneChatList, chatList)
				lii := slices.Index(fyneChatList, peer.chat.id)
				if lii != -1 {
					chatId := fyneChatList[lii]
					delete(chatsMapUI, chatId)
					chatList.Unselect(widget.ListItemID(lii))
				}
				fyne.Do(chatList.Refresh)
			case msg := <-messageReceived:
				chat, exist := chatsMapUI[msg.Peer.chat.id]
				if !exist {
					slog.Error("no chat window found", "chat", msg.Peer.chat.id)
					continue
				}
				chatContent := chat.Content.(*fyne.Container)
				m := fmt.Sprintf("[%s]: %s", msg.Peer.Name, msg.string)
				t := canvas.NewText(m, color.White)
				fyne.Do(func() {
					chatContent.Add(container.NewBorder(nil, nil, t, nil))
					chatContent.Refresh()
				})
			case msg := <-ctrlMessage:
				slog.Info("ctrl message", "msg", msg.string, "peer", msg.Peer)
			case msg := <-imgMessage:
				slog.Info("img message", "data", msg.byte)
			}
		}
	}()

	mainWindow.SetContent(content)
	mainWindow.Show()
	application.Run()
}
