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
        callback(data);
        closeContextMenu();
    });
}

document.addEventListener('keydown', function(e) {
    if (e.key === 'Delete' || e.key === 'Backspace') {
        if (document.activeElement && document.activeElement.tagName === 'INPUT') return;
        network.deleteSelected();
    }
});

network.on("hold", function(params) {
    if (params.nodes.length > 0 || params.edges.length > 0) {
        network.selectNodes(params.nodes);
        network.selectEdges(params.edges);

        var menu = document.getElementById('context-menu');
        menu.style.display = 'block';
        menu.style.left = params.pointer.DOM.x + 'px';
        menu.style.top = params.pointer.DOM.y + 'px';
    }
});

network.on("click", function(params) { closeContextMenu(); });
network.on("dragStart", function(params) { closeContextMenu(); });

function closeContextMenu() {
    document.getElementById('context-menu').style.display = 'none';
}

function executeDelete() {
    network.deleteSelected();
    closeContextMenu();
}

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
