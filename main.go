package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-memdb"
)

// ── Sabitler ─────────────────────────────────────────────────────

const (
	StatusOnline  = "online"
	StatusOffline = "offline"
	StatusIdle    = "idle"
	StatusDND     = "dnd" // rahatsız etmeyin

	MaxStatusTextLen = 128  // durum metni maksimum uzunluk
	MaxTokenLen      = 512  // token maksimum uzunluk
	MaxReadSize      = 4096 // websocket mesaj limit
)

// ── Veri Yapıları ────────────────────────────────────────────────

// Connection — go-memdb'de saklanan bağlantı kaydı
type Connection struct {
	ID           string // ws pointer adresi ("%p")
	WS           *websocket.Conn
	WriteMu      sync.Mutex // ws yazma kilidi (concurrent write koruması)
	UserID       int
	GroupID      int
	ChannelID    int
	Status       string // online/offline/idle/dnd
	StatusText   string // özel durum metni (boş olabilir)
	ChannelGroup string // "channelID,groupID" compound index
	UserGroup    string // "userID,groupID" compound index
}

// safeWrite — concurrent write panic'i önlemek için mutex ile yazma
func (c *Connection) safeWrite(messageType int, data []byte) error {
	c.WriteMu.Lock()
	defer c.WriteMu.Unlock()
	return c.WS.WriteMessage(messageType, data)
}

// ── JSON Mesaj Tipleri ───────────────────────────────────────────

// Client → Server
type IncomingMessage struct {
	Type       string `json:"type"`                  // "auth", "join", "status"
	Token      string `json:"token,omitempty"`        // auth için
	GroupID    int    `json:"group_id,omitempty"`     // join için
	ChannelID  int    `json:"channel_id,omitempty"`   // join için
	Status     string `json:"status,omitempty"`       // status için
	StatusText string `json:"status_text,omitempty"`  // status için
}

// Server → Client
type OutgoingAuth struct {
	Type    string `json:"type"`
	Success bool   `json:"success"`
	UserID  int    `json:"user_id,omitempty"`
}

type OutgoingChannelJoin struct {
	Type       string `json:"type"`
	UserID     int    `json:"user_id"`
	ChannelID  int    `json:"channel_id"`
	GroupID    int    `json:"group_id"`
	Status     string `json:"status"`
	StatusText string `json:"status_text"`
}

type OutgoingChannelLeave struct {
	Type      string `json:"type"`
	UserID    int    `json:"user_id"`
	ChannelID int    `json:"channel_id"`
	GroupID   int    `json:"group_id"`
}

type UserStatus struct {
	S string `json:"s"` // status
	T string `json:"t"` // status_text
}

type OutgoingPresence struct {
	Type     string                       `json:"type"`
	GroupID  int                          `json:"group_id"`
	Channels map[string][]int             `json:"channels"`
	Statuses map[string]UserStatus        `json:"statuses"`
}

type OutgoingStatusChange struct {
	Type       string `json:"type"`
	UserID     int    `json:"user_id"`
	Status     string `json:"status"`
	StatusText string `json:"status_text"`
}

// ── Global Değişkenler ───────────────────────────────────────────

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	mdb         *memdb.MemDB
	db          *sql.DB
	testMode    bool
	testUserSeq atomic.Int64
)

// ── go-memdb Schema ──────────────────────────────────────────────

func createSchema() *memdb.DBSchema {
	return &memdb.DBSchema{
		Tables: map[string]*memdb.TableSchema{
			"connections": {
				Name: "connections",
				Indexes: map[string]*memdb.IndexSchema{
					// Primary index — ws pointer adresi
					"id": {
						Name:    "id",
						Unique:  true,
						Indexer: &memdb.StringFieldIndex{Field: "ID"},
					},
					// UserID index
					"user_id": {
						Name:         "user_id",
						Unique:       false,
						AllowMissing: true,
						Indexer:      &memdb.IntFieldIndex{Field: "UserID"},
					},
					// ChannelID index
					"channel_id": {
						Name:         "channel_id",
						Unique:       false,
						AllowMissing: true,
						Indexer:      &memdb.IntFieldIndex{Field: "ChannelID"},
					},
					// GroupID index
					"group_id": {
						Name:         "group_id",
						Unique:       false,
						AllowMissing: true,
						Indexer:      &memdb.IntFieldIndex{Field: "GroupID"},
					},
					// Compound index: channel_id + group_id
					"channel_group": {
						Name:         "channel_group",
						Unique:       false,
						AllowMissing: true,
						Indexer:      &memdb.StringFieldIndex{Field: "ChannelGroup"},
					},
					// Compound index: user_id + group_id
					"user_group": {
						Name:         "user_group",
						Unique:       false,
						AllowMissing: true,
						Indexer:      &memdb.StringFieldIndex{Field: "UserGroup"},
					},
				},
			},
		},
	}
}

// ── Connection CRUD ──────────────────────────────────────────────

func connID(ws *websocket.Conn) string {
	return fmt.Sprintf("%p", ws)
}

func insertConn(c *Connection) {
	txn := mdb.Txn(true)
	txn.Insert("connections", c)
	txn.Commit()
}

func deleteConn(ws *websocket.Conn) (*Connection, bool) {
	txn := mdb.Txn(true)
	raw, err := txn.First("connections", "id", connID(ws))
	if err == nil && raw != nil {
		txn.Delete("connections", raw)
		txn.Commit()
		return raw.(*Connection), true
	}
	txn.Commit()
	return nil, false
}

func getConn(ws *websocket.Conn) (*Connection, bool) {
	txn := mdb.Txn(false)
	defer txn.Abort()
	raw, err := txn.First("connections", "id", connID(ws))
	if err != nil || raw == nil {
		return nil, false
	}
	return raw.(*Connection), true
}

// updateConn — memdb'de bağlantı kaydını güncelle (delete + insert)
func updateConn(c *Connection) {
	txn := mdb.Txn(true)
	old, err := txn.First("connections", "id", c.ID)
	if err == nil && old != nil {
		txn.Delete("connections", old)
	}
	txn.Insert("connections", c)
	txn.Commit()
}

// getConnsByGroup — belirli bir gruptaki tüm bağlantıları döndür
func getConnsByGroup(groupID int) []*Connection {
	txn := mdb.Txn(false)
	defer txn.Abort()
	it, err := txn.Get("connections", "group_id", groupID)
	if err != nil {
		return nil
	}
	var conns []*Connection
	for obj := it.Next(); obj != nil; obj = it.Next() {
		conns = append(conns, obj.(*Connection))
	}
	return conns
}

// ── Presence Mantığı ─────────────────────────────────────────────

// buildPresenceMap — grup içindeki tüm kullanıcıların kanal konumlarını oluştur
// Çevrimdışı kullanıcılar: sadece bir kanalda iseler gösterilir
func buildPresenceMap(groupID int) (map[string][]int, map[string]UserStatus) {
	conns := getConnsByGroup(groupID)

	channels := make(map[string][]int)       // channelID → [userID, ...]
	statuses := make(map[string]UserStatus)   // userID → {status, text}
	seen := make(map[int]bool)                // aynı kullanıcıyı tekrar eklememek için

	for _, c := range conns {
		if seen[c.UserID] {
			continue
		}
		seen[c.UserID] = true

		// Çevrimdışı kullanıcı kanalda değilse gösterme
		if c.Status == StatusOffline && c.ChannelID == 0 {
			continue
		}

		userIDStr := strconv.Itoa(c.UserID)
		statuses[userIDStr] = UserStatus{S: c.Status, T: c.StatusText}

		if c.ChannelID != 0 {
			chStr := strconv.Itoa(c.ChannelID)
			channels[chStr] = append(channels[chStr], c.UserID)
		}
	}

	return channels, statuses
}

// ── Broadcast Helpers ────────────────────────────────────────────

// broadcastToGroup — gruptaki tüm bağlantılara mesaj gönder (göndereni hariç tut)
func broadcastToGroup(groupID int, excludeWS *websocket.Conn, msg []byte) {
	conns := getConnsByGroup(groupID)
	for _, c := range conns {
		if c.WS == excludeWS {
			continue
		}
		c.safeWrite(websocket.TextMessage, msg)
	}
}

// sendJSON — tek bir bağlantıya JSON mesaj gönder
func sendJSON(conn *Connection, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	conn.safeWrite(websocket.TextMessage, data)
}

// ── Event İşleyicileri ───────────────────────────────────────────

func handleJoin(ws *websocket.Conn, conn *Connection, msg IncomingMessage) {
	// ID aralık kontrolü — negatif veya mantıksız büyük değerleri reddet
	if msg.GroupID < 0 || msg.ChannelID < 0 {
		return
	}

	oldGroupID := conn.GroupID
	oldChannelID := conn.ChannelID

	newGroupID := msg.GroupID
	newChannelID := msg.ChannelID

	// Eski kanaldan çıkış bildirimi
	if oldGroupID != 0 && oldChannelID != 0 {
		leaveMsg, _ := json.Marshal(OutgoingChannelLeave{
			Type:      "channel_leave",
			UserID:    conn.UserID,
			ChannelID: oldChannelID,
			GroupID:   oldGroupID,
		})
		broadcastToGroup(oldGroupID, ws, leaveMsg)
	}

	// Bağlantıyı güncelle
	conn.GroupID = newGroupID
	conn.ChannelID = newChannelID
	conn.ChannelGroup = fmt.Sprintf("%d,%d", newChannelID, newGroupID)
	conn.UserGroup = fmt.Sprintf("%d,%d", conn.UserID, newGroupID)
	updateConn(conn)

	// Yeni kanala giriş bildirimi (gruptakilere)
	if newGroupID != 0 && newChannelID != 0 {
		joinMsg, _ := json.Marshal(OutgoingChannelJoin{
			Type:       "channel_join",
			UserID:     conn.UserID,
			ChannelID:  newChannelID,
			GroupID:    newGroupID,
			Status:     conn.Status,
			StatusText: conn.StatusText,
		})
		broadcastToGroup(newGroupID, ws, joinMsg)
	}

	// Kullanıcıya mevcut presence haritasını gönder
	if newGroupID != 0 {
		channels, statuses := buildPresenceMap(newGroupID)
		sendJSON(conn, OutgoingPresence{
			Type:     "presence",
			GroupID:  newGroupID,
			Channels: channels,
			Statuses: statuses,
		})
	}
}

func handleStatus(ws *websocket.Conn, conn *Connection, msg IncomingMessage) {
	// Geçerli durum kontrolü
	switch msg.Status {
	case StatusOnline, StatusOffline, StatusIdle, StatusDND:
		// geçerli
	default:
		return // geçersiz status → yoksay
	}

	// StatusText uzunluk sınırı
	statusText := msg.StatusText
	if len(statusText) > MaxStatusTextLen {
		statusText = statusText[:MaxStatusTextLen]
	}

	conn.Status = msg.Status
	conn.StatusText = statusText
	updateConn(conn)

	// Gruptakilere bildir
	if conn.GroupID != 0 {
		statusMsg, _ := json.Marshal(OutgoingStatusChange{
			Type:       "status_change",
			UserID:     conn.UserID,
			Status:     conn.Status,
			StatusText: conn.StatusText,
		})
		broadcastToGroup(conn.GroupID, ws, statusMsg)
	}
}

func handleDisconnect(ws *websocket.Conn) {
	conn, ok := deleteConn(ws)
	if !ok {
		return
	}
	// Gruptakilere kanal terk bildirimi
	if conn.GroupID != 0 && conn.ChannelID != 0 {
		leaveMsg, _ := json.Marshal(OutgoingChannelLeave{
			Type:      "channel_leave",
			UserID:    conn.UserID,
			ChannelID: conn.ChannelID,
			GroupID:   conn.GroupID,
		})
		broadcastToGroup(conn.GroupID, nil, leaveMsg)
	}
}

// ── WebSocket Handler ────────────────────────────────────────────

func onlineHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer ws.Close()
	defer handleDisconnect(ws)

	// Read deadline & pong handler
	ws.SetReadLimit(MaxReadSize)
	ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// ── 1. Auth (ilk mesaj zorunlu) ──────────────────────────────

	_, rawMsg, err := ws.ReadMessage()
	if err != nil {
		return
	}

	var authMsg IncomingMessage
	if err := json.Unmarshal(rawMsg, &authMsg); err != nil || authMsg.Type != "auth" {
		// İlk mesaj auth değilse bağlantıyı kapat
		data, _ := json.Marshal(OutgoingAuth{Type: "auth", Success: false})
		ws.WriteMessage(websocket.TextMessage, data)
		return
	}

	// Token uzunluk kontrolü — çok uzun tokenlar reddedilir
	if len(authMsg.Token) > MaxTokenLen {
		data, _ := json.Marshal(OutgoingAuth{Type: "auth", Success: false})
		ws.WriteMessage(websocket.TextMessage, data)
		return
	}

	userID := Func_User_ID(r, authMsg.Token)
	if userID == 0 {
		data, _ := json.Marshal(OutgoingAuth{Type: "auth", Success: false})
		ws.WriteMessage(websocket.TextMessage, data)
		return
	}

	// Bağlantıyı memdb'ye kaydet
	conn := &Connection{
		ID:         connID(ws),
		WS:         ws,
		UserID:     userID,
		GroupID:    0,
		ChannelID:  0,
		Status:     StatusOnline,
		StatusText: "",
	}
	insertConn(conn)

	// Auth başarılı bildirimi
	sendJSON(conn, OutgoingAuth{
		Type:    "auth",
		Success: true,
		UserID:  userID,
	})

	// ── Ping ticker — bağlantıyı canlı tut ──────────────────────

	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Println("ping goroutine panic recovered:", r)
			}
		}()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				c, ok := getConn(ws)
				if !ok {
					return
				}
				if err := c.safeWrite(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()
	defer close(done)

	// ── 2. Mesaj döngüsü ─────────────────────────────────────────

	for {
		_, rawMsg, err := ws.ReadMessage()
		if err != nil {
			break
		}

		var msg IncomingMessage
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}

		// Her mesajda güncel conn'u al (güncellenmiş olabilir)
		conn, ok := getConn(ws)
		if !ok {
			break
		}

		switch msg.Type {
		case "join":
			handleJoin(ws, conn, msg)
		case "status":
			handleStatus(ws, conn, msg)
		}
	}
}

// ── User Identification (example.go ile aynı) ───────────────────

func Func_User_IP_Address(r *http.Request) string {
	headers := []string{"CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP"}
	for _, h := range headers {
		ips := r.Header.Get(h)
		if ips != "" {
			return strings.TrimSpace(strings.Split(ips, ",")[0])
		}
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func Func_User_New_Negative_Hash_Number(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	return -int(h.Sum32() & 0x7FFFFFFF)
}

func Func_User_ID(r *http.Request, token string) int {
	// Test modunda auto-increment ID ver, DB sorgusu atla
	if testMode {
		return int(testUserSeq.Add(1))
	}

	var user_id int
	if token != "" {
		db.QueryRow("SELECT `user_id` FROM `session` WHERE `token` = ? AND `expire` > UNIX_TIMESTAMP()", token).Scan(&user_id)
	}
	if user_id == 0 {
		user_id = Func_User_New_Negative_Hash_Number(Func_User_IP_Address(r))
	}
	return user_id
}

// ── Helpers ──────────────────────────────────────────────────────

func ToInt(number string) int {
	i, err := strconv.Atoi(number)
	if err != nil {
		return 0
	}
	return i
}

func ToString(number int) string {
	return strconv.Itoa(number)
}

func ToJson(data interface{}) string {
	bytes, err := json.Marshal(data)
	if err != nil {
		return "{}"
	}
	return string(bytes)
}

// argument — komut satırı argümanlarını oku (example.go ile aynı format)
// Kullanım: go run main.go port=8080 env=test
func argument(a string) string {
	response := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, a+"=") {
			response = strings.Split(arg, "=")[1]
			response = strings.Trim(response, "\"")
		}
	}
	return response
}

// ── Main ─────────────────────────────────────────────────────────

func main() {
	// go-memdb oluştur
	schema := createSchema()
	var err error
	mdb, err = memdb.NewMemDB(schema)
	if err != nil {
		log.Fatal("memdb init error:", err)
	}

	// Test modu kontrolü (example.go ile aynı)
	testMode = argument("env") == "test"
	if testMode {
		log.Println("⚠ TEST MODE — auth bypass aktif")
	}

	// MySQL bağlantısı (test modunda atla)
	if !testMode {
		//db, err = sql.Open("mysql", "master:master@tcp(127.0.0.1:3306)/db")
		db, err = sql.Open(
			"mysql",
			"master:master@unix(/run/mysqld/mysqld.sock)/db?parseTime=true",
		)
		if err != nil {
			log.Fatal(err)
		}
		db.SetMaxOpenConns(50)
		db.SetMaxIdleConns(50)
		db.SetConnMaxLifetime(5 * time.Minute)
		defer db.Close()
	}

	// HTTP handler'ları
	http.HandleFunc("/!onlines", onlineHandler)

	port := argument("port")
	if port == "" {
		port = "8085"
	}
	log.Println("!onlines server started at :" + port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
