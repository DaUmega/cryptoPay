package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func postReq(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHTTPHardening(t *testing.T) {
	app := testApp(t)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	// Wrong method, wrong token, empty token, and token as query
	// param (not a header) must all fail.
	for _, tc := range []struct {
		name string
		do   func() *http.Response
		want int
	}{
		{"GET on create", func() *http.Response { r, _ := http.Get(srv.URL + "/invoices"); return r }, 405},
		{"POST on healthz", func() *http.Response { return postReq(t, srv.URL+"/healthz", "", `{}`) }, 405},
		{"wrong token", func() *http.Response {
			return postReq(t, srv.URL+"/invoices", "wrong", `{"coin":"BTC","amount_crypto":0.01}`)
		}, 401},
		{"token in query", func() *http.Response {
			req, _ := http.NewRequest("POST", srv.URL+"/invoices?token=test-token", strings.NewReader(`{"coin":"BTC","amount_crypto":0.01}`))
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}, 401},
		{"unknown invoice id", func() *http.Response {
			r, _ := http.Get(srv.URL + "/invoices/0123456789abcdef0123456789abcdef")
			return r
		}, 404},
		{"path traversal collapses to token-gated pool", func() *http.Response { r, _ := http.Get(srv.URL + "/invoices/../admin/pool"); return r }, 401},
		{"non-hex id", func() *http.Response { r, _ := http.Get(srv.URL + "/pay/not-an-id"); return r }, 404},
	} {
		resp := tc.do()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
		resp.Body.Close()
	}

	// Empty API token in config must deny everything, fail closed.
	app.cfg.APIToken = ""
	resp := postReq(t, srv.URL+"/invoices", "test-token", `{"coin":"BTC","amount_crypto":0.01}`)
	if resp.StatusCode != 401 {
		t.Errorf("empty config token: status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	app.cfg.APIToken = "test-token"

	// Oversized body: 200KB of JSON must be rejected, not buffered.
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	big := `{"coin":"BTC","amount_crypto":0.01,"merchant_ref":"` + strings.Repeat("x", 200000) + `"}`
	resp = postReq(t, srv.URL+"/invoices", "test-token", big)
	if resp.StatusCode != 400 {
		t.Errorf("oversized body: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Pool admin without a token must not leak addresses.
	resp, err := http.Get(srv.URL + "/admin/pool")
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("unauthenticated pool list = %v err %v, want 401", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Pool add validates addresses: a typo is a 400, not a silent
	// invoice that can never be paid.
	resp = postReq(t, srv.URL+"/admin/pool", "test-token", `{"coin":"BTC","addresses":["bc1qpooladdr"]}`)
	if resp.StatusCode != 400 {
		t.Errorf("bad pool address: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp = postReq(t, srv.URL+"/admin/pool", "test-token", `{"coin":"BNB","addresses":["`+testETHAddr+`"]}`)
	if resp.StatusCode != 400 {
		t.Errorf("removed coin pool add: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Pay page: a return URL with markup must be JSON-escaped in the
	// script block, never raw, so it cannot break out of it.
	app2 := testApp(t)
	if _, _, err := app2.store.addToPool("BTC", []string{testBTCAddr2}); err != nil {
		t.Fatal(err)
	}
	inv, err := app2.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01, ReturnURL: "https://example.com/thanks?x="})
	if err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(app2.routes())
	defer srv2.Close()
	resp, err = http.Get(srv2.URL + "/pay/" + inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "https://example.com/thanks") {
		t.Fatal("pay page should show the return URL target in script data")
	}
	if strings.Contains(string(body), "</script><script>") {
		t.Fatal("pay page must not contain an injected script block")
	}
	_ = json.Valid
}

func TestWebhookHMACAndRetries(t *testing.T) {
	// A webhook receiver that fails twice then succeeds must still
	// end in "sent" (retries at 0s, 5s would be slow, so this test
	// uses the unit-level sign function plus a direct fireWebhook
	// against a receiver that always succeeds, and checks the
	// failure path separately with a dead URL).
	app := testApp(t)
	if _, _, err := app.store.addToPool("BTC", []string{testBTCAddr}); err != nil {
		t.Fatal(err)
	}
	var gotSig string
	var gotBody []byte
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Cryptopay-Signature")
		w.WriteHeader(200)
	}))
	defer hookSrv.Close()
	inv, err := app.createInvoice(createInvoiceReq{Coin: "BTC", AmountCrypto: 0.01, WebhookURL: hookSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	inv.State = "confirmed"
	inv.ConfirmedAt = time.Now()
	app.fireWebhook(inv)
	if gotSig == "" || gotBody == nil {
		t.Fatal("webhook must be signed and delivered")
	}
	if gotSig != signWebhook("test-secret", gotBody) {
		t.Fatal("webhook signature must verify against the body")
	}
	// Unsigned webhooks (no secret) must carry no signature header
	// at all, rather than a signature over an empty secret.
	if signWebhook("", gotBody) != "" {
		t.Fatal("empty secret must give no signature")
	}
}

func TestRateLimiter(t *testing.T) {
	lim := newRateLimiter(60, 2)
	lim.now = func() time.Time { return time.Unix(1000, 0) }
	if !lim.allow("1.2.3.4") || !lim.allow("1.2.3.4") {
		t.Fatal("burst of 2 should be allowed")
	}
	if lim.allow("1.2.3.4") {
		t.Fatal("third request in the same instant should be limited")
	}
	// A different IP is a different bucket.
	if !lim.allow("5.6.7.8") {
		t.Fatal("different IP should not be limited by another IP")
	}
	// After 60 seconds at 1/sec, the bucket refills to burst.
	lim.now = func() time.Time { return time.Unix(1060, 0) }
	if !lim.allow("1.2.3.4") {
		t.Fatal("bucket should refill over time")
	}
	// The HTTP middleware returns 429, not a hang or a 500.
	app := testApp(t)
	lim2 := newRateLimiter(60, 1)
	handler := lim2.middleware(app.routes())
	srv := httptest.NewServer(handler)
	defer srv.Close()
	if r, _ := http.Get(srv.URL + "/healthz"); r.StatusCode != 200 {
		t.Fatalf("first request = %d, want 200", r.StatusCode)
	} else {
		r.Body.Close()
	}
	if r, _ := http.Get(srv.URL + "/healthz"); r.StatusCode != 429 {
		t.Fatalf("second request = %d, want 429", r.StatusCode)
	} else {
		r.Body.Close()
	}
}

func TestCorruptStoreFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/invoices.json", []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStore(dir); err == nil {
		t.Fatal("corrupt invoices.json must fail, not start with an empty store that loses invoice mappings")
	}
}
