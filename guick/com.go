package main

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
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

type PeerInfo struct {
	Id   uuid.UUID `json:"Id"`
	Name string    `json:"Name"`
}

// Provided to client-peers by server-peers.
// Server address is the adress of server-peer that generated credentials and
// where client-peer will be connecting to.
// Cipherdata is encrypted data which can by decrypted only by server-peer to verify
// that credentials are authentic (server address fiedl is not validated)
type ConnectionCredentials struct {
	ServerAddress string `json:"ServerAddress"`
	Cipherdata    string `json:"Cipherdata"`
}

// peer that initiated the connection sends this data
type ConnectionInfo struct {
	Peer      PeerInfo              `json:"PeerInfo"`
	ConnCreds ConnectionCredentials `json:"ConnCreds"`
}

type wsServeHandler struct {
	hub        *Hub
	privateKey *ecdh.PrivateKey
	peerInfo   *PeerInfo
	// used to decrypt credentials from connection string
	// (implies that connection string was previously encrypted
	// with same aesgcm and nonce and shared with peer)
	aesgcm cipher.AEAD
	nonce  []byte
	// hook incoming connection confirmation by user
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

	peer, err := acceptPeerConnection(
		conn,
		wsh.privateKey,
		wsh.peerInfo,
		wsh.nonce,
		wsh.aesgcm,
	)
	if err != nil {
		slog.Error("connection accept", "error", err)
		conn.WriteMessage(websocket.CloseInternalServerErr, nil)
		conn.Close()
		return
	}
	wsh.hub.RegisterPeer(peer)
}

// accepts incoming client-peer connection
func acceptPeerConnection(
	conn *websocket.Conn,
	privateKey *ecdh.PrivateKey,
	peerInfo *PeerInfo,
	_nonce []byte,
	_aesgcm cipher.AEAD,
) (*Peer, error) {
	// ecdh key exchange and encryption key derivation
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	peerParsedKey, err := x509.ParsePKIXPublicKey(data)
	if err != nil {
		return nil, err
	}
	peerPublicKey, ok := peerParsedKey.(*ecdh.PublicKey)
	if !ok {
		return nil, err
	}
	publicKeyData, err := x509.MarshalPKIXPublicKey(privateKey.PublicKey())
	if err != nil {
		return nil, err
	}
	if err = conn.WriteMessage(websocket.TextMessage, publicKeyData); err != nil {
		return nil, err
	}
	sharedSecret, err := privateKey.ECDH(peerPublicKey)
	if err != nil {
		return nil, err
	}
	hkdf := hkdf.New(sha256.New, sharedSecret, nil, nil)
	encryptionKey := make([]byte, 16) // AES-128
	if _, err := io.ReadFull(hkdf, encryptionKey); err != nil {
		return nil, err
	}
	aesgcm, err := AesGCM(encryptionKey)
	if err != nil {
		return nil, err
	}

	// peer info exchange
	_, clientInfoData, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	decryptedData, err := DecryptMessageData(clientInfoData, aesgcm)
	if err != nil {
		return nil, err
	}
	clientInfo := &ConnectionInfo{}
	if err = json.Unmarshal(decryptedData, clientInfo); err != nil {
		return nil, err
	}
	if clientInfo.Peer.Id == peerInfo.Id {
		return nil, err
	}
	chatId := uuid.New()
	if len(clientInfo.ConnCreds.Cipherdata) > 0 {
		// if server-peer generated connection string to existing chat
		cipherdata, err := base64.StdEncoding.DecodeString(clientInfo.ConnCreds.Cipherdata)
		if err != nil {
			return nil, err
		}
		credsData, err := DecryptData(cipherdata, _nonce, _aesgcm)
		if err != nil {
			return nil, err
		}
		chatId, err = uuid.FromBytes(credsData)
		if err != nil {
			return nil, err
		}
	}
	// NOTE: credentials in server info are ignored
	serverInfo, err := json.Marshal(
		&ConnectionInfo{*peerInfo, ConnectionCredentials{}},
	)
	encryptedMsg, err := EncryptMessage(serverInfo, aesgcm)
	if err != nil {
		return nil, err
	}
	if err = conn.WriteJSON(encryptedMsg); err != nil {
		return nil, err
	}
	// TODO type server is not needed here
	peer := NewPeer(chatId, clientInfo.Peer.Id, clientInfo.Peer.Name, conn, TypeServer, aesgcm)
	return peer, nil
}

// initiates connection with a server-peer
func ConnectToPeer(
	privateKey *ecdh.PrivateKey,
	connInfo *ConnectionInfo,
) (*Peer, error) {
	url := url.URL{Scheme: "ws", Host: string(connInfo.ConnCreds.ServerAddress), Path: "/ws"}
	slog.Debug("connecting to peer", "addr", connInfo.ConnCreds.ServerAddress)
	conn, _, err := websocket.DefaultDialer.Dial(url.String(), nil)
	if err != nil {
		if errors.Is(err, websocket.ErrBadHandshake) {
			return nil, errors.New("rejected by peer")
		}
		return nil, errors.New("peer connection error")
	}

	// establish encryption first
	// ecdh key exchange and encryption key derivation
	publicKeyData, err := x509.MarshalPKIXPublicKey(privateKey.PublicKey())
	if err != nil {
		return nil, err
	}
	if err = conn.WriteMessage(websocket.TextMessage, publicKeyData); err != nil {
		return nil, err
	}
	_, peerPublicKeyData, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	peerParsedKey, err := x509.ParsePKIXPublicKey(peerPublicKeyData)
	if err != nil {
		return nil, err
	}
	peerPublicKey, ok := peerParsedKey.(*ecdh.PublicKey)
	if !ok {
		return nil, errors.New("peer public key type assertion error")
	}
	sharedSecret, err := privateKey.ECDH(peerPublicKey)
	if err != nil {
		return nil, err
	}
	hkdf := hkdf.New(sha256.New, sharedSecret, nil, nil)
	encryptionKey := make([]byte, 16) // AES-128
	if _, err := io.ReadFull(hkdf, encryptionKey); err != nil {
		return nil, err
	}
	aesgcm, err := AesGCM(encryptionKey)
	if err != nil {
		return nil, err
	}

	// exchange app info
	clientInfo, err := json.Marshal(connInfo)
	if err != nil {
		return nil, err
	}
	encryptedMsg, err := EncryptMessage(clientInfo, aesgcm)
	if err != nil {
		return nil, err
	}
	if err = conn.WriteJSON(encryptedMsg); err != nil {
		return nil, err
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	serverData, err := DecryptMessageData(data, aesgcm)
	if err != nil {
		return nil, err
	}
	// NOTE: credentials in server info are ignored
	serverInfo := &ConnectionInfo{}
	if err = json.Unmarshal(serverData, serverInfo); err != nil {
		return nil, err
	}
	if serverInfo.Peer.Id == ourPeerId {
		conn.Close()
		return nil, errors.New("self connection")
	}
	return NewPeer(
		uuid.New(),
		serverInfo.Peer.Id,
		serverInfo.Peer.Name,
		conn,
		TypeClient,
		aesgcm,
	), nil
}
