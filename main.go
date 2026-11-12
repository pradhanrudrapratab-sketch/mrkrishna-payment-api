package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	qrcode "github.com/skip2/go-qrcode"
	_ "modernc.org/sqlite"
)

type Config struct {
	port, baseURL, upiID, upiName, gmailUser, gmailPass, gmailHost, gmailPort, famPaySender, dbPath string
	qrMins, expiryMins, verifySecs                                                                  int
}
type App struct {
	cfg Config
	db  *sql.DB
	mu  sync.Mutex
}
type Payment struct {
	txn                            string
	amount                         int64
	uri, status, created, deadline string
}
type Notice struct {
	id, sender, subject, body, txn string
	amount                         int64
}

func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func envInt(k string, d int) int {
	n, e := strconv.Atoi(os.Getenv(k))
	if e != nil || n <= 0 {
		return d
	}
	return n
}
func config() Config {
	return Config{env("PORT", "3000"), strings.TrimRight(env("BASE_URL", "http://localhost:3000"), "/"), env("UPI_ID", ""), env("UPI_NAME", "MrKrishna"), env("GMAIL_USER", ""), strings.ReplaceAll(env("GMAIL_APP_PASSWORD", ""), " ", ""), env("GMAIL_IMAP_HOST", "imap.gmail.com"), env("GMAIL_IMAP_PORT", "993"), strings.ToLower(env("FAMPAY_OFFICIAL_EMAIL", "")), env("DATABASE_PATH", "./data/db.sqlite"), envInt("QR_DISPLAY_MINUTES", 10), envInt("PAYMENT_EXPIRY_MINUTES", 30), envInt("VERIFICATION_DELAY_SECONDS", 60)}
}

func main() {
	c := config()
	if e := os.MkdirAll(filepath.Dir(c.dbPath), 0755); e != nil {
		log.Fatal(e)
	}
	db, e := sql.Open("sqlite", c.dbPath)
	if e != nil {
		log.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	schema := []string{
		`PRAGMA journal_mode=WAL;`, `PRAGMA busy_timeout=5000;`,
		`CREATE TABLE IF NOT EXISTS payments(txn TEXT PRIMARY KEY,amount INTEGER NOT NULL,upi_id TEXT NOT NULL,uri TEXT NOT NULL,payment_link TEXT NOT NULL,qr_link TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'PENDING',created TEXT NOT NULL,deadline TEXT NOT NULL,verify_at TEXT,confirmed_at TEXT,paid_at TEXT,updated TEXT NOT NULL);`,
		`CREATE INDEX IF NOT EXISTS payments_pending ON payments(status,deadline);`,
		`CREATE TABLE IF NOT EXISTS emails(message_id TEXT PRIMARY KEY,sender TEXT,subject TEXT,body TEXT,txn TEXT,amount INTEGER,received TEXT,processed TEXT);`,
		`CREATE TABLE IF NOT EXISTS verification_logs(id INTEGER PRIMARY KEY,txn TEXT,kind TEXT,result TEXT,email_id TEXT,checked TEXT);`,
	}
	for _, q := range schema {
		if _, e = db.Exec(q); e != nil {
			log.Fatal(e)
		}
	}
	a := &App{c, db, sync.Mutex{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", a.health)
	mux.HandleFunc("/", a.home)
	mux.HandleFunc("/link", func(w http.ResponseWriter, r *http.Request) { a.create(w, r, true) })
	mux.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) { a.create(w, r, false) })
	mux.HandleFunc("/cheque", a.cheque)
	mux.HandleFunc("/pay/", a.payPage)
	mux.HandleFunc("/qr/", a.qrPage)
	mux.HandleFunc("/api/qr/", a.qrImage)
	mux.HandleFunc("/api/payments/", a.confirm)
	go a.scheduler()
	s := &http.Server{Addr: ":" + c.port, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 90 * time.Second}
	log.Printf("listening on :%s", c.port)
	log.Fatal(s.ListenAndServe())
}

// Render liveness only. No DB, Gmail, payment checks, metrics, or tracking.
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write([]byte("ok"))
}
func out(w http.ResponseWriter, s int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(s)
	_ = json.NewEncoder(w).Encode(v)
}
func (a *App) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	out(w, 200, map[string]any{"success": true, "name": "MrKrishna UPI Payment API", "status": "online"})
}
func money(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	f, e := strconv.ParseFloat(s, 64)
	if e != nil || f <= 0 || f > 10000000 {
		return 0, false
	}
	return int64(f*100 + 0.5), true
}
func txnID(t time.Time) (string, error) {
	n, e := rand.Int(rand.Reader, big.NewInt(10000))
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("MK%s%04d", t.In(time.FixedZone("IST", 19800)).Format("20060102"), n.Int64()), nil
}
func (a *App) create(w http.ResponseWriter, r *http.Request, withPage bool) {
	if r.Method != "GET" {
		out(w, 405, map[string]any{"success": false, "error": "METHOD_NOT_ALLOWED"})
		return
	}
	if a.cfg.upiID == "" {
		out(w, 503, map[string]any{"success": false, "error": "UPI_NOT_CONFIGURED"})
		return
	}
	amount, ok := money(r.URL.Query().Get("value"))
	if !ok {
		out(w, 400, map[string]any{"success": false, "error": "INVALID_AMOUNT"})
		return
	}
	now := time.Now().In(time.FixedZone("IST", 19800))
	var id string
	for i := 0; i < 20; i++ {
		v, e := txnID(now)
		if e != nil {
			break
		}
		var n int
		_ = a.db.QueryRow("SELECT COUNT(*) FROM payments WHERE txn=?", v).Scan(&n)
		if n == 0 {
			id = v
			break
		}
	}
	if id == "" {
		out(w, 500, map[string]any{"success": false, "error": "ID_GENERATION_FAILED"})
		return
	}
	q := url.Values{}
	q.Set("pa", a.cfg.upiID)
	q.Set("pn", a.cfg.upiName)
	q.Set("am", fmt.Sprintf("%d.%02d", amount/100, amount%100))
	q.Set("cu", "INR")
	q.Set("tn", id)
	q.Set("tr", id)
	uri := "upi://pay?" + q.Encode()
	pl := a.cfg.baseURL + "/pay/" + id
	ql := a.cfg.baseURL + "/qr/" + id
	created := now.Format(time.RFC3339Nano)
	deadline := now.Add(time.Duration(a.cfg.expiryMins) * time.Minute).Format(time.RFC3339Nano)
	_, e := a.db.Exec(`INSERT INTO payments(txn,amount,upi_id,uri,payment_link,qr_link,status,created,deadline,updated) VALUES(?,?,?,?,?,?,'PENDING',?,?,?)`, id, amount, a.cfg.upiID, uri, pl, ql, created, deadline, created)
	if e != nil {
		out(w, 500, map[string]any{"success": false, "error": "DATABASE_ERROR"})
		return
	}
	v := map[string]any{"success": true, "transaction_id": id, "amount": float64(amount) / 100, "currency": "INR", "qr_link": ql, "status": "PENDING"}
	if withPage {
		v["payment_link"] = pl
	}
	out(w, 200, v)
}
func (a *App) get(id string) (Payment, error) {
	var p Payment
	e := a.db.QueryRow("SELECT txn,amount,uri,status,created,deadline FROM payments WHERE txn=?", id).Scan(&p.txn, &p.amount, &p.uri, &p.status, &p.created, &p.deadline)
	return p, e
}
func parseTime(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }
func (a *App) cheque(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		out(w, 405, map[string]any{"success": false, "error": "METHOD_NOT_ALLOWED"})
		return
	}
	id := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("value")))
	if id == "" {
		out(w, 400, map[string]any{"success": false, "error": "TRANSACTION_ID_REQUIRED"})
		return
	}
	p, e := a.get(id)
	if e == sql.ErrNoRows {
		out(w, 404, map[string]any{"success": false, "error": "TRANSACTION_NOT_FOUND"})
		return
	}
	if e != nil {
		out(w, 500, map[string]any{"success": false, "error": "DATABASE_ERROR"})
		return
	}
	if p.status == "PENDING" {
		result, e := a.verify(id, "API")
		if e != nil {
			out(w, 503, map[string]any{"success": false, "error": "EMAIL_VERIFICATION_UNAVAILABLE", "status": "PENDING"})
			return
		}
		p, _ = a.get(id)
		if result == "PENDING" && time.Now().After(parseTime(p.deadline)) {
			_ = a.mark(id, "FAILED", "FINAL", "")
			p, _ = a.get(id)
		}
	}
	out(w, 200, map[string]any{"success": true, "transaction_id": p.txn, "amount": float64(p.amount) / 100, "status": p.status, "payment_confirmed": p.status == "PAID"})
}
func (a *App) mark(id, status, kind, email string) error {
	now := time.Now().Format(time.RFC3339Nano)
	tx, e := a.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if status == "PAID" {
		_, e = tx.Exec("UPDATE payments SET status='PAID',paid_at=?,updated=?,verify_at=NULL WHERE txn=? AND status='PENDING'", now, now, id)
	} else {
		_, e = tx.Exec("UPDATE payments SET status='FAILED',updated=?,verify_at=NULL WHERE txn=? AND status='PENDING'", now, id)
	}
	if e != nil {
		return e
	}
	_, e = tx.Exec("INSERT INTO verification_logs(txn,kind,result,email_id,checked) VALUES(?,?,?,?,?)", id, kind, status, email, now)
	if e != nil {
		return e
	}
	return tx.Commit()
}

var txnRE = regexp.MustCompile(`(?i)\bMK[0-9]{12}\b`)
var amountRE = regexp.MustCompile(`(?i)(?:₹|INR\s*)\s*([0-9,]+(?:\.[0-9]{1,2})?)`)

func (a *App) fetchEmails() ([]Notice, error) {
	if a.cfg.gmailUser == "" || a.cfg.gmailPass == "" || a.cfg.famPaySender == "" {
		return nil, fmt.Errorf("Gmail/FamPay configuration missing")
	}
	c, e := client.DialTLS(a.cfg.gmailHost+":"+a.cfg.gmailPort, nil)
	if e != nil {
		return nil, e
	}
	defer c.Logout()
	if e = c.Login(a.cfg.gmailUser, a.cfg.gmailPass); e != nil {
		return nil, e
	}
	box, e := c.Select("INBOX", false)
	if e != nil {
		return nil, e
	}
	if box.Messages == 0 {
		return nil, nil
	}
	ids, e := c.Search(imap.NewSearchCriteria())
	if e != nil {
		return nil, e
	}
	if len(ids) == 0 {
		return nil, nil
	}
	start := 0
	if len(ids) > 10 {
		start = len(ids) - 10
	}
	set := new(imap.SeqSet)
	set.AddNum(ids[start:]...)
	ch := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	section := new(imap.BodySectionName)
	go func() { done <- c.Fetch(set, []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}, ch) }()
	var result []Notice
	for m := range ch {
		if m.Envelope == nil {
			continue
		}
		sender := ""
		if len(m.Envelope.From) > 0 {
			sender = strings.ToLower(m.Envelope.From[0].Address())
		}
		if sender != a.cfg.famPaySender {
			continue
		}
		raw := ""
		if body := m.GetBody(section); body != nil {
			buf := make([]byte, 1<<20)
			n, _ := body.Read(buf)
			raw = string(buf[:n])
		}
		subject := m.Envelope.Subject
		parsed, _ := mail.ReadMessage(strings.NewReader(raw))
		if parsed != nil {
			buf := make([]byte, 1<<20)
			n, _ := parsed.Body.Read(buf)
			raw = string(buf[:n])
			if parsed.Header.Get("Subject") != "" {
				subject = parsed.Header.Get("Subject")
			}
		}
		full := subject + "\n" + raw
		tid := ""
		if x := txnRE.FindString(full); x != "" {
			tid = strings.ToUpper(x)
		}
		var amount int64
		if x := amountRE.FindStringSubmatch(full); len(x) > 1 {
			amount, _ = money(x[1])
		}
		messageID := m.Envelope.MessageId
		if messageID == "" {
			messageID = fmt.Sprintf("seq-%d", m.SeqNum)
		}
		result = append(result, Notice{messageID, sender, subject, raw, tid, amount})
	}
	if e := <-done; e != nil {
		return nil, e
	}
	return result, nil
}
func (a *App) verify(id, kind string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, e := a.get(id)
	if e != nil {
		return "", e
	}
	if p.status != "PENDING" {
		return p.status, nil
	}
	notices, e := a.fetchEmails()
	if e != nil {
		return "PENDING", e
	}
	for _, n := range notices {
		_, _ = a.db.Exec("INSERT OR IGNORE INTO emails(message_id,sender,subject,body,txn,amount,received,processed) VALUES(?,?,?,?,?,?,?,?)", n.id, n.sender, n.subject, n.body, n.txn, n.amount, time.Now().Format(time.RFC3339Nano), time.Now().Format(time.RFC3339Nano))
		if n.txn == id && n.amount == p.amount && n.amount > 0 {
			if e := a.mark(id, "PAID", kind, n.id); e != nil {
				return "", e
			}
			return "PAID", nil
		}
	}
	return "PENDING", nil
}
func (a *App) confirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		out(w, 405, map[string]any{"success": false, "error": "METHOD_NOT_ALLOWED"})
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/payments/"), "/")
	if !strings.HasSuffix(path, "/confirm") {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSuffix(path, "/confirm")
	id = strings.Trim(id, "/")
	p, e := a.get(id)
	if e == sql.ErrNoRows {
		out(w, 404, map[string]any{"success": false, "error": "TRANSACTION_NOT_FOUND"})
		return
	}
	if e != nil {
		out(w, 500, map[string]any{"success": false, "error": "DATABASE_ERROR"})
		return
	}
	if p.status != "PENDING" {
		out(w, 200, map[string]any{"success": true, "status": p.status})
		return
	}
	now := time.Now()
	if now.After(parseTime(p.deadline)) {
		out(w, 409, map[string]any{"success": false, "error": "PAYMENT_WINDOW_EXPIRED"})
		return
	}
	due := now.Add(time.Duration(a.cfg.verifySecs) * time.Second).Format(time.RFC3339Nano)
	_, e = a.db.Exec("UPDATE payments SET confirmed_at=?,verify_at=?,updated=? WHERE txn=? AND status='PENDING'", now.Format(time.RFC3339Nano), due, now.Format(time.RFC3339Nano), id)
	if e != nil {
		out(w, 500, map[string]any{"success": false, "error": "DATABASE_ERROR"})
		return
	}
	out(w, 200, map[string]any{"success": true, "status": "PENDING", "message": "Verification scheduled", "verification_at": due})
}
func (a *App) scheduler() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		rows, e := a.db.Query("SELECT txn,created,deadline,verify_at FROM payments WHERE status='PENDING'")
		if e != nil {
			continue
		}
		type item struct {
			id, created, deadline string
			verify                sql.NullString
		}
		var list []item
		for rows.Next() {
			var x item
			if rows.Scan(&x.id, &x.created, &x.deadline, &x.verify) == nil {
				list = append(list, x)
			}
		}
		rows.Close()
		now := time.Now()
		for _, x := range list {
			created, deadline := parseTime(x.created), parseTime(x.deadline)
			due := false
			kind := "AUTO"
			if x.verify.Valid {
				due = !now.Before(parseTime(x.verify.String))
				kind = "CUSTOMER"
			} else {
				due = !now.Before(created.Add(time.Duration(a.cfg.qrMins) * time.Minute))
			}
			if !due && !now.After(deadline) {
				continue
			}
			status, e := a.verify(x.id, kind)
			if e != nil {
				log.Printf("Gmail verification for %s: %v", x.id, e)
				continue
			}
			if status == "PENDING" {
				if !now.Before(deadline) {
					_ = a.mark(x.id, "FAILED", "FINAL", "")
				} else {
					_, _ = a.db.Exec("UPDATE payments SET verify_at=? WHERE txn=? AND status='PENDING'", deadline.Format(time.RFC3339Nano), x.id)
				}
			}
		}
	}
}
func (a *App) payPage(w http.ResponseWriter, r *http.Request) {
	p, e := a.get(strings.TrimPrefix(r.URL.Path, "/pay/"))
	if e != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, payHTML, pageCSS, float64(p.amount)/100, p.txn, p.txn, p.txn, parseTime(p.deadline).UnixMilli())
}
func (a *App) qrPage(w http.ResponseWriter, r *http.Request) {
	p, e := a.get(strings.TrimPrefix(r.URL.Path, "/qr/"))
	if e != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, qrHTML, pageCSS, float64(p.amount)/100, p.txn, p.txn)
}
func (a *App) qrImage(w http.ResponseWriter, r *http.Request) {
	p, e := a.get(strings.TrimPrefix(r.URL.Path, "/api/qr/"))
	if e != nil {
		http.NotFound(w, r)
		return
	}
	png, e := qrcode.Encode(p.uri, qrcode.Medium, 320)
	if e != nil {
		http.Error(w, "QR error", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

const payHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Payment</title><style>%s</style></head><body><main class="card"><div class="brand">MRKRISHNA <span>PAYMENTS</span></div><p class="eyebrow">PAYMENT CHECKOUT</p><h1>Complete your payment</h1><div class="amount">₹%.2f</div><p class="muted">Transaction ID · <b>%s</b></p><div class="qr"><img src="/api/qr/%s"></div><p class="muted">Scan with your UPI application.</p><p class="timer">QR timer <b id="timer">10:00</b></p><button id="check">I've paid · Check payment</button><p id="status">Waiting for payment confirmation</p></main><div id="modal" class="modal hide"><div class="dialog"><h2>Have you completed payment?</h2><p>Confirm only after completing your UPI payment.</p><button class="secondary" id="cancel">Cancel</button><button id="yes">Confirm payment</button></div></div><script>const txn=%q,deadline=%d;const $=x=>document.getElementById(x);function tick(){let n=Math.max(0,deadline-Date.now());$('timer').textContent=String(Math.floor(n/60000)).padStart(2,'0')+':'+String(Math.floor(n%60000/1000)).padStart(2,'0')}tick();setInterval(tick,1000);$('check').onclick=()=>$('modal').classList.remove('hide');$('cancel').onclick=()=>$('modal').classList.add('hide');$('yes').onclick=async()=>{$('modal').classList.add('hide');try{let r=await fetch('/api/payments/'+txn+'/confirm',{method:'POST'});let d=await r.json();$('status').textContent=d.message||d.status||d.error||'Request received'}catch(e){$('status').textContent='Could not schedule verification'}};setInterval(poll,15000);</script></body></html>`
const qrHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>UPI QR</title><style>%s</style></head><body><main class="card"><div class="brand">MRKRISHNA <span>PAYMENTS</span></div><p class="eyebrow">SCAN TO PAY</p><div class="amount">₹%.2f</div><p class="muted">Transaction ID · <b>%s</b></p><div class="qr"><img src="/api/qr/%s"></div><p class="muted">Scan using your UPI application.</p></main></body></html>`
const pageCSS = `*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:20px;background:radial-gradient(circle at top,#222342,#090b12 60%);color:#f5f6fb;font:15px system-ui,sans-serif}.card{width:min(100%,430px);padding:30px 24px;text-align:center;background:#171a29;border:1px solid #30344a;border-radius:22px;box-shadow:0 25px 80px #0008}.brand{font-weight:800;letter-spacing:.16em;font-size:13px}.brand span,.muted{color:#a5abc0}.eyebrow{margin:30px 0 8px;color:#999fbb;font-size:10px;letter-spacing:.2em}h1{font-size:24px;margin:0}.amount{font-size:38px;font-weight:800;margin:12px}.muted{font-size:13px}.qr{width:240px;height:240px;padding:10px;margin:24px auto 12px;background:white;border-radius:14px}.qr img{width:100%;height:100%}.timer{padding:12px;border:1px solid #343950;border-radius:12px;color:#abb0c6}button{width:100%;padding:14px;border:0;border-radius:11px;background:#897bff;color:white;font-weight:700;cursor:pointer;margin-top:8px}.secondary{background:#34384c}.modal{position:fixed;inset:0;background:#000b;display:grid;place-items:center;padding:20px}.hide{display:none}.dialog{width:min(100%,360px);padding:24px;border-radius:18px;background:#1a1d2c;border:1px solid #383d54}.dialog p,#status{color:#aeb3c8;font-size:13px}.dialog button{width:48%;margin:12px 1% 0}`
