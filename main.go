package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/go-ping/ping"
	"github.com/gorilla/websocket"
)

type Device struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IP          string `json:"ip"`
	Description string `json:"description"`
	Type        string `json:"type"` // "device" または "joint" (中継点)
}

type Cable struct {
	ID          string `json:"id"`
	From        string `json:"from"`
	To          string `json:"to"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type NetworkData struct {
	Devices map[string]*Device `json:"devices"`
	Cables  []*Cable           `json:"cables"`
}

const dataFile = "network_config.json"

var (
	devices = map[string]*Device{
		"dev-1":   {ID: "dev-1", Name: "Main Router", IP: "127.0.0.1", Description: "Main", Type: "device"},
		"dev-2":   {ID: "dev-2", Name: "Google DNS", IP: "8.8.8.8", Description: "Internet", Type: "device"},
		"dev-3":   {ID: "dev-3", Name: "Dead Server", IP: "192.0.2.1", Description: "Backup", Type: "device"},
		"joint-1": {ID: "joint-1", Name: "Hub-A", Type: "joint"},
	}
	cables = []*Cable{
		{ID: "cable-1", From: "dev-1", To: "joint-1", Description: "", Status: "unknown"},
		{ID: "cable-2", From: "joint-1", To: "dev-2", Description: "1G", Status: "unknown"},
		{ID: "cable-3", From: "dev-1", To: "dev-3", Description: "10G", Status: "unknown"},
	}

	dataMutex sync.RWMutex

	clients   = make(map[*websocket.Conn]bool)
	clientsMu sync.Mutex
	upgrader  = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func isLocalIP(ipStr string) bool {
	if ipStr == "127.0.0.1" || ipStr == "localhost" { return true }
	ip := net.ParseIP(ipStr)
	if ip == nil { return false }
	if ip.IsLoopback() { return true }
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ipnet.IP.Equal(ip) { return true }
			}
		}
	}
	return false
}

func saveToFile() {
	dataMutex.RLock()
	defer dataMutex.RUnlock()
	data := NetworkData{Devices: devices, Cables: cables}
	file, err := os.Create(dataFile)
	if err == nil {
		defer file.Close()
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		encoder.Encode(data)
	}
}

func loadFromFile() {
	file, err := os.Open(dataFile)
	if err != nil {
		if os.IsNotExist(err) { saveToFile() }
		return
	}
	defer file.Close()
	var data NetworkData
	if err := json.NewDecoder(file).Decode(&data); err == nil {
		dataMutex.Lock()
		devices = data.Devices
		cables = data.Cables
		dataMutex.Unlock()
	}
}

func main() {
	loadFromFile()
	go startPingMonitor()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})
	http.HandleFunc("/ws", handleConnections)

	http.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		dataMutex.RLock()
		defer dataMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"devices": devices, "cables": cables})
	})

	http.HandleFunc("/api/device", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { return }
		var d Device
		json.NewDecoder(r.Body).Decode(&d)
		dataMutex.Lock()
		devices[d.ID] = &d
		dataMutex.Unlock()
		saveToFile()
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/api/cable", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { return }
		var c Cable
		json.NewDecoder(r.Body).Decode(&c)
		dataMutex.Lock()
		cables = append(cables, &c)
		dataMutex.Unlock()
		saveToFile()
		w.WriteHeader(http.StatusOK)
	})

	// ★ 削除用API (デバイスとそれに紐づくケーブルを一括削除)
	http.HandleFunc("/api/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { return }
		var req struct {
			Nodes []string `json:"nodes"`
			Edges []string `json:"edges"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		dataMutex.Lock()
		// デバイスの削除
		for _, id := range req.Nodes {
			delete(devices, id)
		}
		// ケーブルの削除
		if len(req.Edges) > 0 {
			edgeMap := make(map[string]bool)
			for _, id := range req.Edges {
				edgeMap[id] = true
			}
			var newCables []*Cable
			for _, c := range cables {
				if !edgeMap[c.ID] {
					newCables = append(newCables, c)
				}
			}
			cables = newCables
		}
		dataMutex.Unlock()
		
		saveToFile()
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/api/load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { return }
		var data NetworkData
		if err := json.NewDecoder(r.Body).Decode(&data); err == nil {
			dataMutex.Lock()
			devices = data.Devices
			cables = data.Cables
			dataMutex.Unlock()
			saveToFile()
			w.WriteHeader(http.StatusOK)
		}
	})

	fmt.Println("サーバーを起動しました: http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil { return }
	defer ws.Close()

	clientsMu.Lock()
	clients[ws] = true
	clientsMu.Unlock()

	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			clientsMu.Lock()
			delete(clients, ws)
			clientsMu.Unlock()
			break
		}
	}
}

func startPingMonitor() {
	for {
		dataMutex.RLock()
		adj := make(map[string][]*Cable)
		for _, c := range cables {
			adj[c.From] = append(adj[c.From], c)
			adj[c.To] = append(adj[c.To], c)
		}

		type targetInfo struct{ CableID, IP, Status string }
		var targets []targetInfo

		for _, cable := range cables {
			targetIP := ""
			visited := make(map[string]bool)
			queue := []string{cable.To, cable.From}
			
			for len(queue) > 0 {
				curr := queue[0]
				queue = queue[1:]
				
				if visited[curr] { continue }
				visited[curr] = true
				
				dev, exists := devices[curr]
				if exists && dev.IP != "" && !isLocalIP(dev.IP) {
					targetIP = dev.IP
					break
				}
				
				for _, c := range adj[curr] {
					if c.From == curr { queue = append(queue, c.To) }
					if c.To == curr { queue = append(queue, c.From) }
				}
			}

			if targetIP != "" {
				targets = append(targets, targetInfo{CableID: cable.ID, IP: targetIP, Status: cable.Status})
			}
		}
		dataMutex.RUnlock()

		for _, t := range targets {
			isOnline := doPing(t.IP)
			newStatus := "offline"
			if isOnline { newStatus = "online" }

			log.Printf("[Ping Check] IP: %-15s | ケーブル: %-10s | 結果: %s\n", t.IP, t.CableID, newStatus)

			dataMutex.Lock()
			for _, c := range cables {
				if c.ID == t.CableID {
					if c.Status != newStatus {
						c.Status = newStatus
						log.Printf(">>> 【状態変化】 ケーブル %s が %s になりました\n", c.ID, newStatus)
					}
					broadcast(c)
					break
				}
			}
			dataMutex.Unlock()
		}
		time.Sleep(10 * time.Second)
	}
}

func doPing(ip string) bool {
	pinger, err := ping.NewPinger(ip)
	if err != nil { return false }
	if runtime.GOOS == "windows" { pinger.SetPrivileged(true) }
	pinger.Count = 1
	pinger.Timeout = 1 * time.Second
	if err := pinger.Run(); err != nil { return false }
	return pinger.Statistics().PacketsRecv > 0
}

func broadcast(cable *Cable) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	msg, _ := json.Marshal(map[string]string{"cable_id": cable.ID, "status": cable.Status})
	for client := range clients {
		client.WriteMessage(websocket.TextMessage, msg)
	}
}

// --- フロントエンド (HTML + CSS + JavaScript) ---
const indexHTML = `
<!DOCTYPE html>
<html lang="ja">
<head>
    <meta charset="UTF-8">
    <title>Network Dashboard</title>
    <script type="text/javascript" src="https://unpkg.com/vis-network/standalone/umd/vis-network.min.js"></script>
    <style>
        html, body { font-family: sans-serif; margin: 0; padding: 0; width: 100vw; height: 100vh; overflow: hidden; background-color: white; }
        .dashboard-container { position: relative; width: 100%; height: 100%; }
        .title { position: absolute; top: 15px; left: 20px; font-size: 18px; font-weight: bold; z-index: 10; border-bottom: 2px solid #8cc63f; padding-bottom: 2px;}
        .btn-info, .btn-add { position: absolute; right: 20px; width: 30px; height: 30px; border: 2px solid #333; border-radius: 6px; background: white; font-weight: bold; cursor: pointer; z-index: 10;}
        .btn-info { top: 15px; font-family: serif;}
        .btn-add { bottom: 15px; font-size: 18px;}
        #mynetwork { width: 100%; height: 100%; outline: none; cursor: crosshair;}
        
        .modal { display: none; position: absolute; background: white; border: 2px solid #333; border-radius: 8px; padding: 15px; z-index: 100; box-shadow: 2px 2px 10px rgba(0,0,0,0.1);}
        .modal-info { top: 55px; right: 20px; width: 200px; }
        .modal-add { bottom: 55px; right: 20px; width: 180px; }
        .modal ul { list-style: none; padding: 0; margin: 0; }
        .modal ul li { padding: 8px 0; border-bottom: 1px solid #eee; cursor: pointer; }
        .modal ul li:hover { background-color: #f9f9f9; }
        .modal ul li:last-child { border-bottom: none; }
        .close-btn { text-align: right; cursor: pointer; font-weight: bold; margin-bottom: 10px; display: block;}

        .form-overlay { display: none; position: fixed; top: 0; left: 0; width: 100vw; height: 100vh; background: rgba(0,0,0,0.5); z-index: 1000; }
        .form-modal { position: absolute; top: 50%; left: 50%; transform: translate(-50%, -50%); background: white; padding: 25px; border-radius: 8px; box-shadow: 0 4px 20px rgba(0,0,0,0.3); width: 320px; border: 2px solid #333; }
        .form-modal h3 { margin-top: 0; border-bottom: 3px solid #8cc63f; padding-bottom: 8px; font-size: 18px; color: #333; }
        .form-group { margin-bottom: 15px; }
        .form-group label { display: block; font-size: 13px; margin-bottom: 5px; font-weight: bold; color: #555; }
        .form-group input { width: 100%; padding: 10px; box-sizing: border-box; border: 1px solid #ccc; border-radius: 4px; font-size: 14px; outline: none; }
        .form-group input:focus { border-color: #8cc63f; box-shadow: 0 0 5px rgba(140, 198, 63, 0.5); }
        .form-actions { text-align: right; margin-top: 25px; }
        .btn-save { background: #8cc63f; color: white; border: none; padding: 10px 20px; border-radius: 4px; cursor: pointer; font-weight: bold; font-size: 14px; transition: background 0.2s; }
        .btn-save:hover { background: #7ab332; }
        .btn-cancel { background: #f0f0f0; color: #333; border: 1px solid #ccc; padding: 10px 20px; border-radius: 4px; cursor: pointer; margin-right: 10px; font-size: 14px; }
        .btn-cancel:hover { background: #e0e0e0; }

        /* 長押し・右クリック時のコンテキストメニュー(削除ボタン) */
        .context-menu {
            display: none; position: absolute; background: white; border: 2px solid #333; 
            box-shadow: 2px 2px 10px rgba(0,0,0,0.2); z-index: 1000; border-radius: 6px; overflow: hidden;
        }
        .context-menu div { padding: 10px 20px; cursor: pointer; color: #d9534f; font-weight: bold; font-size: 14px; }
        .context-menu div:hover { background: #f9f9f9; }
    </style>
</head>
<body>
    <div class="dashboard-container">
        <div class="title">Network Dashboard v1.0</div>
        <button class="btn-info" onclick="toggleMenu('info-modal')">i</button>
        <button class="btn-add" onclick="toggleMenu('add-modal')">+</button>
        <div id="mynetwork"></div>

        <div id="context-menu" class="context-menu">
            <div onclick="executeDelete()">🗑️ Delete (削除)</div>
        </div>

        <div id="info-modal" class="modal modal-info">
            <span class="close-btn" onclick="toggleMenu('info-modal')">✖</span>
            <ul>
                <li>License</li>
                <li onclick="backupData()">Backup</li>
                <li onclick="document.getElementById('fileInput').click()">Load</li>
            </ul>
        </div>

        <div id="add-modal" class="modal modal-add">
            <span class="close-btn" onclick="toggleMenu('add-modal')">✖</span>
            <ul>
                <li onclick="addDeviceMode()">Add device</li>
                <li onclick="addJointMode()">Add joint (中継点)</li>
                <li onclick="addCableMode()">Add cable</li>
            </ul>
        </div>
    </div>
    
    <input type="file" id="fileInput" style="display: none;" accept=".json" onchange="loadData(event)">

    <div id="form-overlay" class="form-overlay">
        <div id="form-device" class="form-modal" style="display:none;">
            <h3>Add New Device</h3>
            <div class="form-group"><label>Device Name</label><input type="text" id="input-dev-name" autocomplete="off"></div>
            <div class="form-group"><label>IP Address</label><input type="text" id="input-dev-ip" autocomplete="off"></div>
            <div class="form-group"><label>Description</label><input type="text" id="input-dev-desc" autocomplete="off"></div>
            <div class="form-actions">
                <button class="btn-cancel" onclick="cancelCustomForm()">Cancel</button>
                <button class="btn-save" onclick="submitDeviceForm()">Add</button>
            </div>
        </div>
        <div id="form-joint" class="form-modal" style="display:none;">
            <h3>Add Joint (曲がり角)</h3>
            <div class="form-group"><label>Label (省略可)</label><input type="text" id="input-joint-label" autocomplete="off"></div>
            <div class="form-actions">
                <button class="btn-cancel" onclick="cancelCustomForm()">Cancel</button>
                <button class="btn-save" onclick="submitJointForm()">Add</button>
            </div>
        </div>
        <div id="form-cable" class="form-modal" style="display:none;">
            <h3>Connect Cable</h3>
            <div class="form-group"><label>Description (省略可)</label><input type="text" id="input-cable-desc" autocomplete="off"></div>
            <div class="form-actions">
                <button class="btn-cancel" onclick="cancelCustomForm()">Cancel</button>
                <button class="btn-save" onclick="submitCableForm()">Connect</button>
            </div>
        </div>
    </div>

    <script type="text/javascript">
        function createSvgNode(name, ip) {
            var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="160" height="60">' +
                '<rect x="1" y="1" width="158" height="58" rx="5" ry="5" fill="white" stroke="black" stroke-width="1.5"/>' +
                '<rect x="8" y="10" width="35" height="40" rx="3" ry="3" fill="none" stroke="black" stroke-width="1"/>' +
                '<text x="25.5" y="34" font-family="sans-serif" font-size="10" text-anchor="middle" fill="black">icon</text>' +
                '<text x="55" y="24" font-family="sans-serif" font-size="12" font-weight="bold" fill="black">' + name + '</text>' +
                '<line x1="55" y1="30" x2="150" y2="30" stroke="black" stroke-width="1"/>' +
                '<text x="55" y="46" font-family="sans-serif" font-size="12" fill="black">' + ip + '</text>' +
            '</svg>';
            return "data:image/svg+xml;charset=utf-8," + encodeURIComponent(svg);
        }

        var nodes = new vis.DataSet();
        var edges = new vis.DataSet();
        var isAddingJoint = false;
        var pendingData = null;
        var pendingCallback = null;

        fetch('/api/data').then(res => res.json()).then(data => {
            if (data.devices) {
                Object.values(data.devices).forEach(dev => {
                    if (dev.type === "joint") {
                        nodes.add({ id: dev.id, shape: 'dot', size: 5, color: '#333', label: dev.name, font: { size: 12, color: '#555', background: 'rgba(255, 255, 255, 0.8)' }});
                    } else {
                        nodes.add({ id: dev.id, image: createSvgNode(dev.name, dev.ip), shape: 'image', label: dev.description, font: { size: 14, color: 'black' } });
                    }
                });
            }
            if (data.cables) {
                data.cables.forEach(cable => {
                    edges.add({ id: cable.id, from: cable.from, to: cable.to, label: cable.description, color: { color: "red", highlight: "red" }, font: { align: 'middle', background: 'white' }, width: 2 });
                });
            }
        });

        var container = document.getElementById('mynetwork');
        var data = { nodes: nodes, edges: edges };
        
        var options = {
            physics: false,
            edges: { smooth: false },
            manipulation: {
                enabled: false,
                addNode: function(data, callback) {
                    pendingData = data;
                    pendingCallback = callback;
                    document.getElementById('form-overlay').style.display = 'block';
                    
                    if (isAddingJoint) {
                        document.getElementById('form-joint').style.display = 'block';
                        document.getElementById('input-joint-label').value = "";
                        document.getElementById('input-joint-label').focus();
                    } else {
                        document.getElementById('form-device').style.display = 'block';
                        document.getElementById('input-dev-name').value = "New Device";
                        document.getElementById('input-dev-ip').value = "192.168.1.1";
                        document.getElementById('input-dev-desc').value = "";
                        document.getElementById('input-dev-name').focus();
                    }
                },
                addEdge: function(data, callback) {
                    if (data.from === data.to) { callback(null); return; }
                    pendingData = data;
                    pendingCallback = callback;
                    document.getElementById('form-overlay').style.display = 'block';
                    document.getElementById('form-cable').style.display = 'block';
                    document.getElementById('input-cable-desc').value = "";
                    document.getElementById('input-cable-desc').focus();
                },
                // ★ 削除時の処理 (APIにリクエストを投げてからキャンバスから消す)
                deleteNode: function(data, callback) { deleteFromBackend(data, callback); },
                deleteEdge: function(data, callback) { deleteFromBackend(data, callback); }
            }
        };
        var network = new vis.Network(container, data, options);

        function deleteFromBackend(data, callback) {
            fetch('/api/delete', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ nodes: data.nodes || [], edges: data.edges || [] })
            }).then(() => {
                callback(data); // 成功したらキャンバスからも削除
                closeContextMenu();
            });
        }

        // --- キーボードの Delete / Backspace キーに対応 ---
        document.addEventListener('keydown', function(e) {
            if (e.key === 'Delete' || e.key === 'Backspace') {
                // 入力フォーム操作中は削除を発動しない
                if (document.activeElement && document.activeElement.tagName === 'INPUT') return;
                network.deleteSelected(); 
            }
        });

        // --- 長押し (ホールド) に対応 ---
        network.on("hold", function(params) {
            if (params.nodes.length > 0 || params.edges.length > 0) {
                // ホールドされた要素を選択状態にする
                network.selectNodes(params.nodes);
                network.selectEdges(params.edges);

                // メニューをポインターの場所に表示
                var menu = document.getElementById('context-menu');
                menu.style.display = 'block';
                menu.style.left = params.pointer.DOM.x + 'px';
                menu.style.top = params.pointer.DOM.y + 'px';
            }
        });

        // 普通のクリック等でメニューを閉じる
        network.on("click", function(params) { closeContextMenu(); });
        network.on("dragStart", function(params) { closeContextMenu(); });

        function closeContextMenu() {
            document.getElementById('context-menu').style.display = 'none';
        }

        function executeDelete() {
            network.deleteSelected(); // これを呼ぶと options 内の deleteNode / deleteEdge が発動する
            closeContextMenu();
        }

        // --- フォーム送信処理 ---
        function submitDeviceForm() {
            var name = document.getElementById('input-dev-name').value;
            var ip = document.getElementById('input-dev-ip').value;
            var desc = document.getElementById('input-dev-desc').value;
            pendingData.id = "dev-" + new Date().getTime();
            pendingData.image = createSvgNode(name, ip);
            pendingData.shape = 'image';
            pendingData.label = desc;
            pendingData.font = { size: 14, color: 'black' };

            fetch('/api/device', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id: pendingData.id, name: name, ip: ip, description: desc, type: "device" })
            }).then(() => { pendingCallback(pendingData); closeCustomForm(); });
        }

        function submitJointForm() {
            var label = document.getElementById('input-joint-label').value;
            pendingData.id = "joint-" + new Date().getTime();
            pendingData.shape = 'dot';
            pendingData.size = 5;
            pendingData.color = '#333';
            if (label !== "") {
                pendingData.label = label;
                pendingData.font = { size: 12, color: '#555', background: 'rgba(255, 255, 255, 0.8)' };
            }
            fetch('/api/device', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id: pendingData.id, name: label, type: "joint" })
            }).then(() => { pendingCallback(pendingData); closeCustomForm(); });
        }

        function submitCableForm() {
            var desc = document.getElementById('input-cable-desc').value;
            pendingData.id = "cable-" + new Date().getTime();
            pendingData.label = desc;
            pendingData.color = { color: "red", highlight: "red" };
            pendingData.font = { align: 'middle', background: 'white' };
            pendingData.width = 2;
            fetch('/api/cable', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id: pendingData.id, from: pendingData.from, to: pendingData.to, description: desc, status: "unknown" })
            }).then(() => { pendingCallback(pendingData); closeCustomForm(); });
        }

        function cancelCustomForm() {
            if (pendingCallback) pendingCallback(null);
            closeCustomForm();
        }

        function closeCustomForm() {
            document.getElementById('form-overlay').style.display = 'none';
            document.getElementById('form-device').style.display = 'none';
            document.getElementById('form-joint').style.display = 'none';
            document.getElementById('form-cable').style.display = 'none';
            pendingData = null;
            pendingCallback = null;
        }

        function connectWebSocket() {
            var loc = window.location;
            var wsUri = (loc.protocol === "https:" ? "wss:" : "ws:") + "//" + loc.host + "/ws";
            var socket = new WebSocket(wsUri);
            socket.onmessage = function(event) {
                var msg = JSON.parse(event.data);
                if (msg.status === "offline") {
                    edges.update({ id: msg.cable_id, color: { color: "#CCCCCC" }, dashes: true });
                } else if (msg.status === "online") {
                    edges.update({ id: msg.cable_id, color: { color: "red" }, dashes: false });
                }
            };
            socket.onclose = function(e) { setTimeout(connectWebSocket, 3000); };
        }
        connectWebSocket();

        function toggleMenu(id) {
            var modal = document.getElementById(id);
            modal.style.display = (modal.style.display === "block") ? "none" : "block";
        }

        function addDeviceMode() { isAddingJoint = false; toggleMenu('add-modal'); network.addNodeMode(); }
        function addJointMode() { isAddingJoint = true; toggleMenu('add-modal'); network.addNodeMode(); }
        function addCableMode() { toggleMenu('add-modal'); network.addEdgeMode(); }

        function backupData() {
            fetch('/api/data').then(res => res.blob()).then(blob => {
                const url = window.URL.createObjectURL(blob);
                const a = document.createElement('a');
                a.style.display = 'none';
                a.href = url;
                a.download = 'network_backup.json';
                document.body.appendChild(a);
                a.click();
                window.URL.revokeObjectURL(url);
                toggleMenu('info-modal');
            });
        }

        function loadData(event) {
            const file = event.target.files[0];
            if (!file) return;
            const reader = new FileReader();
            reader.onload = function(e) {
                fetch('/api/load', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: e.target.result
                }).then(response => {
                    if (response.ok) { location.reload(); } 
                    else { alert("無効なファイル形式です。"); }
                });
            };
            reader.readAsText(file);
            toggleMenu('info-modal');
        }
    </script>
</body>
</html>
`