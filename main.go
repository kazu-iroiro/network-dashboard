package main

import (
	"encoding/json"
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

    http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" {
            http.NotFound(w, r)
            return
        }
        http.ServeFile(w, r, "./static/index.html")
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

	log.Println("サーバーを起動しました: http://localhost:8080")
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
