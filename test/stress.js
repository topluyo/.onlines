/**
 * !onlines WebSocket Stres Testi
 *
 * Kullanım:
 *   node stress.js [--url=ws://...] [--connections=N] [--batch=N] [--delay=ms] [--duration=s]
 *
 * Varsayılan:
 *   url          = ws://localhost:8085/!onlines
 *   connections  = 500
 *   batch        = 50
 *   delay        = 100   (batch arası ms)
 *   duration     = 10    (saniye)
 */

const WebSocket = require("ws");

// ── Argüman Ayrıştırma ───────────────────────────────────────────

function parseArgs() {
  const defaults = {
    url: "ws://localhost:8085/!onlines",
    connections: 500,
    batch: 50,
    delay: 100,
    duration: 10,
  };
  for (const arg of process.argv.slice(2)) {
    const [key, val] = arg.replace(/^--/, "").split("=");
    if (key in defaults) {
      defaults[key] = isNaN(Number(val)) ? val : Number(val);
    }
  }
  return defaults;
}

const cfg = parseArgs();

// ── İstatistikler ────────────────────────────────────────────────

const stats = {
  attempted: 0,
  connected: 0,
  authed: 0,
  failed: 0,
  messagesSent: 0,
  messagesReceived: 0,
  batchMessagesReceived: 0,
  eventsInBatches: 0,
  presenceReceived: 0,
  joinReceived: 0,
  leaveReceived: 0,
  statusChangeReceived: 0,
  connectTimes: [],
  errors: {},
};

function addError(msg) {
  const key = msg.substring(0, 80);
  stats.errors[key] = (stats.errors[key] || 0) + 1;
}

// ── Bağlantı Fabrikası ──────────────────────────────────────────

const allSockets = [];

function createConnection(index) {
  return new Promise((resolve) => {
    stats.attempted++;
    const start = Date.now();

    const groupId = (index % 10) + 1;   // 1-10 arası gruplar
    const channelId = (index % 5) + 1;  // 1-5 arası kanallar

    let ws;
    try {
      ws = new WebSocket(cfg.url);
    } catch (e) {
      stats.failed++;
      addError(e.message);
      resolve(null);
      return;
    }

    const timeout = setTimeout(() => {
      stats.failed++;
      addError("Connection timeout");
      ws.terminate();
      resolve(null);
    }, 10000);

    ws.on("open", () => {
      stats.connected++;
      stats.connectTimes.push(Date.now() - start);
      clearTimeout(timeout);

      // 1. Auth gönder
      const authMsg = JSON.stringify({
        type: "auth",
        token: `test-token-${index}`,
      });
      ws.send(authMsg);
      stats.messagesSent++;
    });

    // Event işleyici (batch destekli)
    function handleEvent(msg) {
      switch (msg.type) {
        case "auth":
          if (msg.success) {
            stats.authed++;
            const joinMsg = JSON.stringify({
              type: "join",
              group_id: groupId,
              channel_id: channelId,
            });
            ws.send(joinMsg);
            stats.messagesSent++;

            if (index % 10 === 0) {
              setTimeout(() => {
                if (ws.readyState === WebSocket.OPEN) {
                  ws.send(JSON.stringify({
                    type: "status",
                    status: "idle",
                    status_text: `Bot #${index}`,
                  }));
                  stats.messagesSent++;
                }
              }, 500);
            }

            if (index % 20 === 0) {
              setTimeout(() => {
                if (ws.readyState === WebSocket.OPEN) {
                  ws.send(JSON.stringify({
                    type: "join",
                    group_id: groupId,
                    channel_id: ((channelId % 5) + 1),
                  }));
                  stats.messagesSent++;
                }
              }, 1000);
            }
          } else {
            stats.failed++;
            addError("Auth failed");
          }
          break;
        case "batch":
          stats.batchMessagesReceived++;
          if (Array.isArray(msg.events)) {
            stats.eventsInBatches += msg.events.length;
            for (const evt of msg.events) {
              handleEvent(typeof evt === "string" ? JSON.parse(evt) : evt);
            }
          }
          break;
        case "presence":
          stats.presenceReceived++;
          break;
        case "channel_join":
          stats.joinReceived++;
          break;
        case "channel_leave":
          stats.leaveReceived++;
          break;
        case "status_change":
          stats.statusChangeReceived++;
          break;
      }
    }

    ws.on("message", (data) => {
      stats.messagesReceived++;
      try {
        handleEvent(JSON.parse(data.toString()));
      } catch (e) {
        // JSON parse hatası
      }
    });

    ws.on("error", (e) => {
      clearTimeout(timeout);
      stats.failed++;
      addError(e.message || "Unknown error");
    });

    ws.on("close", () => {
      clearTimeout(timeout);
    });

    allSockets.push(ws);
    resolve(ws);
  });
}

// ── Batch Bağlantı ──────────────────────────────────────────────

async function connectBatch(startIdx, count) {
  const promises = [];
  for (let i = 0; i < count; i++) {
    promises.push(createConnection(startIdx + i));
  }
  return Promise.all(promises);
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// ── Rapor ────────────────────────────────────────────────────────

function printReport(elapsed) {
  const times = stats.connectTimes.sort((a, b) => a - b);
  const avg = times.length ? (times.reduce((a, b) => a + b, 0) / times.length).toFixed(1) : 0;
  const min = times.length ? times[0] : 0;
  const max = times.length ? times[times.length - 1] : 0;
  const p95 = times.length ? times[Math.floor(times.length * 0.95)] : 0;

  console.log("\n" + "═".repeat(60));
  console.log("  📊 !onlines STRES TESTİ RAPORU");
  console.log("═".repeat(60));
  console.log(`\n  ⏱  Toplam süre: ${(elapsed / 1000).toFixed(1)}s`);
  console.log(`\n  🔌 Bağlantılar:`);
  console.log(`     Denenen:   ${stats.attempted}`);
  console.log(`     Bağlanan:  ${stats.connected}  (${((stats.connected / stats.attempted) * 100).toFixed(1)}%)`);
  console.log(`     Auth olan: ${stats.authed}  (${((stats.authed / stats.attempted) * 100).toFixed(1)}%)`);
  console.log(`     Başarısız: ${stats.failed}  (${((stats.failed / stats.attempted) * 100).toFixed(1)}%)`);
  console.log(`\n  ⏳ Bağlantı süreleri:`);
  console.log(`     Ortalama:  ${avg}ms`);
  console.log(`     Minimum:   ${min}ms`);
  console.log(`     Maksimum:  ${max}ms`);
  console.log(`     P95:       ${p95}ms`);
  console.log(`\n  📨 Mesajlar:`);
  console.log(`     Gönderilen:         ${stats.messagesSent}`);
  console.log(`     Alınan (toplam):    ${stats.messagesReceived}`);
  console.log(`     ├─ batch mesaj:     ${stats.batchMessagesReceived}  (${stats.eventsInBatches} event)`);
  console.log(`     ├─ presence:        ${stats.presenceReceived}`);
  console.log(`     ├─ channel_join:    ${stats.joinReceived}`);
  console.log(`     ├─ channel_leave:   ${stats.leaveReceived}`);
  console.log(`     └─ status_change:   ${stats.statusChangeReceived}`);

  if (Object.keys(stats.errors).length > 0) {
    console.log(`\n  ❌ Hatalar:`);
    for (const [msg, count] of Object.entries(stats.errors)) {
      console.log(`     ${count}x  ${msg}`);
    }
  }

  console.log("\n" + "═".repeat(60));

  // Özet sonuç
  const successRate = (stats.authed / stats.attempted) * 100;
  if (successRate >= 95) {
    console.log("  ✅ BAŞARILI — %95+ auth başarısı");
  } else if (successRate >= 80) {
    console.log("  ⚠️  KISMEN BAŞARILI — %80-95 auth başarısı");
  } else {
    console.log("  ❌ BAŞARISIZ — <%80 auth başarısı");
  }
  console.log("═".repeat(60) + "\n");
}

// ── Ana Akış ─────────────────────────────────────────────────────

async function main() {
  console.log("═".repeat(60));
  console.log("  🚀 !onlines Stres Testi Başlıyor");
  console.log("═".repeat(60));
  console.log(`  URL:          ${cfg.url}`);
  console.log(`  Bağlantı:     ${cfg.connections}`);
  console.log(`  Batch:        ${cfg.batch}`);
  console.log(`  Delay:        ${cfg.delay}ms`);
  console.log(`  Süre:         ${cfg.duration}s`);
  console.log("═".repeat(60) + "\n");

  const startTime = Date.now();

  // Bağlantıları batch'ler halinde aç
  let created = 0;
  while (created < cfg.connections) {
    const batchSize = Math.min(cfg.batch, cfg.connections - created);
    process.stdout.write(
      `  [${created + batchSize}/${cfg.connections}] Bağlanıyor...  \r`
    );
    await connectBatch(created, batchSize);
    created += batchSize;
    if (created < cfg.connections) {
      await sleep(cfg.delay);
    }
  }

  console.log(`\n  ✅ ${stats.connected} bağlantı açıldı, ${cfg.duration}s beklenecek...\n`);

  // Bağlantıları belirtilen süre boyunca açık tut
  let remaining = cfg.duration;
  while (remaining > 0) {
    process.stdout.write(`  ⏳ Kalan: ${remaining}s  \r`);
    await sleep(1000);
    remaining--;
  }

  // Tüm bağlantıları kapat
  console.log("\n  🔒 Bağlantılar kapatılıyor...");
  for (const ws of allSockets) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.close();
    }
  }
  await sleep(1000); // kapanma bekle

  const elapsed = Date.now() - startTime;
  printReport(elapsed);

  process.exit(0);
}

main().catch((e) => {
  console.error("Fatal error:", e);
  process.exit(1);
});
