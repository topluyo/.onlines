package main

import (
	"database/sql"
	"encoding/json"
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
)

// ── Sabitler ─────────────────────────────────────────────────────

const (
	StatusOnline  = "online"
	StatusOffline = "offline"
	StatusIdle    = "idle"
	StatusDND     = "dnd"

	MaxStatusTextLen   = 128
	MaxTokenLen        = 512
	MaxReadSize        = 4096
	WriteQueueSize     = 256
	WriteTimeout       = 10 * time.Second
	PingInterval       = 30 * time.Second
	PongWait           = 60 * time.Second
	BatchInterval      = 50 * time.Millisecond // event batching aralığı
	MaxPresenceUsers   = 1000                   // initial presence'da max user sayısı
	JoinRateLimitMs    = 200                    // aynı user min join aralığı
)

// ── JSON Mesaj Tipleri ───────────────────────────────────────────

// Client → Server
type IncomingMessage struct {
	Type       string `json:"type"`
	Token      string `json:"token,omitempty"`
	GroupID    int    `json:"group_id,omitempty"`
	ChannelID  int    `json:"channel_id,omitempty"`
	Status     string `json:"status,omitempty"`
	StatusText string `json:"status_text,omitempty"`
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

type UserStatusInfo struct {
	S string `json:"s"`
	T string `json:"t"`
}

type OutgoingPresence struct {
	Type     string                    `json:"type"`
	GroupID  int                       `json:"group_id"`
	Channels map[string][]int          `json:"channels"`
	Statuses map[string]UserStatusInfo `json:"statuses"`
	HasMore  bool                     `json:"has_more,omitempty"` // lazy load göstergesi
}

type OutgoingStatusChange struct {
	Type       string `json:"type"`
	UserID     int    `json:"user_id"`
	Status     string `json:"status"`
	StatusText string `json:"status_text"`
}

// Batched events - toplu event mesajı
type OutgoingBatch struct {
	Type   string            `json:"type"`    // "batch"
	Events []json.RawMessage `json:"events"`
}

// Arkadaş durumları — auth sonrası gönderilir
type OutgoingFriendStatuses struct {
	Type     string                    `json:"type"`     // "friends"
	Friends  map[string]UserStatusInfo `json:"friends"`  // userID → {s, t}
}

// ── Connection ───────────────────────────────────────────────────

type Connection struct {
	WS         *websocket.Conn
	UserID     int
	GroupID    int
	ChannelID  int
	Status     string
	StatusText string
	send       chan []byte
	done       chan struct{}
	lastJoin   time.Time // join rate limit
}

// Send — non-blocking push. Bağlantı kapalıysa yoksay, queue doluysa disconnect.
func (c *Connection) Send(msg []byte) {
	select {
	case <-c.done:
		return // bağlantı kapanmış
	default:
	}
	select {
	case c.send <- msg:
	case <-c.done:
		return
	default:
		c.Close() // queue dolu → slow client
	}
}

// SendJSON — serialize + queue push
func (c *Connection) SendJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.Send(data)
}

// Close — idempotent kapatma
func (c *Connection) Close() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// writePump — dedicated async writer + ping + batch drain
func (c *Connection) writePump() {
	ticker := time.NewTicker(PingInterval)
	defer func() {
		ticker.Stop()
		c.WS.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				c.WS.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			c.WS.SetWriteDeadline(time.Now().Add(WriteTimeout))
			if err := c.WS.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
			// Batch drain — queue'daki kalan mesajları hemen gönder
			n := len(c.send)
			for i := 0; i < n; i++ {
				next, ok := <-c.send
				if !ok {
					return
				}
				if err := c.WS.WriteMessage(websocket.TextMessage, next); err != nil {
					return
				}
			}
		case <-ticker.C:
			c.WS.SetWriteDeadline(time.Now().Add(WriteTimeout))
			if err := c.WS.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// ── channelKey — compound key "groupID:channelID" ────────────────

type channelKey struct {
	GroupID   int
	ChannelID int
}

// ── Hub — Merkezi Bağlantı Yöneticisi ───────────────────────────

type Hub struct {
	mu          sync.RWMutex
	connections map[*websocket.Conn]*Connection
	users       map[int][]*Connection
	groups      map[int][]*Connection
	channels    map[channelKey][]*Connection    // ← visible set: channel bazlı
	friends     map[int][]int

	// Event batching
	batchMu     sync.Mutex
	batchQueues map[int][]json.RawMessage       // userID → pending events
	batchTimer  *time.Ticker
}

var hub *Hub

func newHub() *Hub {
	h := &Hub{
		connections: make(map[*websocket.Conn]*Connection),
		users:       make(map[int][]*Connection),
		groups:      make(map[int][]*Connection),
		channels:    make(map[channelKey][]*Connection),
		friends:     make(map[int][]int),
		batchQueues: make(map[int][]json.RawMessage),
	}
	// Batch flush ticker başlat
	h.batchTimer = time.NewTicker(BatchInterval)
	go h.batchFlusher()
	return h
}

// ── Batch Event System ─────────────────────────────────────────

// queueEventBulk — birden fazla kullanıcıya aynı event'i tek lock ile ekle
func (h *Hub) queueEventBulk(userIDs []int, event []byte) {
	raw := json.RawMessage(event)
	h.batchMu.Lock()
	for _, uid := range userIDs {
		h.batchQueues[uid] = append(h.batchQueues[uid], raw)
	}
	h.batchMu.Unlock()
}

// queueEventSingle — tek kullanıcıya event ekle
func (h *Hub) queueEventSingle(userID int, event []byte) {
	h.batchMu.Lock()
	h.batchQueues[userID] = append(h.batchQueues[userID], json.RawMessage(event))
	h.batchMu.Unlock()
}

// broadcastToGroup — gruptaki herkese event (dedup + bulk queue)
// mu.RLock veya mu.Lock altında çağrılabilir (batchMu ayrı lock)
func (h *Hub) broadcastToGroup(groupID int, excludeUserID int, event []byte) {
	targets := collectGroupTargets(h.groups[groupID], excludeUserID)
	if len(targets) > 0 {
		h.queueEventBulk(targets, event)
	}
}

// batchFlusher — periyodik flush, kısa lock süreleri
func (h *Hub) batchFlusher() {
	defer func() {
		if r := recover(); r != nil {
			log.Println("batchFlusher panic:", r)
			go h.batchFlusher() // yeniden başlat
		}
	}()

	for range h.batchTimer.C {
		// 1. Batch queue snapshot (kısa lock)
		h.batchMu.Lock()
		if len(h.batchQueues) == 0 {
			h.batchMu.Unlock()
			continue
		}
		queues := h.batchQueues
		h.batchQueues = make(map[int][]json.RawMessage, len(queues))
		h.batchMu.Unlock()

		// 2. User→conns snapshot (kısa RLock, sadece pointer kopyalama)
		type delivery struct {
			conns  []*Connection
			events []json.RawMessage
		}
		deliveries := make([]delivery, 0, len(queues))

		h.mu.RLock()
		for userID, events := range queues {
			conns := h.users[userID]
			if len(conns) == 0 {
				continue
			}
			cc := make([]*Connection, len(conns))
			copy(cc, conns)
			deliveries = append(deliveries, delivery{conns: cc, events: events})
		}
		h.mu.RUnlock()

		// 3. Gönderim (hiçbir lock tutmadan)
		for _, d := range deliveries {
			var msg []byte
			if len(d.events) == 1 {
				msg = d.events[0]
			} else {
				msg, _ = json.Marshal(OutgoingBatch{
					Type:   "batch",
					Events: d.events,
				})
			}
			for _, c := range d.conns {
				c.Send(msg)
			}
		}
	}
}

// ── Hub CRUD ─────────────────────────────────────────────────────

func (h *Hub) register(c *Connection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connections[c.WS] = c
	h.users[c.UserID] = append(h.users[c.UserID], c)
}

func (h *Hub) unregister(c *Connection) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.connections, c.WS)

	// users
	if conns, ok := h.users[c.UserID]; ok {
		h.users[c.UserID] = removeConn(conns, c)
		if len(h.users[c.UserID]) == 0 {
			delete(h.users, c.UserID)
			delete(h.friends, c.UserID) // son bağlantı → friend cache temizle
		}
	}

	// groups
	if c.GroupID != 0 {
		if conns, ok := h.groups[c.GroupID]; ok {
			h.groups[c.GroupID] = removeConn(conns, c)
			if len(h.groups[c.GroupID]) == 0 {
				delete(h.groups, c.GroupID)
			}
		}
	}

	// channels (visible set)
	if c.GroupID != 0 && c.ChannelID != 0 {
		ck := channelKey{c.GroupID, c.ChannelID}
		if conns, ok := h.channels[ck]; ok {
			h.channels[ck] = removeConn(conns, c)
			if len(h.channels[ck]) == 0 {
				delete(h.channels, ck)
			}
		}
	}

	c.Close() // done kapat → writePump durur → ws kapanır
}

// ── Interest-Based Routing ───────────────────────────────────────

// joinGroup — group-scoped broadcast + partial presence
// Lock stratejisi: mu.Lock sadece veri mutasyonu için, broadcast lock dışında
func (h *Hub) joinGroup(c *Connection, groupID, channelID int) {
	// Rate limit kontrolü
	now := time.Now()
	if now.Sub(c.lastJoin) < time.Duration(JoinRateLimitMs)*time.Millisecond {
		return
	}
	c.lastJoin = now

	// Broadcast için toplanacak bilgiler
	var leaveTargets []int
	var leaveMsg []byte
	var joinTargets []int
	var joinMsg []byte
	var presence *OutgoingPresence

	h.mu.Lock()

	oldGroupID := c.GroupID
	oldChannelID := c.ChannelID

	// ── 1. Eski channel'dan çıkar ────────────────────────────────

	if oldGroupID != 0 && oldChannelID != 0 {
		oldCK := channelKey{oldGroupID, oldChannelID}
		if conns, ok := h.channels[oldCK]; ok {
			h.channels[oldCK] = removeConn(conns, c)
			if len(h.channels[oldCK]) == 0 {
				delete(h.channels, oldCK)
			}
		}

		// Leave hedeflerini topla (broadcast lock dışında yapılacak)
		leaveMsg, _ = json.Marshal(OutgoingChannelLeave{
			Type:      "channel_leave",
			UserID:    c.UserID,
			ChannelID: oldChannelID,
			GroupID:   oldGroupID,
		})
		leaveTargets = collectGroupTargets(h.groups[oldGroupID], c.UserID)
	}

	// Eski gruptan çıkar
	if oldGroupID != 0 && oldGroupID != groupID {
		if conns, ok := h.groups[oldGroupID]; ok {
			h.groups[oldGroupID] = removeConn(conns, c)
			if len(h.groups[oldGroupID]) == 0 {
				delete(h.groups, oldGroupID)
			}
		}
	}

	// ── 2. Yeni channel'a ekle ───────────────────────────────────

	c.GroupID = groupID
	c.ChannelID = channelID

	if groupID != 0 {
		if oldGroupID != groupID {
			h.groups[groupID] = append(h.groups[groupID], c)
		}

		if channelID != 0 {
			newCK := channelKey{groupID, channelID}
			h.channels[newCK] = append(h.channels[newCK], c)

			// Join hedeflerini topla
			joinMsg, _ = json.Marshal(OutgoingChannelJoin{
				Type:       "channel_join",
				UserID:     c.UserID,
				ChannelID:  channelID,
				GroupID:    groupID,
				Status:     c.Status,
				StatusText: c.StatusText,
			})
			joinTargets = collectGroupTargets(h.groups[groupID], c.UserID)
		}

		// Presence sadece yeni gruba katılınca gönder (kanal değişikliğinde gönderme)
		if oldGroupID != groupID {
			presence = h.buildPartialPresenceLocked(groupID, channelID)
		}
	}

	// ── mu.Unlock — TÜM broadcast işlemleri lock dışında ─────────
	h.mu.Unlock()

	// Broadcast (lock tutmadan)
	if len(leaveTargets) > 0 {
		h.queueEventBulk(leaveTargets, leaveMsg)
	}
	if len(joinTargets) > 0 {
		h.queueEventBulk(joinTargets, joinMsg)
	}
	if presence != nil {
		c.SendJSON(presence)
	}
}

// collectGroupTargets — gruptaki unique userID'leri topla (exclude hariç)
// mu.Lock altında çağrılmalı
func collectGroupTargets(groupConns []*Connection, excludeUserID int) []int {
	seen := make(map[int]bool)
	if excludeUserID != 0 {
		seen[excludeUserID] = true
	}
	var targets []int
	for _, gc := range groupConns {
		if !seen[gc.UserID] {
			seen[gc.UserID] = true
			targets = append(targets, gc.UserID)
		}
	}
	return targets
}

// updateStatus — group-scoped + friend broadcast (dedup + bulk)
func (h *Hub) updateStatus(c *Connection, status, statusText string) {
	h.mu.Lock()

	c.Status = status
	c.StatusText = statusText

	statusMsg, _ := json.Marshal(OutgoingStatusChange{
		Type:       "status_change",
		UserID:     c.UserID,
		Status:     status,
		StatusText: statusText,
	})

	// Hedef: gruptakiler + arkadaşlar (dedup)
	seen := make(map[int]bool)
	seen[c.UserID] = true
	var targets []int

	// 1. Gruptaki herkes
	if c.GroupID != 0 {
		for _, gc := range h.groups[c.GroupID] {
			if !seen[gc.UserID] {
				seen[gc.UserID] = true
				targets = append(targets, gc.UserID)
			}
		}
	}

	// 2. Arkadaşlar (grup dışındakiler de)
	if fids, ok := h.friends[c.UserID]; ok {
		for _, fid := range fids {
			if !seen[fid] {
				seen[fid] = true
				targets = append(targets, fid)
			}
		}
	}

	h.mu.Unlock()

	if len(targets) > 0 {
		h.queueEventBulk(targets, statusMsg)
	}
}

// handleDisconnect — temizlik + group-scoped leave bildirimi
// Lock stratejisi: RLock sadece target toplama, broadcast lock dışında
func (h *Hub) handleDisconnect(c *Connection) {
	groupID := c.GroupID
	channelID := c.ChannelID
	userID := c.UserID

	h.unregister(c)
	c.Close()

	if groupID != 0 && channelID != 0 {
		var leaveTargets []int

		h.mu.RLock()
		ck := channelKey{groupID, channelID}
		stillInChannel := false
		for _, gc := range h.channels[ck] {
			if gc.UserID == userID {
				stillInChannel = true
				break
			}
		}
		if !stillInChannel {
			leaveTargets = collectGroupTargets(h.groups[groupID], 0)
		}
		h.mu.RUnlock()

		// Broadcast lock dışında
		if len(leaveTargets) > 0 {
			leaveMsg, _ := json.Marshal(OutgoingChannelLeave{
				Type:      "channel_leave",
				UserID:    userID,
				ChannelID: channelID,
				GroupID:   groupID,
			})
			h.queueEventBulk(leaveTargets, leaveMsg)
		}
	}
}

func (h *Hub) setFriends(userID int, friendIDs []int) {
	h.mu.Lock()
	h.friends[userID] = friendIDs
	h.mu.Unlock()
}

// getOnlineFriendStatuses — arkadaşların çevrimiçi durumlarını döndür (offline hariç)
// Birden fazla bağlantı varsa en iyi durumu seç (online > idle > dnd)
func (h *Hub) getOnlineFriendStatuses(userID int) map[string]UserStatusInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	fids, ok := h.friends[userID]
	if !ok || len(fids) == 0 {
		return nil
	}

	result := make(map[string]UserStatusInfo)
	for _, fid := range fids {
		conns, ok := h.users[fid]
		if !ok || len(conns) == 0 {
			continue
		}
		// Tüm bağlantılardan en iyi durumu seç
		best := conns[0]
		for _, c := range conns[1:] {
			if statusPriority(c.Status) > statusPriority(best.Status) {
				best = c
			}
		}
		if best.Status == StatusOffline {
			continue
		}
		result[strconv.Itoa(fid)] = UserStatusInfo{S: best.Status, T: best.StatusText}
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// statusPriority — durum öncelik sırası (yüksek = daha görünür)
func statusPriority(s string) int {
	switch s {
	case StatusOnline:
		return 4
	case StatusIdle:
		return 3
	case StatusDND:
		return 2
	case StatusOffline:
		return 1
	default:
		return 0
	}
}

// ── Hub Internal ─────────────────────────────────────────────────

// queueEventLocked — batch queue'ya event ekle (batchMu ile)
func (h *Hub) queueEventLocked(userID int, event []byte) {
	h.batchMu.Lock()
	h.batchQueues[userID] = append(h.batchQueues[userID], json.RawMessage(event))
	h.batchMu.Unlock()
}

// buildPartialPresenceLocked — grup içindeki presence (lock altında)
// Aynı kullanıcı farklı kanallarda → her kanalda göster
// Aynı kullanıcı aynı kanalda → bir kez göster (dedup per channel)
func (h *Hub) buildPartialPresenceLocked(groupID, channelID int) *OutgoingPresence {
	result := &OutgoingPresence{
		Type:     "presence",
		GroupID:  groupID,
		Channels: make(map[string][]int),
		Statuses: make(map[string]UserStatusInfo),
	}

	// per-(channel, user) dedup
	type chUserKey struct {
		ch   int
		user int
	}
	seen := make(map[chUserKey]bool)
	seenStatus := make(map[int]bool) // status dedup (aynı userı farklı kanallarda göster ama status 1 kez)
	count := 0

	// Önce istenen channel
	if channelID != 0 {
		ck := channelKey{groupID, channelID}
		chStr := strconv.Itoa(channelID)
		if conns, ok := h.channels[ck]; ok {
			for _, gc := range conns {
				key := chUserKey{channelID, gc.UserID}
				if seen[key] {
					continue // aynı user aynı kanalda → dedup
				}
				if gc.Status == StatusOffline {
					continue
				}
				seen[key] = true
				result.Channels[chStr] = append(result.Channels[chStr], gc.UserID)
				if !seenStatus[gc.UserID] {
					seenStatus[gc.UserID] = true
					result.Statuses[strconv.Itoa(gc.UserID)] = UserStatusInfo{S: gc.Status, T: gc.StatusText}
					count++
				}
				if count >= MaxPresenceUsers {
					result.HasMore = true
					return result
				}
			}
		}
	}

	// Sonra gruptaki diğer channel'lar
	if conns, ok := h.groups[groupID]; ok {
		for _, gc := range conns {
			if gc.ChannelID == 0 {
				continue
			}
			key := chUserKey{gc.ChannelID, gc.UserID}
			if seen[key] {
				continue
			}
			if gc.Status == StatusOffline {
				continue
			}
			seen[key] = true
			chStr := strconv.Itoa(gc.ChannelID)
			result.Channels[chStr] = append(result.Channels[chStr], gc.UserID)
			if !seenStatus[gc.UserID] {
				seenStatus[gc.UserID] = true
				result.Statuses[strconv.Itoa(gc.UserID)] = UserStatusInfo{S: gc.Status, T: gc.StatusText}
				count++
			}
			if count >= MaxPresenceUsers {
				result.HasMore = true
				return result
			}
		}
	}

	return result
}

// removeConn — swap-remove (O(1))
func removeConn(slice []*Connection, c *Connection) []*Connection {
	for i, v := range slice {
		if v == c {
			slice[i] = slice[len(slice)-1]
			return slice[:len(slice)-1]
		}
	}
	return slice
}

// ── WebSocket Handler ────────────────────────────────────────────

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  512,
		WriteBufferSize: 512,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	db          *sql.DB
	testMode    bool
	testUserSeq atomic.Int64
)

func onlineHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	ws.SetReadLimit(MaxReadSize)
	ws.SetReadDeadline(time.Now().Add(PongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(PongWait))
		return nil
	})

	// ── 1. Auth ──────────────────────────────────────────────────

	_, rawMsg, err := ws.ReadMessage()
	if err != nil {
		ws.Close()
		return
	}

	var authMsg IncomingMessage
	if err := json.Unmarshal(rawMsg, &authMsg); err != nil || authMsg.Type != "auth" {
		data, _ := json.Marshal(OutgoingAuth{Type: "auth", Success: false})
		ws.WriteMessage(websocket.TextMessage, data)
		ws.Close()
		return
	}

	if len(authMsg.Token) > MaxTokenLen {
		data, _ := json.Marshal(OutgoingAuth{Type: "auth", Success: false})
		ws.WriteMessage(websocket.TextMessage, data)
		ws.Close()
		return
	}

	userID := Func_User_ID(r, authMsg.Token)
	if userID == 0 {
		data, _ := json.Marshal(OutgoingAuth{Type: "auth", Success: false})
		ws.WriteMessage(websocket.TextMessage, data)
		ws.Close()
		return
	}

	conn := &Connection{
		WS:         ws,
		UserID:     userID,
		Status:     StatusOnline,
		StatusText: "",
		send:       make(chan []byte, WriteQueueSize),
		done:       make(chan struct{}),
	}

	hub.register(conn)

	// Arkadaş listesini async yükle + durumlarını gönder
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Println("loadFriends panic:", r)
			}
		}()
		friends := loadFriends(userID)
		if len(friends) > 0 {
			hub.setFriends(userID, friends)

			// Arkadaşların çevrimiçi durumlarını gönder
			statuses := hub.getOnlineFriendStatuses(userID)
			if statuses != nil {
				conn.SendJSON(OutgoingFriendStatuses{
					Type:    "friends",
					Friends: statuses,
				})
			}
		}
	}()

	// Auth başarılı
	conn.SendJSON(OutgoingAuth{
		Type:    "auth",
		Success: true,
		UserID:  userID,
	})

	// Writer goroutine
	go conn.writePump()
	defer hub.handleDisconnect(conn)

	// ── 2. Read loop ─────────────────────────────────────────────

	for {
		_, rawMsg, err := ws.ReadMessage()
		if err != nil {
			break
		}

		var msg IncomingMessage
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "join":
			if msg.GroupID < 0 || msg.ChannelID < 0 {
				continue
			}
			hub.joinGroup(conn, msg.GroupID, msg.ChannelID)

		case "status":
			switch msg.Status {
			case StatusOnline, StatusOffline, StatusIdle, StatusDND:
			default:
				continue
			}
			statusText := msg.StatusText
			if len(statusText) > MaxStatusTextLen {
				statusText = statusText[:MaxStatusTextLen]
			}
			hub.updateStatus(conn, msg.Status, statusText)
		}
	}
}

// ── Friend System ────────────────────────────────────────────────

func loadFriends(userID int) []int {
	if testMode || db == nil {
		return nil
	}
	rows, err := db.Query("SELECT `friend_id` FROM `friend` WHERE `user_id` = ? AND `status` = 1", userID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var friends []int
	for rows.Next() {
		var fid int
		if err := rows.Scan(&fid); err == nil {
			friends = append(friends, fid)
		}
	}
	return friends
}

// ── User Identification ──────────────────────────────────────────

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
	hub = newHub()

	testMode = argument("env") == "test"
	if testMode {
		log.Println("⚠ TEST MODE — auth bypass aktif")
	}

	if !testMode {
		var err error
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

	http.HandleFunc("/!onlines", onlineHandler)

	port := argument("port")
	if port == "" {
		port = "8085"
	}

	server := &http.Server{
		Addr:              ":" + port,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    4096,
	}

	log.Println("!onlines server started at :" + port)
	log.Fatal(server.ListenAndServe())
}
