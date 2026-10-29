package main

// Hosted pay page: address, amount, live status. No JavaScript
// frameworks, no external CSS, and deliberately no QR image from a
// third party: the old page sent every payment URI to a public QR API,
// leaking the address and amount to that operator and depending on
// them for the page to work. The merchant's own frontend can render
// its own QR code from the API's payment_uri if it wants one; the
// hosted page works with address, amount, and copy buttons alone.
// Status polls GET /invoices/{id} and redirects to the merchant's
// return URL once confirmed. The ID and return URL are embedded as
// JSON strings, never as raw template text, so a return URL containing
// quotes or markup cannot break out of the script block.

import (
	"encoding/json"
	"html/template"
)

type payPageView struct {
	ID            string
	Coin          string
	Address       string
	Amount        string
	URI           string
	State         string
	StateText     string
	Received      string
	Confirmations int
	Required      int
	ReturnURL     string
	IDJSON        template.JS
	ReturnJSON    template.JS
}

func stateText(state string) string {
	switch state {
	case "awaiting":
		return "Waiting for payment"
	case "detected":
		return "Payment detected, waiting for confirmations"
	case "confirming":
		return "Payment confirming"
	case "confirmed":
		return "Payment confirmed"
	case "expired":
		return "Invoice expired (no payment received)"
	case "underpaid":
		return "Payment received but below the expected amount"
	case "paid_late":
		return "Payment received after expiry; the merchant will review it"
	}
	return state
}

func jsonString(s string) template.JS {
	b, _ := json.Marshal(s)
	return template.JS(b)
}

func payPageData(cfg *Config, inv *Invoice) payPageView {
	uri := paymentURI(cfg, inv)
	maxConf := 0
	for _, p := range inv.Payments {
		if p.Confirmations > maxConf {
			maxConf = p.Confirmations
		}
	}
	return payPageView{
		ID: inv.ID, Coin: inv.Coin, Address: inv.Address,
		Amount: fmtCoinAmount(inv.Coin, inv.AmountCrypto), URI: uri,
		State:     inv.State,
		StateText: stateText(inv.State), Received: fmtCoinAmount(inv.Coin, inv.Received),
		Confirmations: maxConf, Required: cfg.required(inv.Coin),
		ReturnURL: inv.ReturnURL,
		IDJSON:    jsonString(inv.ID), ReturnJSON: jsonString(inv.ReturnURL),
	}
}

var payTemplate = template.Must(template.New("pay").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pay {{.Coin}}</title>
<style>
body{background:#0d1117;color:#c9d1d9;font-family:system-ui,sans-serif;margin:0;padding:24px}
main{max-width:480px;margin:0 auto;text-align:center}
h1{font-size:20px;color:#58a6ff}
.box{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:20px;margin:16px 0}
.addr{word-break:break-all;font-family:monospace;font-size:15px}
.amount{font-size:26px;font-weight:700;color:#3fb950}
button{background:#21262d;color:#c9d1d9;border:1px solid #30363d;border-radius:6px;padding:8px 16px;cursor:pointer;margin:4px}
button:hover{background:#30363d}
.muted{color:#8b949e;font-size:13px}
a{color:#58a6ff}
</style></head><body><main>
<h1>Pay with {{.Coin}}</h1>
<div class="box">
<div class="amount">{{.Amount}} {{.Coin}}</div>
<p class="addr" id="addr">{{.Address}}</p>
<button onclick="copyText('{{.Address}}')">Copy address</button>
<button onclick="copyText('{{.Amount}}')">Copy amount</button>
{{if .URI}}<p><a href="{{.URI}}">Open in wallet</a></p>{{end}}
<p class="muted">To pay by QR code, render one from the payment link on the merchant site, or scan the address above in your wallet.</p>
</div>
<div class="box">
<div id="status">{{.StateText}}</div>
<div class="muted" id="confs">Confirmations: {{.Confirmations}} / {{.Required}} &middot; Received: {{.Received}} {{.Coin}}</div>
<p class="muted">Send exactly the amount above to the address above. Do not reuse this address for another payment.</p>
</div>
<script>
function copyText(t){navigator.clipboard.writeText(t);}
var id={{.IDJSON}}, ret={{.ReturnJSON}};
setInterval(function(){
  fetch('/invoices/'+id).then(function(r){return r.json();}).then(function(inv){
    var s=inv.state, text=s;
    if(s==='awaiting')text='Waiting for payment';
    if(s==='detected')text='Payment detected, waiting for confirmations';
    if(s==='confirming')text='Payment confirming';
    if(s==='confirmed')text='Payment confirmed';
    if(s==='expired')text='Invoice expired (no payment received)';
    if(s==='underpaid')text='Payment received but below the expected amount';
    if(s==='paid_late')text='Payment received after expiry; the merchant will review it';
    document.getElementById('status').textContent=text;
    document.getElementById('confs').textContent='Confirmations: '+inv.confirmations+' / '+inv.confirmations_required+' \u00b7 Received: '+inv.received+' '+inv.coin;
    if((s==='confirmed'||s==='paid_late')&&ret){window.location.href=ret;}
  });
},10000);
</script>
</main></body></html>`))
