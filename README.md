# !onlines — Online Presence WebSocket Sunucusu

Yüksek performanslı Go WebSocket sunucusu: interest-based routing, event batching, partial presence, friend durumları.

## Çalıştırma

```bash
go run main.go port=8085              # production
go run main.go port=8085 env=test     # test modu (MySQL bypass)
```

---

## Client Kullanım Kılavuzu

### 1. Bağlantı

WebSocket bağlantısı aç:

```
ws://sunucu:8085/!onlines
```

### 2. Auth (zorunlu, ilk mesaj)

Bağlandıktan sonra **ilk mesaj** olarak token gönder:

```json
{"type": "auth", "token": "kullanici-session-tokeni"}
```

Sunucu cevabı:

```json
{"type": "auth", "success": true, "user_id": 42}
```

Başarısızsa `success: false` döner ve bağlantı kapanır.

Auth başarılı olunca otomatik olarak arkadaş durumları gelir (sadece durum bilgisi, kanal bilgisi yok):

```json
{"type": "friends", "friends": {"7": {"s": "online", "t": ""}, "12": {"s": "idle", "t": "brb"}}}
```

> Sadece sokete bağlı ve offline olmayan arkadaşlar listelenir.

### 3. Kanala Giriş

Bir grubun kanalına girmek için:

```json
{"type": "join", "group_id": 5, "channel_id": 12}
```

Sunucudan gelen cevaplar:

```json
// Sana — sadece gruba ilk katılımda gelir (kanal değişikliğinde gelmez)
{"type": "presence", "group_id": 5, "channels": {"12": [7, 8], "15": [3]}, "statuses": {"7": {"s": "online", "t": ""}}, "has_more": false}

// Aynı gruptaki herkese — senin kanala girdiğini bildirir
{"type": "channel_join", "user_id": 42, "channel_id": 12, "group_id": 5, "status": "online", "status_text": ""}
```

> **Not:** `presence` sadece **gruba ilk katılımda** gönderilir. Aynı grup içinde kanal değiştirdiğinde presence tekrar gönderilmez.

> **Not:** `channel_join` ve `channel_leave` event'leri aynı **gruptaki herkese** gider — sadece aynı kanaldakilere değil. Bu sayede herhangi bir kanalda olmayan biri de gruptaki hareketleri görür.

**Kanal değiştirmek:** Yeni bir `join` gönder. Eski kanaldan otomatik çıkış olur ve gruptaki herkese `channel_leave` gider.

**Rate limit:** Join'ler arası minimum 200ms. Daha hızlı göndersen yoksayılır.

### 4. Durum Güncelleme

```json
{"type": "status", "status": "idle", "status_text": "Birazdan dönerim"}
```

Geçerli durum değerleri:

| Değer | Anlam |
|-------|-------|
| `online` | Çevrimiçi |
| `offline` | Çevrimdışı (presence'da gizlenir) |
| `idle` | Boşta |
| `dnd` | Rahatsız etmeyin |

`status_text` isteğe bağlıdır (boş olabilir, max 128 karakter).

Durum değişikliği **aynı gruptakilere** ve **arkadaşlarına** bildirilir:

```json
{"type": "status_change", "user_id": 42, "status": "idle", "status_text": "Birazdan dönerim"}
```

### 5. Bağlantı Kopması

Bağlantı kapandığında sunucu otomatik olarak:
- Kullanıcıyı kanaldan çıkarır
- Aynı kullanıcının o kanalda başka bağlantısı yoksa → **gruptaki herkese** `channel_leave` gönderir
- Birden fazla bağlantısı varsa (farklı sekmeler/cihazlar) diğer bağlantılar etkilenmez

### 6. Toplu Mesajlar (Batch)

Sunucu 50ms aralıklarla biriken event'leri toplu gönderebilir:

```json
{"type": "batch", "events": [
  {"type": "channel_join", "user_id": 7, ...},
  {"type": "status_change", "user_id": 8, ...}
]}
```

Client'ta batch mesajları unpack edip her event'i ayrı işlemelisin.

---

## Sunucudan Gelen Mesaj Tipleri

| type | Ne zaman gelir | Kime gider |
|------|---------------|-----------|
| `auth` | Bağlantı anında | Bağlanan kişiye |
| `friends` | Auth sonrası (otomatik) | Bağlanan kişiye (sadece durum bilgisi) |
| `presence` | Gruba ilk katılınca | Giren kişiye |
| `channel_join` | Birisi kanala girince | **Gruptaki herkese** |
| `channel_leave` | Birisi kanaldan çıkınca | **Gruptaki herkese** |
| `status_change` | Durum değişince | **Gruptakiler + arkadaşlar** |
| `batch` | 50ms'de biriken çoklu event | İlgili kişilere |

## Çevrimdışı Kuralı

`status = "offline"` olan kullanıcılar:
- Presence haritasında **gösterilmez**
- Arkadaş listesinde **gösterilmez**
- Ama hâlâ kanaldaysa `channel_leave/join` bildirimleri yine gider

---

## JavaScript Client Örneği

```javascript
const ws = new WebSocket("ws://localhost:8085/!onlines");

ws.onopen = () => {
  // 1. Auth
  ws.send(JSON.stringify({ type: "auth", token: "session-token" }));
};

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);

  // Batch mesajları unpack et
  if (msg.type === "batch") {
    msg.events.forEach(evt => handleMessage(evt));
    return;
  }
  handleMessage(msg);
};

function handleMessage(msg) {
  switch (msg.type) {
    case "auth":
      if (msg.success) {
        console.log("Auth OK, userID:", msg.user_id);
        // 2. Kanala gir
        ws.send(JSON.stringify({
          type: "join",
          group_id: 5,
          channel_id: 12
        }));
      }
      break;

    case "friends":
      // Arkadaşların çevrimiçi durumları
      console.log("Online arkadaşlar:", msg.friends);
      break;

    case "presence":
      // Kanaldaki mevcut kullanıcılar
      console.log("Kanalda:", msg.channels, "Durumlar:", msg.statuses);
      if (msg.has_more) console.log("Daha fazla var...");
      break;

    case "channel_join":
      console.log(`${msg.user_id} kanala girdi`);
      break;

    case "channel_leave":
      console.log(`${msg.user_id} kanaldan çıktı`);
      break;

    case "status_change":
      console.log(`${msg.user_id} durumu: ${msg.status}`);
      break;
  }
}

// 3. Durum güncelle
function setStatus(status, text) {
  ws.send(JSON.stringify({
    type: "status",
    status: status,         // "online" | "offline" | "idle" | "dnd"
    status_text: text || ""
  }));
}

// 4. Kanal değiştir
function joinChannel(groupId, channelId) {
  ws.send(JSON.stringify({
    type: "join",
    group_id: groupId,
    channel_id: channelId
  }));
}
```

---

## Mimari

```
event geldi
   ↓
visible set (channel-scoped, O(1))
   ↓
dedup (channel ∪ friends)
   ↓
batch queue (50ms buffer)
   ↓
writePump goroutine (async)
```

## Güvenlik

| Önlem | Değer |
|-------|-------|
| Token limit | 512 char |
| StatusText limit | 128 char |
| Mesaj boyutu | 4096 byte |
| Negatif ID | Reddedilir |
| Join rate limit | 200ms |
| Slow client | Queue dolu → disconnect |
| Write timeout | 10s |

## Stres Testi

```bash
cd test && npm install
npm test           # 500 bağlantı
npm run test:1k    # 1.000
npm run test:5k    # 5.000
npm run test:10k   # 10.000
```

## Dosyalar

```
online/
├── main.go         # Sunucu
├── go.mod/go.sum
└── test/
    ├── package.json
    └── stress.js
```
