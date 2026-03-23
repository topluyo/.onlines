# !onlines — Online Presence WebSocket Sunucusu

Kullanıcıların hangi gruptaki hangi kanalda olduğunu ve çevrimiçi durumlarını anlık izleyen Go WebSocket sunucusu.

## Özellikler

- **go-memdb** — O(1) lookup, 6 indeksli in-memory veritabanı
- **sync.Mutex** — Concurrent write koruması
- **Ping/Pong** — 30 sn keep-alive, 60 sn read deadline
- **Otomatik temizlik** — `defer` ile bağlantı kapanırken memdb'den silinme
- **Durum yönetimi** — `online`, `offline`, `idle`, `dnd` + özel durum metni
- **Güvenlik** — Token/StatusText uzunluk sınırları, parameterized SQL, panic recovery

## Çalıştırma

```bash
# Production (MySQL gerektirir)
go run main.go port=8085

# Test modu (MySQL atlanır, auto-increment user ID)
go run main.go port=8085 env=test
```

### Argümanlar

| Argüman | Varsayılan | Açıklama |
|---------|-----------|----------|
| `port`  | `8085`    | HTTP dinleme portu |
| `env`   | —         | `test` → MySQL bypass, auth otomatik |

## WebSocket Protokolü

Endpoint: `/!onlines`

### Client → Server

```jsonc
// 1. Auth (ilk mesaj, zorunlu)
{"type": "auth", "token": "abc123"}

// 2. Kanala giriş
{"type": "join", "group_id": 5, "channel_id": 12}

// 3. Durum güncelleme
{"type": "status", "status": "idle", "status_text": "Birazdan dönerim"}
```

### Server → Client

```jsonc
// Auth sonucu
{"type": "auth", "success": true, "user_id": 42}

// Birisi kanala girdi (gruptaki herkese)
{"type": "channel_join", "user_id": 7, "channel_id": 12, "group_id": 5, "status": "online", "status_text": ""}

// Birisi kanaldan çıktı (gruptaki herkese)
{"type": "channel_leave", "user_id": 7, "channel_id": 12, "group_id": 5}

// Presence haritası (join sonrası gönderilir)
{"type": "presence", "group_id": 5, "channels": {"12": [7,8], "15": [3]}, "statuses": {"7": {"s": "online", "t": ""}, "8": {"s": "idle", "t": "brb"}}}

// Durum değişikliği (gruptaki herkese)
{"type": "status_change", "user_id": 7, "status": "idle", "status_text": "brb"}
```

## Akış Senaryoları

### Kanala Giriş (C kullanıcısı X kanalına girdiğinde)
1. C'nin memdb kaydı güncellenir (groupID, channelID)
2. Gruptaki tüm bağlantılara `channel_join` bildirimi gönderilir
3. C'ye gruptaki tüm kullanıcı konumlarını gösteren `presence` haritası gönderilir

### Kanaldan Çıkış
1. Gruptaki tüm bağlantılara `channel_leave` bildirimi gönderilir
2. memdb kaydı güncellenir

### Kanal Değiştirme
- Yeni `join` göndermek eski kanaldan otomatik `channel_leave` tetikler

### Bağlantı Kopması
- `defer handleDisconnect()` → memdb'den silinme + `channel_leave` bildirimi

### Çevrimdışı Kuralı
- `status = "offline"` olan kullanıcılar presence haritasında **gösterilmez**
- İstisna: Çevrimdışı kullanıcı bir kanalda ise sadece o kanalda gösterilir

## Güvenlik

| Önlem | Detay |
|-------|-------|
| SQL Injection | Parameterized query (`?` placeholder) |
| Token sınırı | Max 512 karakter |
| StatusText sınırı | Max 128 karakter, aşan kırpılır |
| Mesaj boyutu | Max 4096 byte (`SetReadLimit`) |
| Negatif ID | `group_id < 0` veya `channel_id < 0` reddedilir |
| Panic recovery | Goroutine'lerde `recover()` ile panic yakalanır |
| Read deadline | 60 sn timeout, pong ile yenilenir |

## memdb Schema

```
connections tablosu:
├── id             (primary, unique)     — ws pointer adresi
├── user_id        (index)               — kullanıcı bazlı sorgulama
├── group_id       (index)               — grup bazlı sorgulama
├── channel_id     (index)               — kanal bazlı sorgulama
├── channel_group  (compound index)      — "channelID,groupID"
└── user_group     (compound index)      — "userID,groupID"
```

## 🧪 Test Modu

`env=test` ile başlatıldığında:
- ✅ Auth bypass — Token doğrulama atlanır, her bağlantıya auto-increment ID
- ✅ MySQL opsiyonel — Veritabanı bağlantısı atlanır
- ✅ Atomic counter — `sync/atomic` ile thread-safe ID üretimi

## 📊 Stres Testi

`test/` dizininde Node.js tabanlı stres test aracı.

### Kurulum & Çalıştırma

```bash
cd test
npm install

# Varsayılan: 500 bağlantı
npm test

# Önceden tanımlı presetler
npm run test:1k    # 1.000 bağlantı
npm run test:5k    # 5.000 bağlantı
npm run test:10k   # 10.000 bağlantı

# Özelleştirilmiş
node stress.js --url=ws://localhost:8085/!onlines --connections=2000 --batch=100 --delay=50 --duration=20
```

### Parametreler

| Parametre | Varsayılan | Açıklama |
|-----------|-----------|----------|
| `--url` | `ws://localhost:8085/!onlines` | WebSocket adresi |
| `--connections` | `500` | Toplam bağlantı sayısı |
| `--batch` | `50` | Batch başına bağlantı |
| `--delay` | `100` | Batch arası bekleme (ms) |
| `--duration` | `10` | Bağlantıları açık tutma süresi (s) |

### Test Senaryosu

Her bağlantı:
1. WebSocket bağlantısı açar
2. Auth token gönderir (`test-token-N`)
3. Gruba ve kanala katılır (10 grup, 5 kanal)
4. Her 10. bağlantı durum günceller (`idle`)
5. Her 20. bağlantı kanal değiştirir
6. Belirtilen süre boyunca açık tutulur
7. Kapanış → Detaylı rapor yazdırılır

### Rapor İçeriği

- Başarılı/başarısız bağlantı sayısı ve oranı
- Ortalama, min, max ve P95 bağlantı süresi
- Mesaj türlerine göre sayılar (presence, join, leave, status_change)
- Gruplandırılmış hata özeti

## Dosya Yapısı

```
online/
├── main.go          # Ana sunucu
├── go.mod           # Go module
├── go.sum           # Bağımlılık hash'leri
├── example.go.bak   # Referans dosya (derlemeye dahil değil)
└── test/
    ├── package.json # Node.js bağımlılıkları
    └── stress.js    # Stres test aracı
```
