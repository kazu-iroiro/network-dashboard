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
		"dev-1": {ID: "dev-1", Name: "Main Router", IP: "127.0.0.1", Description: "Main"},
		"dev-2": {ID: "dev-2", Name: "Google DNS", IP: "8.8.8.8", Description: "Internet Gateway"},
		"dev-3": {ID: "dev-3", Name: "Dead Server", IP: "192.0.2.1", Description: "Backup Storage"},
	}
	cables = []*Cable{
		{ID: "cable-1", From: "dev-1", To: "dev-2", Description: "1G", Status: "unknown"},
		{ID: "cable-2", From: "dev-1", To: "dev-3", Description: "10G", Status: "unknown"},
	}

	dataMutex sync.RWMutex

	clients   = make(map[*websocket.Conn]bool)
	clientsMu sync.Mutex
	upgrader  = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

// 指定されたIPが、監視サーバー自身(ローカルホストまたは自身のNICのIP)か判定
func isLocalIP(ipStr string) bool {
	if ipStr == "127.0.0.1" || ipStr == "localhost" {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ipnet.IP.Equal(ip) {
					return true
				}
			}
		}
	}
	return false
}

// JSONファイルに現在の状態を保存
func saveToFile() {
	dataMutex.RLock()
	defer dataMutex.RUnlock()

	data := NetworkData{Devices: devices, Cables: cables}
	file, err := os.Create(dataFile)
	if err != nil {
		log.Println("保存エラー:", err)
		return
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encoder.Encode(data)
}

// JSONファイルから状態を復元
func loadFromFile() {
	file, err := os.Open(dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("設定ファイルが存在しないため、デフォルトデータを使用し新規作成します。")
			saveToFile()
		}
		return
	}
	defer file.Close()

	var data NetworkData
	if err := json.NewDecoder(file).Decode(&data); err == nil {
		dataMutex.Lock()
		devices = data.Devices
		cables = data.Cables
		dataMutex.Unlock()
		log.Println("設定ファイル (network_config.json) からデータをロードしました。")
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

	http.HandleFunc("/api/load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { return }
		var data NetworkData
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, "無効なJSONデータです", http.StatusBadRequest)
			return
		}
		
		dataMutex.Lock()
		devices = data.Devices
		cables = data.Cables
		dataMutex.Unlock()
		
		saveToFile()
		w.WriteHeader(http.StatusOK)
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
		type targetInfo struct{ CableID, IP, Status string }
		var targets []targetInfo

		dataMutex.RLock()
		for _, cable := range cables {
			toDev, toExists := devices[cable.To]
			fromDev, fromExists := devices[cable.From]

			if toExists && toDev.IP != "" {
				targetIP := toDev.IP

				// Toが監視サーバー自身の場合は、Fromに向けてPingを打つ
				if isLocalIP(targetIP) {
					if fromExists && fromDev.IP != "" && !isLocalIP(fromDev.IP) {
						targetIP = fromDev.IP
					} else {
						continue // 両方ローカル等の場合はスキップ
					}
				}
				targets = append(targets, targetInfo{CableID: cable.ID, IP: targetIP, Status: cable.Status})
			}
		}
		dataMutex.RUnlock()

		for _, t := range targets {
			isOnline := doPing(t.IP)
			newStatus := "offline"
			if isOnline {
				newStatus = "online"
			}

			// 継続的なログ出力
			log.Printf("[Ping Check] IP: %-15s | ケーブル: %-10s | 結果: %s\n", t.IP, t.CableID, newStatus)

			dataMutex.Lock()
			for _, c := range cables {
				if c.ID == t.CableID {
					if c.Status != newStatus {
						c.Status = newStatus
						log.Printf(">>> 【状態変化】 ケーブル %s が %s になりました\n", c.ID, newStatus)
					}
					// 毎回フロントエンドに最新状態を送信
					broadcast(c)
					break
				}
			}
			dataMutex.Unlock()
		}
		time.Sleep(30 * time.Second)
	}
}

// go-pingパッケージを使った正確なICMP通信
func doPing(ip string) bool {
	pinger, err := ping.NewPinger(ip)
	if err != nil {
		log.Printf("Ping初期化エラー (%s): %v\n", ip, err)
		return false
	}

	// Windows環境でネイティブICMPパケットを送信するには特権（管理者権限）が必要
	if runtime.GOOS == "windows" {
		pinger.SetPrivileged(true)
	}

	pinger.Count = 1
	pinger.Timeout = 1 * time.Second

	err = pinger.Run()
	if err != nil {
		return false
	}

	stats := pinger.Statistics()
	return stats.PacketsRecv > 0
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
        /* 画面全体を使うためのリセットとフルスクリーン設定 */
        html, body { 
            font-family: sans-serif; 
            margin: 0; 
            padding: 0; 
            width: 100vw; 
            height: 100vh; 
            overflow: hidden; /* スクロールバーを消す */
            background-color: white; 
        }
        
        /* コンテナを画面いっぱいに広げる */
        .dashboard-container { 
            position: relative; 
            width: 100%; 
            height: 100%; 
        }
        
        /* UIパーツの配置（画面の四隅からの絶対配置なのでフルスクリーンでも崩れません） */
        .title { position: absolute; top: 15px; left: 20px; font-size: 18px; font-weight: bold; z-index: 10; border-bottom: 2px solid #8cc63f; padding-bottom: 2px;}
        .btn-info, .btn-add { position: absolute; right: 20px; width: 30px; height: 30px; border: 2px solid #333; border-radius: 6px; background: white; font-weight: bold; cursor: pointer; z-index: 10;}
        .btn-info { top: 15px; font-family: serif;}
        .btn-add { bottom: 15px; font-size: 18px;}
        
        /* ネットワークキャンバス本体 */
        #mynetwork { width: 100%; height: 100%; outline: none; cursor: crosshair;}
        
        .modal { display: none; position: absolute; background: white; border: 2px solid #333; border-radius: 8px; padding: 15px; z-index: 100; box-shadow: 2px 2px 10px rgba(0,0,0,0.1);}
        .modal-info { top: 55px; right: 20px; width: 200px; }
        .modal-add { bottom: 55px; right: 20px; width: 150px; }
        .modal ul { list-style: none; padding: 0; margin: 0; }
        .modal ul li { padding: 8px 0; border-bottom: 1px solid #eee; cursor: pointer; }
        .modal ul li:hover { background-color: #f9f9f9; }
        .modal ul li:last-child { border-bottom: none; }
        .close-btn { text-align: right; cursor: pointer; font-weight: bold; margin-bottom: 10px; display: block;}
    </style>
</head>
<body>
    <div class="dashboard-container">
        <div class="title">Network Dashboard v1.4</div>
        <button class="btn-info" onclick="toggleModal('info-modal')">i</button>
        <button class="btn-add" onclick="toggleModal('add-modal')">+</button>
        
        <div id="mynetwork"></div>

        <div id="info-modal" class="modal modal-info">
            <span class="close-btn" onclick="toggleModal('info-modal')">✖</span>
            <ul>
                <li>License</li>
                <li onclick="backupData()">Backup</li>
                <li onclick="document.getElementById('fileInput').click()">Load</li>
                <li>How to use</li>
                <li>github</li>
            </ul>
        </div>

        <div id="add-modal" class="modal modal-add">
            <span class="close-btn" onclick="toggleModal('add-modal')">✖</span>
            <ul>
                <li onclick="addDeviceMode()">Add device</li>
                <li onclick="addCableMode()">Add cable</li>
            </ul>
        </div>
    </div>
    
    <input type="file" id="fileInput" style="display: none;" accept=".json" onchange="loadData(event)">
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

        fetch('/api/data').then(res => res.json()).then(data => {
            if (data.devices) {
                Object.values(data.devices).forEach(dev => {
                    nodes.add({ id: dev.id, image: createSvgNode(dev.name, dev.ip), shape: 'image', label: dev.description, font: { size: 14, color: 'black' } });
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
            edges: { smooth: { type: 'continuous' } },
            manipulation: {
                enabled: false,
                addNode: function(data, callback) {
                    var name = prompt("デバイス名:", "New Device");
                    if (!name) { callback(null); return; }
                    var ip = prompt("IPアドレス:", "192.168.1.1");
                    var desc = prompt("説明(Description):", "Switch");

                    data.id = "dev-" + new Date().getTime();
                    data.image = createSvgNode(name, ip);
                    data.shape = 'image';
                    data.label = desc;
                    data.font = { size: 14, color: 'black' };

                    fetch('/api/device', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ id: data.id, name: name, ip: ip, description: desc })
                    }).then(() => callback(data));
                },
                addEdge: function(data, callback) {
                    if (data.from === data.to) { callback(null); return; }
                    var desc = prompt("ケーブルの説明 (例: 10G, Giga):", "1G");
                    if (!desc) { callback(null); return; }

                    data.id = "cable-" + new Date().getTime();
                    data.label = desc;
                    data.color = { color: "red", highlight: "red" };
                    data.font = { align: 'middle', background: 'white' };
                    data.width = 2;

                    fetch('/api/cable', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ id: data.id, from: data.from, to: data.to, description: desc, status: "unknown" })
                    }).then(() => callback(data));
                }
            }
        };
        var network = new vis.Network(container, data, options);

        function connectWebSocket() {
            var loc = window.location;
            var wsUri = (loc.protocol === "https:" ? "wss:" : "ws:") + "//" + loc.host + "/ws";
            var socket = new WebSocket(wsUri);

            socket.onopen = function() {
                console.log("WebSocket 接続成功");
            };

            socket.onmessage = function(event) {
                var msg = JSON.parse(event.data);
                
                // JS側の継続的なログ出力
                var now = new Date().toLocaleTimeString();
                console.log("[" + now + "] 監視ログ受信 - ケーブル: " + msg.cable_id + " | 状態: " + msg.status);
                
                if (msg.status === "offline") {
                    edges.update({ id: msg.cable_id, color: { color: "#CCCCCC" }, dashes: true });
                } else if (msg.status === "online") {
                    edges.update({ id: msg.cable_id, color: { color: "red" }, dashes: false });
                }
            };

            socket.onclose = function(e) {
                console.log("WebSocket 切断。3秒後に再接続を試みます...");
                setTimeout(function() {
                    connectWebSocket();
                }, 3000);
            };

            socket.onerror = function(err) {
                console.error("WebSocket エラー:", err);
                socket.close();
            };
        }

        connectWebSocket();

        function toggleModal(id) {
            var modal = document.getElementById(id);
            modal.style.display = (modal.style.display === "block") ? "none" : "block";
        }

        function addDeviceMode() { toggleModal('add-modal'); network.addNodeMode(); }
        function addCableMode() { toggleModal('add-modal'); network.addEdgeMode(); }

        function backupData() {
            fetch('/api/data')
                .then(res => res.blob())
                .then(blob => {
                    const url = window.URL.createObjectURL(blob);
                    const a = document.createElement('a');
                    a.style.display = 'none';
                    a.href = url;
                    a.download = 'network_backup.json';
                    document.body.appendChild(a);
                    a.click();
                    window.URL.revokeObjectURL(url);
                    toggleModal('info-modal');
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
                    if (response.ok) {
                        alert("データをロードしました。ページを再読み込みします。");
                        location.reload();
                    } else {
                        alert("無効なファイル形式です。");
                    }
                });
            };
            reader.readAsText(file);
            toggleModal('info-modal');
        }
    </script>
</body>
</html>
`