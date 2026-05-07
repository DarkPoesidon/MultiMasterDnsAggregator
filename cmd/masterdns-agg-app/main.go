package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/DarkPoesidon/MultiMasterDnsAggregator/internal/appcontrol"
)

type appServer struct {
	runtime *appcontrol.Runtime
}

func main() {
	uiListen := flag.String("ui-listen", "127.0.0.1:19100", "UI HTTP listen address")
	flag.Parse()

	srv := &appServer{runtime: appcontrol.NewRuntime(appcontrol.DefaultAppConfig())}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/status", srv.handleStatus)
	mux.HandleFunc("/api/config", srv.handleConfig)
	mux.HandleFunc("/api/start", srv.handleStart)
	mux.HandleFunc("/api/stop", srv.handleStop)
	mux.HandleFunc("/api/logs", srv.handleLogs)
	mux.HandleFunc("/api/config/save", srv.handleSaveConfig)
	mux.HandleFunc("/api/config/load", srv.handleLoadConfig)

	fmt.Printf("masterdns-agg-app UI listening on http://%s\n", *uiListen)
	if err := http.ListenAndServe(*uiListen, mux); err != nil {
		panic(err)
	}
}

func (s *appServer) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := strings.ReplaceAll(indexHTML, "__CONFIG_FILE__", appcontrol.DefaultConfigFile)
	_, _ = w.Write([]byte(html))
}

func (s *appServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"status": s.runtime.Status(),
		"line":   appcontrol.FormatStatusLine(s.runtime.Status()),
	})
}

func (s *appServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.runtime.Config())
	case http.MethodPost:
		var cfg appcontrol.AppConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.runtime.UpdateConfig(cfg); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *appServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := s.runtime.Start(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *appServer) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.runtime.Stop()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *appServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	max := 400
	if raw := strings.TrimSpace(r.URL.Query().Get("max")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			max = parsed
		}
	}
	writeJSON(w, map[string]any{"logs": s.runtime.RecentLogs(max)})
}

func (s *appServer) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := appcontrol.SaveConfig(s.runtime.Config()); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "file": appcontrol.DefaultConfigFile})
}

func (s *appServer) handleLoadConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cfg, err := appcontrol.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.runtime.UpdateConfig(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{
		"ok":     true,
		"config": cfg,
		"file":   appcontrol.DefaultConfigFile,
	})
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSONWithStatus(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONWithStatus(w, http.StatusOK, v)
}

func writeJSONWithStatus(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

const indexHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>MasterDNS Aggregator App</title>
  <style>
    body{font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;margin:20px;max-width:960px}
    input,textarea,button{font:inherit;padding:8px}
    textarea{width:100%;height:220px}
    .row{display:grid;grid-template-columns:220px 1fr;gap:10px;margin:8px 0}
    .toolbar{display:flex;gap:8px;flex-wrap:wrap;margin:12px 0}
  </style>
</head>
<body>
  <h2>MasterDNS Aggregator App</h2>
  <div id="status">Loading...</div>
  <div class="toolbar">
    <button onclick="startApp()">Start</button>
    <button onclick="stopApp()">Stop</button>
    <button onclick="saveConfig()">Save Config</button>
    <button onclick="loadConfig()">Load Config</button>
    <button onclick="refreshAll()">Refresh</button>
  </div>
  <div class="row"><label>Listen</label><input id="listen" /></div>
  <div class="row"><label>Aggregator</label><input id="agg" /></div>
  <div class="row"><label>Chunk Size</label><input id="chunk" /></div>
  <div class="row"><label>Dial Timeout (sec)</label><input id="dial" /></div>
  <div class="row"><label>Reconnect (sec)</label><input id="reconnect" /></div>
  <div class="row"><label>Tunnel 1</label><input id="t1" /></div>
  <div class="row"><label>Tunnel 2</label><input id="t2" /></div>
  <div class="row"><label>Tunnel 3</label><input id="t3" /></div>
  <div class="row"><label>Tunnel 4</label><input id="t4" /></div>
  <div class="row"><label>Tunnel 5</label><input id="t5" /></div>
  <p>Config import/export file: <code>__CONFIG_FILE__</code></p>
  <h3>Logs</h3>
  <textarea id="logs" readonly></textarea>
<script>
async function j(url, opt){const r=await fetch(url,opt);const d=await r.json();if(!r.ok||d.error)throw new Error(d.error||r.statusText);return d}
function cfgFromUi(){return{
  listen_addr: q('listen').value,
  aggregator_addr: q('agg').value,
  chunk_size: parseInt(q('chunk').value||'0',10),
  dial_timeout_sec: parseInt(q('dial').value||'0',10),
  reconnect_sec: parseInt(q('reconnect').value||'0',10),
  read_buffer_size: 32768,
  inbound_depth: 4096,
  dispatch_retries: 0,
  tunnels:[
    {label:'tunnel-1',socks5_addr:q('t1').value,weight:1},
    {label:'tunnel-2',socks5_addr:q('t2').value,weight:1},
    {label:'tunnel-3',socks5_addr:q('t3').value,weight:1},
    {label:'tunnel-4',socks5_addr:q('t4').value,weight:1},
    {label:'tunnel-5',socks5_addr:q('t5').value,weight:1},
  ],
}}
function cfgToUi(c){
  q('listen').value=c.listen_addr||''; q('agg').value=c.aggregator_addr||'';
  q('chunk').value=c.chunk_size||''; q('dial').value=c.dial_timeout_sec||''; q('reconnect').value=c.reconnect_sec||'';
  const t=c.tunnels||[];
  q('t1').value=t[0]?.socks5_addr||''; q('t2').value=t[1]?.socks5_addr||''; q('t3').value=t[2]?.socks5_addr||'';
  q('t4').value=t[3]?.socks5_addr||''; q('t5').value=t[4]?.socks5_addr||'';
}
function q(id){return document.getElementById(id)}
async function refreshAll(){
  try{
    const [cfg, st, logs] = await Promise.all([j('/api/config'), j('/api/status'), j('/api/logs?max=400')]);
    cfgToUi(cfg); q('status').textContent = st.line; q('logs').value=(logs.logs||[]).join('\n');
  }catch(e){q('status').textContent='Error: '+e.message}
}
async function applyConfig(){await j('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(cfgFromUi())})}
async function startApp(){try{await applyConfig();await j('/api/start',{method:'POST'});await refreshAll()}catch(e){alert(e.message)}}
async function stopApp(){try{await j('/api/stop',{method:'POST'});await refreshAll()}catch(e){alert(e.message)}}
async function saveConfig(){try{await applyConfig();await j('/api/config/save',{method:'POST'});alert('saved')}catch(e){alert(e.message)}}
async function loadConfig(){try{const r=await j('/api/config/load',{method:'POST'});cfgToUi(r.config);await refreshAll()}catch(e){alert(e.message)}}
setInterval(refreshAll,2000); refreshAll();
</script>
</body></html>`
