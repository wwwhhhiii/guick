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

	peerConnectedUI := make(chan *Peer, 50)
	peerDisconnectedUI := make(chan *Peer, 50)
	messageReceived := make(chan *Message, 100)
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

	nickname := GenRandNickname()
	slog.Info("app is running", "name", nickname)

	application = app.New()
	mainWindow = application.NewWindow("Guic")
	mainWindow.Resize(fyne.NewSize(800, 600))

	chatListWdg := widget.NewList(
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

	uiOnConnectToChat := func() {
		connectWin := application.NewWindow("Connect to chat")
		connectWin.Resize(fyne.NewSize(400, 200))
		offerEntry := widget.NewMultiLineEntry()
		offerEntry.Wrapping = fyne.TextWrapBreak
		offerEntry.SetPlaceHolder("Paste offer here")
		var submitBtn *widget.Button
		var cancelBtn *widget.Button
		submit := func() {
			// defer connectWin.Close()
			offerEntry.Disable()
			submitBtn.Disable()
			cancelBtn.Disable()

			sdp, err := decodeSdp(offerEntry.Text)
			if err != nil {
				slog.Error("error decoding sdp")
				return
			}
			conn, err := webrtc.NewPeerConnection(webrtcConf)
			if err != nil {
				slog.Error("error opening connection")
				return
			}
			chat := &Chat{id: uuid.New(), peers: make(map[uuid.UUID]*Peer), isHosted: false}
			chatsMap[chat.id] = chat
			peer, err := SetupOfferee(conn, nickname, messageReceived)
			if err != nil {
				log.Fatalln(err)
			}
			chat.addPeers(peer)
			if err := conn.SetRemoteDescription(*sdp); err != nil {
				log.Fatalln(err)
			}
			answer, err := conn.CreateAnswer(nil)
			if err != nil {
				log.Fatalln(err)
			}
			if err = conn.SetLocalDescription(answer); err != nil {
				log.Fatalln(err)
			}

			go func() {
				activity := widget.NewActivity()
				activity.Start()
				defer fyne.Do(func() { activity.Stop(); activity.Hide() })
				fyne.Do(func() {
					connectWin.SetContent(
						container.NewBorder(widget.NewLabel("Contacting STUN servers"), nil, nil, nil, activity),
					)
				})

				<-webrtc.GatheringCompletePromise(conn)
				conn.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
					slog.Debug("connection state", "peer", "", "state", state.String())
					if state == webrtc.PeerConnectionStateFailed {
						chat.DisconnectPeer(peer.id)
					}
					if state == webrtc.PeerConnectionStateClosed {
						chat.DisconnectPeer(peer.id)
					}
					if state == webrtc.PeerConnectionStateConnected {
						peer.addFlag(connectedFlag)
						// need to wait for peer to send his data
						<-peer.Ready()
						peerConnectedUI <- peer
						NewModalPopup(
							fmt.Sprintf("Peer is %s connected", peer.Name), mainWindow.Canvas(),
						).Show()
						fyne.Do(connectWin.Close)
					}
					if state == webrtc.PeerConnectionStateDisconnected {
						peerDisconnectedUI <- peer
						delete(chatsMap, peer.chat.id)
					}
				})
				ssdp, err := conn.RemoteDescription().Unmarshal()
				if err != nil {
					// TODO unpanic
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
				sdpdata, err := json.Marshal(answer)
				if err != nil {
					slog.Error("peer answer marshal", "error", err)
					NewModalPopup(fmt.Sprintf("%s", err), mainWindow.Canvas()).Show()
					return
				}
				sdpstr := base64.StdEncoding.EncodeToString(sdpdata)

				answerEntry := widget.NewMultiLineEntry()
				answerEntry.Wrapping = fyne.TextWrapBreak
				answerEntry.Disable()
				answerEntry.SetText(sdpstr)
				content := container.NewBorder(
					container.NewVBox(
						widget.NewLabel("Share this with your peer"),
						widget.NewButton("Copy", func() {
							clipboard.Write(clipboard.FmtText, []byte(sdpstr))
						}),
					),
					nil,
					nil,
					nil,
					answerEntry,
				)
				fyne.Do(func() { connectWin.SetContent(content) })
			}()
		}
		offerEntry.OnSubmitted = func(s string) { submit() }
		submitBtn = widget.NewButton("Submit", submit)
		cancelBtn = widget.NewButton("Cancel", func() { connectWin.Close() })
		content := container.NewBorder(
			nil,
			container.NewVBox(submitBtn, cancelBtn),
			nil,
			nil,
			offerEntry,
		)
		connectWin.SetContent(content)
		connectWin.Show()
	}
	uiOnAddPeerToChat := func() {
		addWin := application.NewWindow("Add new peer")
		addWin.Resize(fyne.NewSize(400, 200))

		chatsIds := make([]string, len(chatsMap))
		for k := range chatsMapUI {
			chatsIds = append(chatsIds, k.String())
		}
		chatsSelect := widget.NewSelect(chatsIds, func(s string) {})
		chatsSelect.PlaceHolder = "Create new chat"

		submit := func() {
			var chat *Chat
			if chatsSelect.SelectedIndex() == -1 {
				// TODO prompt user for chat name
				chat = &Chat{id: uuid.New(), name: GenRandNickname(), peers: make(map[uuid.UUID]*Peer), isHosted: true}
			} else {
				id, err := uuid.Parse(chatsSelect.Selected)
				if err != nil {
					log.Fatalln(err)
				}
				var exist bool
				chat, exist = getChat(id)
				if !exist {
					NewModalPopup("Error. Selected chat does not exist", addWin.Canvas()).Show()
					return
				}
				if !chat.isHosted {
					NewModalPopup("You are not chat host", addWin.Canvas()).Show()
					return
				}
			}
			conn, err := webrtc.NewPeerConnection(webrtcConf)
			if err != nil {
				log.Fatalln(err)
			}
			peer, err := SetupOfferor(conn, nickname, chat.name, messageReceived)
			if err != nil {
				log.Fatalln(err)
			}
			offer, err := conn.CreateOffer(nil)
			if err != nil {
				slog.Error("create peer connection", "error", err)
				return
			}
			if err = conn.SetLocalDescription(offer); err != nil {
				slog.Error("set local SDP", "error", err)
				return
			}
			go func() {
				activity := widget.NewActivity()
				activity.Start()
				defer fyne.Do(func() { activity.Stop(); activity.Hide() })
				fyne.Do(func() {
					addWin.SetContent(
						container.NewBorder(
							widget.NewLabel("Contacting STUN servers..."), nil, nil, nil, activity),
					)
				})

				<-webrtc.GatheringCompletePromise(conn)

				conn.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
					slog.Debug("connection state", "peer", "", "state", state.String())
					if state == webrtc.PeerConnectionStateFailed {
						chat.DisconnectPeer(peer.id)
					}
					if state == webrtc.PeerConnectionStateClosed {
						chat.DisconnectPeer(peer.id)
					}
					if state == webrtc.PeerConnectionStateConnected {
						peer.addFlag(connectedFlag)
						// need to wait for peer to send his data
						<-peer.Ready()
						chat.addPeers(peer)
						addChat(chat)
						peerConnectedUI <- peer
						fyne.Do(func() {
							addWin.SetContent(container.NewVBox(widget.NewLabel(fmt.Sprintf("Peer %s is connected", peer.Name))))
						})
					}
					if state == webrtc.PeerConnectionStateDisconnected {
						peerDisconnectedUI <- peer
						rmChat(peer.chat.id)
					}
				})
				sdpdata, err := json.Marshal(*conn.LocalDescription())
				if err != nil {
					slog.Error("peer offer marshal", "id", peer.id, "error", err)
					chat.DisconnectPeer(peer.id)
					return
				}
				sdpstr := base64.StdEncoding.EncodeToString(sdpdata)
				offerentry := widget.NewMultiLineEntry()
				offerentry.SetText(sdpstr)
				offerentry.Disable()
				offerentry.Wrapping = fyne.TextWrapBreak
				answerentry := widget.NewMultiLineEntry()
				answerentry.Wrapping = fyne.TextWrapBreak

				onFinish := func() {
					sdp, err := decodeSdp(answerentry.Text)
					if err != nil {
						// TODO
						log.Fatalln(err)
					}
					if err := conn.SetRemoteDescription(*sdp); err != nil {
						// TODO
						log.Fatalln(err)
					}
					fyne.Do(
						func() {
							activity.Start()
							activity.Show()
							addWin.SetContent(activity)
						},
					)
				}

				content := container.NewBorder(
					container.NewVBox(
						widget.NewLabel("Share this with your peer"),
						widget.NewButton("Copy", func() { clipboard.Write(clipboard.FmtText, []byte(sdpstr)) }),
					),
					container.NewVBox(
						widget.NewButton("Finish", onFinish),
						widget.NewButton("Cancel", func() {}),
					),
					nil,
					nil,
					container.NewVBox(
						offerentry,
						widget.NewLabel("Put peer answer here"),
						answerentry,
					),
				)
				fyne.Do(func() { addWin.SetContent(content) })
			}()
		}
		content := container.NewBorder(
			widget.NewLabel("Select chat to add peer to"),
			container.NewVBox(
				widget.NewButton("Next", submit),
			),
			nil,
			nil,
			chatsSelect,
		)
		addWin.SetContent(content)
		// TODO do a cleanup logic
		addWin.SetOnClosed(func() {})
		addWin.Show()
	}
	controlMenu := container.NewVBox(
		widget.NewButton("Connect", uiOnConnectToChat),
		widget.NewButton("Add peer to chat", uiOnAddPeerToChat),
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
			chat, exist := getChat(selectedChatId)
			if exist {
				chat.Close()
			}
			rmChat(chat.id)
		}
		dialog.NewConfirm("Confirm", "Remove chat?", rmChat, mainWindow).Show()
	})
	rmChatBtn.Disable()
	connContainer := container.NewBorder(
		container.NewVBox(controlMenu),
		nil, nil, nil,
		container.NewBorder(nil, rmChatBtn, nil, nil, chatListWdg),
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
		chat, exist := getChat(selectedChatId)
		if !exist {
			log.Fatalf("selected chat %s not found in global chats map", selectedChatId)
		}
		chatUI, exist := chatsMapUI[chat.id]
		if !exist {
			slog.Error("chat not found in UI chats", "id", chat.id)
			return
		}
		content := chatUI.Content.(*fyne.Container)
		t := canvas.NewText(fmt.Sprintf("[me]: %s ", textEntry.Text), color.White)
		content.Add(container.NewBorder(nil, nil, nil, t))
		content.Refresh()
		chatUI.ScrollToBottom()
		msg := &Message{PeerName: nickname, PeerId: ourPeerId, ChatId: chat.id, Text: text}
		if err := chat.sendMessage(msg); err != nil {
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

	content := container.NewHSplit(
		connContainer,
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

	chatListWdg.OnSelected = func(lii widget.ListItemID) {
		chatId := fyneChatList[lii]
		if chatId == uuid.Nil {
			return
		}
		// TODO this condition does not wooooork
		if chatId == selectedChatId {
			chatListWdg.Unselect(lii)
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
	chatListWdg.OnUnselected = func(lii widget.ListItemID) {
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

	// UI reactor
	go func() {
		for {
			select {
			case peer := <-peerConnectedUI:
				if _, exist := chatsMapUI[peer.chat.id]; !exist {
					fyneChatList = append(fyneChatList, peer.chat.id)
				}
				fyne.Do(chatListWdg.Refresh)
				getOrCreateChatWindow(peer.chat.id)
			case peer := <-peerDisconnectedUI:
				// TODO review this case
				rmChatFromList(peer.chat.id, &fyneChatList, chatListWdg)
				lii := slices.Index(fyneChatList, peer.chat.id)
				if lii != -1 {
					chatId := fyneChatList[lii]
					delete(chatsMapUI, chatId)
					chatListWdg.Unselect(widget.ListItemID(lii))
				}
				fyne.Do(chatListWdg.Refresh)
			case msg := <-messageReceived:
				chat, exist := chatsMapUI[msg.ChatId]
				if !exist {
					slog.Error("no chat window found", "chat", msg.ChatId)
					continue
				}
				chatContent := chat.Content.(*fyne.Container)
				m := fmt.Sprintf("[%s]: %s", msg.PeerName, msg.Text)
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
	mainWindow.SetMaster()
	mainWindow.Show()
	application.Run()
}
