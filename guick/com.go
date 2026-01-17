package main

import (
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/hkdf"
)

var upgrader = websocket.Upgrader{}

// peer that initiated the connection sends this data
type connectingPeerData struct {
	ChatId uuid.UUID `json:"chatId"`
	PeerId uuid.UUID `json:"peerId"`
}

type wsServeHandler struct {
	hub        *Hub
	privateKey *ecdh.PrivateKey
	peerId     uuid.UUID

	// incoming connection confirmation by user
	requestAccept func(r *http.Request) (<-chan bool, func())
}

// serve incoming peers connections. register connected peers in a hub
func (wsh *wsServeHandler) serveWs(w http.ResponseWriter, r *http.Request) {
	// request accept of incoming connection in UI
	accepted, closer := wsh.requestAccept(r)
	defer closer()
	if !<-accepted {
		// in fact any code other than 101 will suffice
		http.Error(w, "Connection refused", http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("ws upgrade error", "error", err)
		return
	}

	// close connection on error
	defer func() {
		if err != nil {
			conn.WriteMessage(websocket.CloseInternalServerErr, nil)
			conn.Close()
		}
	}()

	// ecdh key exchange and encryption key derivation
	_, data, err := conn.ReadMessage()
	if err != nil {
		slog.Error("peer public key read", "error", err)
		return
	}
	peerParsedKey, err := x509.ParsePKIXPublicKey(data)
	if err != nil {
		slog.Error("peer public key parse", "error", err)
		return
	}
	peerPublicKey, ok := peerParsedKey.(*ecdh.PublicKey)
	if !ok {
		slog.Error("peer public key type assertion", "error", err)
		return
	}
	publicKeyData, err := x509.MarshalPKIXPublicKey(wsh.privateKey.PublicKey())
	if err != nil {
		slog.Error("public key marshal", "error", err)
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
		return
	}
	aesgcm, err := AesGCM(encryptionKey)
	if err != nil {
		slog.Error("aesgcm generation", "error", err)
		return
	}

	// exchange app info
	_, encryptedData, err := conn.ReadMessage()
	if err != nil {
		slog.Error("peer info read", "error", err)
		return
	}
	slog.Debug("xxx", "connecting peer encrypted data", encryptedData)
	decryptedData, err := DecryptMessageData(encryptedData, aesgcm)
	if err != nil {
		slog.Error("peer message decrypt", "error", err)
		return
	}
	slog.Debug("xxx", "connecting peer decrypted data", decryptedData)
	jsonPeerData := &connectingPeerData{}
	if err = json.Unmarshal(decryptedData, jsonPeerData); err != nil {
		slog.Error("peer info unmarshal", "error", err)
		return
	}
	slog.Info("incoming peer connection", "peer", jsonPeerData.PeerId)
	if jsonPeerData.PeerId == wsh.peerId {
		slog.Error("tried to connect to self, closing connection...")
		return
	}
	encMsg, err := EncryptMessage([]byte(wsh.peerId.String()), aesgcm)
	if err != nil {
		return
	}
	encryptedData, err = json.Marshal(encMsg)
	if err != nil {
		return
	}
	if err = conn.WriteMessage(websocket.BinaryMessage, encryptedData); err != nil {
		slog.Error("peer UUID send error", "error", err)
		return
	}
	// TODO here we assume that we are ok after sending our peer id,
	// but we may be not ok if the client did not recv our peer id, (or some other err occured)
	// so we need some response that he registered us?
	wsh.hub.RegisterPeer(
		NewPeer(jsonPeerData.ChatId, jsonPeerData.PeerId, conn, TypeServer, aesgcm),
	)
}

// tries to connect to a peer. returns peer as client struct
// chatId is id of the peer chat we are connecting to
func ConnectToPeer(
	addr string,
	chatId uuid.UUID,
	ourPeerId uuid.UUID,
	privateKey *ecdh.PrivateKey,
) (*Peer, error) {
	url := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	slog.Debug("connecting to peer", "addr", addr)
	conn, _, err := websocket.DefaultDialer.Dial(url.String(), nil)
	if err != nil {
		slog.Debug("peer connection", "error", err)
		if errors.Is(err, websocket.ErrBadHandshake) {
			return nil, errors.New("rejected by peer")
		}
		return nil, errors.New("peer connection error")
	}

	// establish encryption first
	// ecdh key exchange and encryption key derivation
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

	// exchange app info
	jsonPeerData, err := json.Marshal(&connectingPeerData{chatId, ourPeerId})
	if err != nil {
		return nil, err
	}
	slog.Debug("xxx", "connecting peer decrypted data", jsonPeerData)
	encryptedMsg, err := EncryptMessage(jsonPeerData, aesgcm)
	if err != nil {
		return nil, err
	}
	encryptedData, err := json.Marshal(encryptedMsg)
	if err != nil {
		return nil, err
	}
	slog.Debug("xxx", "connecting peer encrypted data", encryptedData)
	err = conn.WriteMessage(websocket.BinaryMessage, encryptedData)
	if err != nil {
		slog.Error("peer info send", "error", err)
		return nil, err
	}
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		slog.Error("peer info read", "error", err)
		return nil, err
	}
	uuidData, err := DecryptMessageData(msgData, aesgcm)
	if err != nil {
		return nil, err
	}
	serverPeerId, err := uuid.Parse(string(uuidData))
	if err != nil {
		slog.Error("peer uuid parse", "error", err, "message", encryptedMsg)
		return nil, err
	}
	if serverPeerId == ourPeerId {
		slog.Error("peer connection", "error", "self connection")
		conn.Close()
		return nil, errors.New("self connection")
	}
	return NewPeer(chatId, serverPeerId, conn, TypeClient, aesgcm), nil
}
