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

	// peerId exchange
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		slog.Error("peer UUID read error", "error", err)
		return
	}
	uuidText, err := DecryptMessageData(msgData, aesgcm)
	if err != nil {
		return
	}
	peerId, err := uuid.Parse(uuidText)
	if err != nil {
		slog.Error("UUID parse error", "error", err)
		return
	}
	slog.Info("incoming peer connection", "peer", uuidText)
	if peerId == wsh.peerId {
		slog.Error("tried to connect to self, closing connection...")
		return
	}
	encMsg, err := EncryptMessage(wsh.peerId.String(), aesgcm)
	if err != nil {
		return
	}
	msgData, err = json.Marshal(encMsg)
	if err != nil {
		return
	}
	if err = conn.WriteMessage(websocket.BinaryMessage, msgData); err != nil {
		slog.Error("peer UUID send error", "error", err)
		return
	}
	// TODO here we assuming that we are ok after sending our peer id,
	// but we may be not ok if the client did not recv our peer id, (or some other err occured)
	// so we need some response that he registered us?
	wsh.hub.RegisterClient(NewClient(peerId, conn, TypeServer, aesgcm))
}

// tries to connect to a peer. returns peer as client struct
func ConnectToPeer(
	addr string,
	ourPeerId uuid.UUID,
	privateKey *ecdh.PrivateKey,
) (*Client, error) {
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

	// peerId exchange
	msg, err := EncryptMessage(ourPeerId.String(), aesgcm)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	err = conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		slog.Error("error sending peer UUID", "error", err)
		return nil, err
	}
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		slog.Error("server peer UUID read error", "error", err)
		return nil, err
	}
	uuidText, err := DecryptMessageData(msgData, aesgcm)
	if err != nil {
		return nil, err
	}
	serverPeerId, err := uuid.Parse(uuidText)
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
