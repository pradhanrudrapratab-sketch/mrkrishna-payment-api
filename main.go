package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/textproto"
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
	received                       time.Time
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
	v := map[string]any{"success": true, "transaction_id": id, "amount": float64(amount) / 100, "currency": "INR", "qr_link": ql}
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

func decodeMIMEBody(header textproto.MIMEHeader, body io.Reader) (string, error) {
	mediaType, params, _ := mime.ParseMediaType(header.Get("Content-Type"))

	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", fmt.Errorf("multipart email has no boundary")
		}
		reader := multipart.NewReader(body, boundary)
		var parts []string

		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			if strings.Contains(strings.ToLower(part.Header.Get("Content-Disposition")), "attachment") {
				continue
			}
			value, err := decodeMIMEBody(part.Header, part)
			if err == nil && strings.TrimSpace(value) != "" {
				parts = append(parts, value)
			}
		}
		return strings.Join(parts, "\n"), nil
	}

	var reader io.Reader = body
	switch strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding"))) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		reader = quotedprintable.NewReader(body)
	}

	data, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err != nil {
		return "", err
	}
	value := string(data)

	if strings.EqualFold(mediaType, "text/html") {
		value = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(value, " ")
		value = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(value, " ")
		value = regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(value, " ")
		value = html.UnescapeString(value)
	}
	return value, nil
}

func (a *App) fetchEmails() ([]Notice, error) {
	if a.cfg.gmailUser == "" || a.cfg.gmailPass == "" || a.cfg.famPaySender == "" {
		return nil, fmt.Errorf("Gmail/FamPay configuration missing")
	}

	c, err := client.DialTLS(a.cfg.gmailHost+":"+a.cfg.gmailPort, nil)
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	if err = c.Login(a.cfg.gmailUser, a.cfg.gmailPass); err != nil {
		return nil, err
	}
	box, err := c.Select("INBOX", false)
	if err != nil {
		return nil, err
	}
	if box.Messages == 0 {
		return nil, nil
	}

	ids, err := c.Search(imap.NewSearchCriteria())
	if err != nil {
		return nil, err
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

	go func() {
		done <- c.Fetch(set, []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchInternalDate,
			section.FetchItem(),
		}, ch)
	}()

	var result []Notice
	for m := range ch {
		if m.Envelope == nil || len(m.Envelope.From) == 0 {
			continue
		}

		sender := strings.ToLower(m.Envelope.From[0].Address())
		if sender != a.cfg.famPaySender {
			continue
		}

		body := ""
		if raw := m.GetBody(section); raw != nil {
			data, _ := io.ReadAll(io.LimitReader(raw, 1<<20))
			message, parseErr := mail.ReadMessage(strings.NewReader(string(data)))
			if parseErr == nil {
				body, _ = decodeMIMEBody(textproto.MIMEHeader(message.Header), message.Body)
			}
		}

		subject := m.Envelope.Subject
		if decoded, decodeErr := new(mime.WordDecoder).DecodeHeader(subject); decodeErr == nil {
			subject = decoded
		}

		received := m.InternalDate
		if received.IsZero() {
			received = m.Envelope.Date
		}
		if received.IsZero() {
			continue
		}

		full := subject + "\n" + body
		var amount int64
		if match := amountRE.FindStringSubmatch(full); len(match) > 1 {
			amount, _ = money(match[1])
		}

		// FamPay's own transaction ID / UTR is deliberately NOT matched
		// against our generated MK transaction ID.
		messageID := m.Envelope.MessageId
		if messageID == "" {
			messageID = fmt.Sprintf("seq-%d", m.SeqNum)
		}

		result = append(result, Notice{
			id:       messageID,
			sender:   sender,
			subject:  subject,
			body:     body,
			amount:   amount,
			received: received,
		})
	}

	if err := <-done; err != nil {
		return nil, err
	}
	return result, nil
}

func (a *App) verify(id, kind string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	p, err := a.get(id)
	if err != nil {
		return "", err
	}
	if p.status != "PENDING" {
		return p.status, nil
	}

	notices, err := a.fetchEmails()
	if err != nil {
		return "PENDING", err
	}

	created := parseTime(p.created)
	deadline := parseTime(p.deadline)

	for _, n := range notices {
		now := time.Now().Format(time.RFC3339Nano)
		_, _ = a.db.Exec(
			`INSERT OR IGNORE INTO emails(message_id,sender,subject,body,txn,amount,received,processed)
 VALUES(?,?,?,?,?,?,?,?)`,
			n.id, n.sender, n.subject, n.body, "", n.amount,
			n.received.Format(time.RFC3339Nano), now,
		)

		// Require an incoming-payment notification, not a payment sent by us.
		content := strings.ToLower(n.subject + "\n" + n.body)
		if !strings.Contains(content, "received") {
			continue
		}
		if strings.Contains(content, "successfully paid") ||
			strings.Contains(content, "payment sent") ||
			strings.Contains(content, "you paid") ||
			strings.Contains(content, "debited") {
			continue
		}

		if n.amount <= 0 || n.amount != p.amount {
			continue
		}

		// Email must have arrived around this payment's creation and expiry.
		if n.received.Before(created.Add(-2*time.Minute)) ||
			n.received.After(deadline.Add(5*time.Minute)) {
			continue
		}

		// Never reuse one notification to mark a second payment as paid.
		var used int
		err = a.db.QueryRow(
			`SELECT COUNT(*) FROM verification_logs WHERE email_id=? AND result='PAID'`,
			n.id,
		).Scan(&used)
		if err != nil {
			return "", err
		}
		if used > 0 {
			continue
		}

		// Amount-only matching is safe to automate only when exactly one
		// pending payment of this amount fits the email's time window.
		rows, err := a.db.Query(
			`SELECT txn,created,deadline FROM payments WHERE amount=? AND status='PENDING'`,
			n.amount,
		)
		if err != nil {
			return "", err
		}

		candidates := 0
		matchedThisPayment := false
		for rows.Next() {
			var candidateID, candidateCreated, candidateDeadline string
			if err := rows.Scan(&candidateID, &candidateCreated, &candidateDeadline); err != nil {
				rows.Close()
				return "", err
			}
			start := parseTime(candidateCreated)
			end := parseTime(candidateDeadline)
			if !n.received.Before(start.Add(-2*time.Minute)) &&
				!n.received.After(end.Add(5*time.Minute)) {
				candidates++
				if candidateID == id {
					matchedThisPayment = true
				}
			}
		}
		rows.Close()

		if candidates != 1 || !matchedThisPayment {
			continue
		}

		if err := a.mark(id, "PAID", kind, n.id); err != nil {
			return "", err
		}
		return "PAID", nil
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
	p, err := a.get(strings.TrimPrefix(r.URL.Path, "/pay/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	qrDeadline := parseTime(p.created).Add(time.Duration(a.cfg.qrMins) * time.Minute)
	fmt.Fprintf(w, payHTML, pageCSS, float64(p.amount)/100, p.txn, p.txn, p.txn, qrDeadline.UnixMilli())
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

const payHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Payment</title><style>%s</style></head><body><main class="card"><div class="brand">MRKRISHNA <span>PAYMENTS</span></div><p class="eyebrow">PAYMENT CHECKOUT</p><h1>Complete your payment</h1><div class="amount">₹%.2f</div><p class="muted">Transaction ID · <b>%s</b></p><div class="qr"><img src="/api/qr/%s" alt="UPI payment QR code"></div><p class="muted">Scan with your UPI application.</p><p class="timer">QR timer <b id="timer">10:00</b></p><button id="check" type="button">I've paid · Check payment</button><p id="status" aria-live="polite">Waiting for payment confirmation</p></main><div id="modal" class="modal hide" role="dialog" aria-modal="true"><div class="dialog"><h2>Have you completed payment?</h2><p>Confirm only after completing your UPI payment.</p><button class="secondary" id="cancel" type="button">Cancel</button><button id="yes" type="button">Confirm payment</button></div></div><script>
const txn=%q,deadline=%d;
const $=id=>document.getElementById(id);
let checking=false, poller;
function tick(){
 const left=Math.max(0,deadline-Date.now());
 $('timer').textContent=String(Math.floor(left/60000)).padStart(2,'0')+':'+String(Math.floor(left%60000/1000)).padStart(2,'0');
}
tick();
setInterval(tick,1000);

$('check').addEventListener('click',()=> $('modal').classList.remove('hide'));
$('cancel').addEventListener('click',()=> $('modal').classList.add('hide'));

async function poll(){
 if(checking)return;
 checking=true;
 try{
  const response=await fetch('/cheque?value='+encodeURIComponent(txn),{cache:'no-store'});
  const data=await response.json();
  if(data.status==='PAID'){
   $('status').textContent='Payment verified successfully.';
   $('check').textContent='Payment verified';
   $('check').disabled=true;
   clearInterval(poller);
  }else if(data.status==='FAILED'){
   $('status').textContent='Payment window expired. Contact support if you have paid.';
   clearInterval(poller);
  }else if(!response.ok){
   $('status').textContent='Verification unavailable. Retrying...';
  }else{
   $('status').textContent='Payment not found yet. Checking again...';
  }
 }catch(error){
  $('status').textContent='Connection problem. Retrying payment check...';
 }finally{
  checking=false;
 }
}

$('yes').addEventListener('click',async()=>{
 $('modal').classList.add('hide');
 $('yes').disabled=true;
 $('status').textContent='Your confirmation was received. Checking payment...';
 try{
  const response=await fetch('/api/payments/'+encodeURIComponent(txn)+'/confirm',{
   method:'POST',
   headers:{'Content-Type':'application/json'}
  });
  const data=await response.json();
  if(!response.ok){
   $('status').textContent=data.error||'Could not schedule verification.';
  }else{
   $('status').textContent=data.message||'Checking payment...';
   await poll();
  }
 }catch(error){
  $('status').textContent='Could not contact the payment server. Retrying...';
 }finally{
  $('yes').disabled=false;
 }
});

poll();
poller=setInterval(poll,15000);
</script></body></html>`

const qrHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>UPI QR</title><style>%s</style></head><body><main class="card"><div class="brand">MRKRISHNA <span>PAYMENTS</span></div><p class="eyebrow">SCAN TO PAY</p><div class="amount">₹%.2f</div><p class="muted">Transaction ID · <b>%s</b></p><div class="qr"><img src="/api/qr/%s"></div><p class="muted">Scan using your UPI application.</p></main></body></html>`
const pageCSS = `*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:20px;background:radial-gradient(circle at top,#222342,#090b12 60%);color:#f5f6fb;font:15px system-ui,sans-serif}.card{width:min(100%,430px);padding:30px 24px;text-align:center;background:#171a29;border:1px solid #30344a;border-radius:22px;box-shadow:0 25px 80px #0008}.brand{font-weight:800;letter-spacing:.16em;font-size:13px}.brand span,.muted{color:#a5abc0}.eyebrow{margin:30px 0 8px;color:#999fbb;font-size:10px;letter-spacing:.2em}h1{font-size:24px;margin:0}.amount{font-size:38px;font-weight:800;margin:12px}.muted{font-size:13px}.qr{width:240px;height:240px;padding:10px;margin:24px auto 12px;background:white;border-radius:14px}.qr img{width:100%;height:100%}.timer{padding:12px;border:1px solid #343950;border-radius:12px;color:#abb0c6}button{width:100%;padding:14px;border:0;border-radius:11px;background:#897bff;color:white;font-weight:700;cursor:pointer;margin-top:8px}.secondary{background:#34384c}.modal{position:fixed;inset:0;background:#000b;display:grid;place-items:center;padding:20px}.hide{display:none}.dialog{width:min(100%,360px);padding:24px;border-radius:18px;background:#1a1d2c;border:1px solid #383d54}.dialog p,#status{color:#aeb3c8;font-size:13px}.dialog button{width:48%;margin:12px 1% 0}`
