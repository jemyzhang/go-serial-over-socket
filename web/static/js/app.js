// ============================================================
// Theme Management
// ============================================================
const TERMINAL_THEMES = {
    dark: { background: '#0a0e17', foreground: '#f1f5f9', cursor: '#ea6645', selection: 'rgba(234, 102, 69, 0.35)' },
    light: { background: '#ffffff', foreground: '#0f172a', cursor: '#ea6645', selection: 'rgba(234, 102, 69, 0.2)' },
    blue: { background: '#0b1329', foreground: '#93c5fd', cursor: '#38bdf8', selection: 'rgba(56, 189, 248, 0.25)' },
    green: { background: '#0a1a0f', foreground: '#86efac', cursor: '#4ade80', selection: 'rgba(74, 222, 128, 0.25)' }
};

function toggleTheme() {
    const html = document.documentElement;
    const current = html.getAttribute('data-theme') || 'dark';
    const next = current === 'dark' ? 'light' : 'dark';
    html.setAttribute('data-theme', next);
    localStorage.setItem('theme', next);
    const btn = document.getElementById('themeToggle');
    if (btn) btn.textContent = next === 'dark' ? '🌙' : '☀️';
}

// Load saved theme immediately on script parse
(function () {
    const saved = localStorage.getItem('theme') || 'dark';
    document.documentElement.setAttribute('data-theme', saved);
    const btn = document.getElementById('themeToggle');
    if (btn) btn.textContent = saved === 'dark' ? '🌙' : '☀️';
})();

// ============================================================
// Toast Notification System
// ============================================================
function showToast(message, type = 'info', duration = 3000) {
    const container = document.getElementById('toastContainer');
    if (!container) return;

    const toast = document.createElement('div');
    toast.className = `toast ${type}`;
    const icons = { success: '✓', info: 'ℹ', warning: '⚠', error: '✕' };
    toast.innerHTML = `
        <div class="toast-icon">${icons[type] || icons.info}</div>
        <div class="toast-content">${message}</div>
    `;
    container.appendChild(toast);

    setTimeout(() => {
        toast.classList.add('hiding');
        setTimeout(() => { if (toast.parentNode) toast.parentNode.removeChild(toast); }, 300);
    }, duration);
}

// ============================================================
// Clipboard
// ============================================================
function copyToClipboard(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(() => {
            showToast('Copied to clipboard', 'success', 1000);
        }).catch(() => execCommandCopy(text));
    } else {
        execCommandCopy(text);
    }
}

function execCommandCopy(text) {
    const el = document.createElement('textarea');
    el.value = text;
    el.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(el);
    el.select();
    try {
        if (document.execCommand('copy')) showToast('Copied to clipboard', 'success', 1000);
    } catch (e) {}
    document.body.removeChild(el);
}

// ============================================================
// Per-Port Terminal Management
// ============================================================
// Map: portId -> { terminal, fitAddon, searchAddon, pane, xtermHost }
const portTerminals = new Map();
let activePortId = null;

// Core state
let ws = null;
let selectedPort = null;   // kept for backward compat, always equals activePortId
let portStatus = {};
let terminalConnected = false;
let isSearchActive = false;

function isPortConnected(portId) {
    if (!portId) return false;
    const info = portStatus[portId];
    return !!(info && (info.connected || info.status === 'connected'));
}

function loadTerminalSettings() {
    return {
        scrollback: parseInt(localStorage.getItem('terminal_scrollback') || '10000'),
        fontSize: parseInt(localStorage.getItem('terminal_fontsize') || '14'),
        themeName: localStorage.getItem('terminal_theme_name') || 'dark',
        fontFamily: localStorage.getItem('terminal_fontfamily') || "'Monaco', 'Menlo', monospace"
    };
}

function getActivePT() {
    return (activePortId && portTerminals.has(activePortId)) ? portTerminals.get(activePortId) : null;
}

function getOrCreatePortTerminal(portId) {
    if (portTerminals.has(portId)) return portTerminals.get(portId);

    const wrapper = document.getElementById('terminalsWrapper');
    if (!wrapper) return null;

    // Create pane container
    const pane = document.createElement('div');
    pane.className = 'terminal-pane';
    pane.id = `pane-${portId}`;

    // Create the xterm host div
    const xtermHost = document.createElement('div');
    xtermHost.className = 'xterm-host';
    xtermHost.id = `xterm-${portId}`;
    pane.appendChild(xtermHost);

    wrapper.appendChild(pane);

    // Load settings
    const settings = loadTerminalSettings();
    const theme = TERMINAL_THEMES[settings.themeName] || TERMINAL_THEMES.dark;

    // Create xterm instance
    const term = new window.Terminal({
        theme,
        fontSize: settings.fontSize,
        fontFamily: settings.fontFamily,
        cursorBlink: true,
        convertEol: true,
        scrollback: settings.scrollback,
        allowTransparency: true,
        allowProposedApi: true
    });

    let fit = null;
    let search = null;

    try {
        fit = new window.FitAddon.FitAddon();
        term.loadAddon(fit);
    } catch (e) { console.warn('FitAddon unavailable:', e); }

    try {
        search = new window.SearchAddon.SearchAddon();
        term.loadAddon(search);
    } catch (e) { console.warn('SearchAddon unavailable:', e); }

    term.open(xtermHost);
    if (fit) setTimeout(() => { try { fit.fit(); } catch(e) {} }, 50);

    // Close search on terminal click
    xtermHost.addEventListener('click', () => {
        const bar = document.getElementById('searchBar');
        if (bar && bar.style.display !== 'none') closeSearch();
    });

    // Batch xterm input and send to server
    let _sendBuf = '';
    let _sendScheduled = false;
    function flushSendBuf() {
        _sendScheduled = false;
        if (!_sendBuf) return;
        const payload = _sendBuf;
        _sendBuf = '';
        const isConn = terminalConnected || isPortConnected(portId);
        if (activePortId === portId && isConn && ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'write_data', port_id: portId, data: payload }));
        }
    }

    // Ctrl+S saves the terminal buffer (the existing export) —
    // and must never reach the device: 0x13 is XOFF, it freezes
    // the device's output until an XON arrives.
    term.attachCustomKeyEventHandler((ev) => {
        if (ev.type === 'keydown' && ev.ctrlKey && !ev.altKey && !ev.metaKey && ev.key.toLowerCase() === 's') {
            ev.preventDefault();
            saveTerminalContent();
            return false;
        }
        return true;
    });

    term.onData(data => {
        _sendBuf += data;
        if (!_sendScheduled) {
            _sendScheduled = true;
            queueMicrotask(flushSendBuf);
        }
    });

    // Auto-copy selection
    term.onSelectionChange(() => {
        const bar = document.getElementById('searchBar');
        if (bar && bar.style.display !== 'none') return;
        const sel = term.getSelection();
        if (sel) copyToClipboard(sel);
    });

    // Welcome message
    term.writeln(`\x1b[1;36mSerial Over Socket — ${portId}\x1b[0m`);
    term.writeln('\x1b[90mSelect port in sidebar and click Connect to start\x1b[0m\n');

    const entry = { terminal: term, fitAddon: fit, searchAddon: search, pane, xtermHost };
    portTerminals.set(portId, entry);
    return entry;
}

function switchToPort(portId) {
    if (!portId) return;

    // Hide current pane
    if (activePortId && portTerminals.has(activePortId)) {
        const cur = portTerminals.get(activePortId);
        cur.pane.classList.remove('active');
    }

    // Hide empty state
    const empty = document.getElementById('emptyState');
    if (empty) empty.style.display = 'none';

    // Create/activate new pane
    const pt = getOrCreatePortTerminal(portId);
    if (!pt) return;

    pt.pane.classList.add('active');
    activePortId = portId;
    selectedPort = portId;  // backward compat

    // Fit and focus
    setTimeout(() => {
        if (pt.fitAddon) try { pt.fitAddon.fit(); } catch(e) {}
        pt.terminal.focus();
    }, 50);

    // Update tab highlight
    updateTabHighlight(portId);

    // Update cmd bar port label
    updateCmdBarLabel();

    // Update sidebar port settings
    selectPortSettings(portId);

    // Notify backend
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'select_port', port_id: portId }));
    }
}

function updateTabHighlight(portId) {
    const tabsScroll = document.getElementById('tabsScroll');
    if (!tabsScroll) return;
    tabsScroll.querySelectorAll('.port-tab').forEach(tab => {
        tab.classList.toggle('active', tab.dataset.port === portId);
    });
}

function updateCmdBarLabel() {
    const dot = document.getElementById('cmdDot');
    const label = document.getElementById('cmdPortLabel');
    if (!activePortId) {
        if (dot) dot.className = 'cmd-dot';
        if (label) label.textContent = 'No port';
        return;
    }
    const info = portStatus[activePortId] || {};
    const alias = info.alias || '';
    if (label) label.textContent = alias ? `${alias} (${activePortId})` : activePortId;
    if (dot) dot.className = 'cmd-dot' + (info.connected ? ' connected' : '');
}

// ============================================================
// Tab Bar Management
// ============================================================
function updateTabBar(ports) {
    const tabsScroll = document.getElementById('tabsScroll');
    if (!tabsScroll) return;

    const portIds = Object.keys(ports).sort();

    // Remove tabs for ports that no longer exist
    tabsScroll.querySelectorAll('.port-tab').forEach(tab => {
        if (!ports[tab.dataset.port]) tab.remove();
    });

    // Add or update tabs
    portIds.forEach(portId => {
        let tab = tabsScroll.querySelector(`[data-port="${portId}"]`);
        const info = ports[portId] || {};
        const alias = info.alias || '';

        let statusClass = 'unknown';
        if (info.status === 'connected' || info.connected) statusClass = 'connected';
        else if (info.status === 'connecting') statusClass = 'connecting';
        else if (info.status === 'disconnected') statusClass = 'disconnected';

        const displayName = alias || portId;
        const subLabel = alias ? `<span class="tab-port-id">${portId}</span>` : '';

        if (!tab) {
            tab = document.createElement('button');
            tab.className = 'port-tab';
            tab.dataset.port = portId;
            tab.onclick = () => switchToPort(portId);
            tabsScroll.appendChild(tab);
        }

        tab.innerHTML = `<span class="tab-dot ${statusClass}"></span><span class="tab-name">${displayName}</span>${subLabel}`;
        tab.classList.toggle('active', portId === activePortId);
    });

    // If no active port or active port removed, pick first
    if (!activePortId || !ports[activePortId]) {
        if (portIds.length > 0) {
            switchToPort(portIds[0]);
        } else {
            activePortId = null;
            selectedPort = null;
            if (empty) empty.style.removeProperty('display');
            const empty = document.getElementById('emptyState');
            if (empty) empty.style.display = '';
        }
    } else if (selectedPort !== activePortId) {
        // The active port is still there but the session binding
        // was lost: ws.onclose clears selectedPort (and
        // terminalConnected), and after the auto-reconnect nothing
        // re-selected the port, so the terminal sat at
        // "[Not connected]" while serial data kept flowing.
        // Re-bind: switchToPort restores the selection, the
        // connected flag and the server-side select_port.
        switchToPort(activePortId);
    }
}

// ============================================================
// WebSocket
// ============================================================
function initWebSocket() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${protocol}//${window.location.host}/ws`;
    ws = new WebSocket(wsUrl);
    ws.binaryType = 'arraybuffer';

    ws.onopen = function () {
        showToast('Connected to server', 'success', 1500);
        setWsStatus(true);

        // Send terminal_ready for the active terminal
        setTimeout(() => {
            if (ws.readyState === WebSocket.OPEN) {
                const pt = getActivePT();
                ws.send(JSON.stringify({
                    type: 'terminal_ready',
                    rows: pt ? pt.terminal.rows : 24,
                    cols: pt ? pt.terminal.cols : 80
                }));
            }
        }, 100);

        refreshPorts();
    };

    function _handleBinaryFrame(bytes) {
        if (bytes.length < 1) return;
        const portIdLen = bytes[0];
        if (bytes.length < 1 + portIdLen) return;

        const portId = new TextDecoder('utf-8').decode(bytes.subarray(1, 1 + portIdLen));
        const serialData = bytes.subarray(1 + portIdLen);
        const text = new TextDecoder('utf-8', { fatal: false }).decode(serialData);

        const pt = getOrCreatePortTerminal(portId);
        if (pt) {
            pt.terminal.write(text);
        }

        if (portId === activePortId) {
            appendToLog(text);
        }
    }

    ws.onmessage = function (event) {
        if (event.data instanceof ArrayBuffer) {
            _handleBinaryFrame(new Uint8Array(event.data));
        } else if (event.data instanceof Blob) {
            event.data.arrayBuffer().then(buf => _handleBinaryFrame(new Uint8Array(buf)));
        } else {
            handleWebSocketMessage(event.data);
        }
    };

    ws.onerror = function (error) {
        console.error('WebSocket error', error);
        setWsStatus(false);
    };

    ws.onclose = function () {
        showToast('Disconnected from server', 'info', 1500);
        selectedPort = null;
        terminalConnected = false;
        setWsStatus(false);
        setTimeout(initWebSocket, 3000);
    };
}

function setWsStatus(connected) {
    const dot = document.getElementById('wsDot');
    const text = document.getElementById('wsStatusText');
    const pill = document.getElementById('sidebarStatusPill');
    if (dot) dot.className = 'status-dot' + (connected ? ' connected' : '');
    if (text) text.textContent = connected ? '已连接' : '未连接';
    if (pill) pill.classList.toggle('connected', !!connected);
}

function handleWebSocketMessage(message) {
    try {
        const data = JSON.parse(message);

        switch (data.type) {
            case 'ports':
                updatePortList(data.ports || {});
                break;

            case 'port_status': {
                const statusObj = { status: data.status, connected: data.status === 'connected' };
                showToast(`${data.port}: ${data.status}`, 'info', 2000);
                updatePortStatus(data.port, statusObj);
                if (data.port === selectedPort || data.port === activePortId) {
                    terminalConnected = statusObj.connected;
                    updateCmdBarLabel();
                }
                break;
            }

            case 'error':
                showToast(data.message || JSON.stringify(data), 'error', 4000);
                break;

            case 'response':
                if (data._ack && webdavPushWaiters[data._ack]) {
                    webdavPushWaiters[data._ack]();
                    delete webdavPushWaiters[data._ack];
                    break;
                }
                showToast(data.message || JSON.stringify(data), 'info', 3000);
                break;

            case 'welcome':
                if (data.message) showToast(data.message, 'success', 3000);
                if (data.ports) {
                    updatePortList(data.ports || {});
                    if (!selectedPort) {
                        const first = Object.keys(data.ports)[0];
                        if (first) switchToPort(first);
                    } else {
                        switchToPort(selectedPort);
                    }
                }
                if (ws && ws.readyState === WebSocket.OPEN) {
                    ws.send(JSON.stringify({ type: 'get_history' }));
                    ws.send(JSON.stringify({ type: 'get_webdav_config' }));
                }
                break;

            case 'history':
            case 'history_update':
                if (data.history && Array.isArray(data.history)) {
                    window.historyRecords = data.history;
                    commandHistory = data.history.map(item => item.command);
                    saveCommandHistory();
                    const hm = document.getElementById('historyManager');
                    if (hm && hm.style.display === 'flex') {
                        renderHistoryList();
                    }
                    renderHistoryView();
                    updateHistoryDropdown();
                }
                break;

            case 'webdav_config':
                if (data.config) {
                    const cfg = data.config;
                    if (document.getElementById('webdavUrl')) document.getElementById('webdavUrl').value = cfg.url || '';
                    if (document.getElementById('webdavUsername')) document.getElementById('webdavUsername').value = cfg.username || '';
                    // The server never returns the password — only whether one
                    // is stored. Keep the input empty: empty on save = keep it.
                    webdavPasswordIsSet = !!cfg.password_set;
                    const pwEl = document.getElementById('webdavPassword');
                    if (pwEl) { pwEl.value = ''; pwEl.placeholder = cfg.password_set ? '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022 (\u5df2\u4fdd\u5b58)' : '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022'; }
                    if (document.getElementById('webdavInterval')) document.getElementById('webdavInterval').value = cfg.interval || 30;
                    if (document.getElementById('webdavMaxBackups')) document.getElementById('webdavMaxBackups').value = cfg.max_backups || 10;
                    let localCfg = loadWebDAVConfig();
                    localCfg.url = cfg.url || localCfg.url;
                    localCfg.username = cfg.username || localCfg.username;
                    // Never touch localCfg.password here: the server doesn't send it.
                    localCfg.interval = cfg.interval || localCfg.interval;
                    localCfg.maxBackups = cfg.max_backups || localCfg.maxBackups;
                    localStorage.setItem(WEBDAV_CONFIG_KEY, JSON.stringify(localCfg));
                }
                break;

            default:
                console.log('Unknown message type:', data.type);
        }
    } catch (e) {
        console.error('Failed to parse WebSocket message:', e, message);
    }
}

// ============================================================
// Port List / Tab Updates
// ============================================================
function updatePortList(ports) {
    portStatus = ports;

    const portIds = Object.keys(ports).sort();
    let connectedCount = 0;

    portIds.forEach(portId => {
        const info = ports[portId] || {};
        if (info.connected || info.status === 'connected') {
            connectedCount++;
        }
    });

    const connEl = document.getElementById('connectedPorts');
    const totalEl = document.getElementById('totalPorts');
    if (connEl) connEl.textContent = connectedCount;
    if (totalEl) totalEl.textContent = portIds.length;

    const badge = document.getElementById('portStatsBadge');
    if (badge) badge.textContent = `${connectedCount} / ${portIds.length}`;

    // Update Metacube sidebar badges
    const navPortsBadge = document.getElementById('navPortsBadge');
    if (navPortsBadge) navPortsBadge.textContent = portIds.length;

    const navTerminalBadge = document.getElementById('navTerminalBadge');
    if (navTerminalBadge) {
        if (activePortId) {
            const curInfo = ports[activePortId] || {};
            navTerminalBadge.textContent = curInfo.alias || activePortId;
            navTerminalBadge.style.display = '';
        } else {
            navTerminalBadge.style.display = 'none';
        }
    }

    // Keep terminalConnected in sync with active port's status
    if (activePortId && ports[activePortId]) {
        const curInfo = ports[activePortId] || {};
        terminalConnected = !!(curInfo.connected || curInfo.status === 'connected');
    }

    // Update sidebar port list if element exists (legacy support)
    const portListEl = document.getElementById('portList');
    if (portListEl) {
        portListEl.innerHTML = '';
        portIds.forEach(portId => {
            const info = ports[portId] || {};
            const item = document.createElement('div');
            item.className = 'port-item' + (portId === activePortId ? ' active' : '');
            item.onclick = () => switchToPort(portId);

            const statusClass = (info.connected || info.status === 'connected') ? 'connected' : (info.status === 'connecting' ? 'connecting' : 'disconnected');
            const alias = info.alias || '';
            const displayName = alias ? `<strong>${alias}</strong>` : portId;
            const subText = alias ? portId : (info.description || '');

            item.innerHTML = `
                <span class="port-dot ${statusClass}"></span>
                <div class="port-item-info">
                    <div class="port-item-name">${displayName}</div>
                    ${subText ? `<div class="port-item-sub" title="${subText}">${subText}</div>` : ''}
                </div>
                <div style="font-size:10px;color:var(--text-muted);flex-shrink:0">${info.status || statusClass}</div>
            `;
            portListEl.appendChild(item);
        });
    }

    // Update tab bar
    updateTabBar(ports);

    // Update port buttons
    updatePortButtons();

    // Update cmd bar label
    updateCmdBarLabel();

    // Re-render Ports management grid view
    if (typeof renderPortsView === 'function') {
        renderPortsView();
    }
}

function updatePortStatus(portId, status) {
    if (portStatus[portId]) {
        portStatus[portId] = { ...portStatus[portId], ...status };
        updatePortList(portStatus);
    }
}

// Update sidebar settings panel for given port
function selectPortSettings(portId) {
    selectedPort = portId;
    updatePortButtons();

    const info = portStatus[portId] || {};
    terminalConnected = !!(info.connected || info.status === 'connected');

    // Sync sidebar dropdowns to port's saved config
    if (info.baudrate) {
        const br = document.getElementById('baudRate');
        if (br) {
            const match = [...br.options].find(o => parseInt(o.value) === info.baudrate);
            if (match) {
                br.value = info.baudrate;
            } else {
                const opt = document.createElement('option');
                opt.value = info.baudrate;
                opt.textContent = info.baudrate;
                br.appendChild(opt);
                br.value = info.baudrate;
            }
        }
    }
    if (info.databits) {
        const db = document.getElementById('dataBits');
        if (db && [...db.options].find(o => parseInt(o.value) === info.databits)) db.value = info.databits;
    }
    if (info.stopbit) {
        const sb = document.getElementById('stopBits');
        if (sb && [...sb.options].find(o => parseFloat(o.value) === info.stopbit)) sb.value = info.stopbit;
    }
    if (info.parity) {
        const p = document.getElementById('parity');
        if (p) p.value = info.parity;
    }

    // Update alias input
    const aliasInput = document.getElementById('portAlias');
    if (aliasInput) aliasInput.value = info.alias || '';
}

// Keep selectPort for backward compat
function selectPort(port) {
    switchToPort(port);
}

function addPort() {
    const portInput = document.getElementById('newPort');
    const port = portInput.value.trim();
    if (!port) { alert('Please enter a port name'); return; }
    if (portStatus[port]) { alert('Port already exists'); return; }

    portStatus[port] = { connected: false };
    updatePortList(portStatus);
    portInput.value = '';

    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'discover_port', port }));
    }
}

function refreshPorts() {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'get_ports' }));
    }
}

function connectCurrentPort() {
    if (!selectedPort || !ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({
        type: 'connect_port',
        port_id: selectedPort,
        baudrate: parseInt(document.getElementById('baudRate').value),
        databits: parseInt(document.getElementById('dataBits').value),
        stopbit: parseFloat(document.getElementById('stopBits').value),
        parity: document.getElementById('parity').value,
        timeout: 10
    }));
    showToast(`Connecting to ${selectedPort}...`, 'info', 2000);
}

function disconnectCurrentPort() {
    if (!selectedPort || !ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'disconnect_port', port_id: selectedPort }));
    showToast(`Disconnecting from ${selectedPort}...`, 'warning', 2000);
}

function updatePortConfig() {
    if (!selectedPort || !ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({
        type: 'configure_port',
        port_id: selectedPort,
        baudrate: parseInt(document.getElementById('baudRate').value),
        databits: parseInt(document.getElementById('dataBits').value),
        stopbit: parseFloat(document.getElementById('stopBits').value),
        parity: document.getElementById('parity').value,
        timeout: 10
    }));
    showToast(`Config updated for ${selectedPort}`, 'info', 2000);
}

function updatePortButtons() {
    const connectBtn = document.getElementById('connectBtn');
    const disconnectBtn = document.getElementById('disconnectBtn');
    if (!connectBtn || !disconnectBtn) return;

    if (selectedPort) {
        const info = portStatus[selectedPort] || {};
        connectBtn.disabled = false;
        disconnectBtn.disabled = false;
        if (info.connected) {
            connectBtn.style.display = 'none';
            disconnectBtn.style.display = 'block';
        } else {
            connectBtn.style.display = 'block';
            disconnectBtn.style.display = 'none';
        }
    } else {
        connectBtn.disabled = true;
        disconnectBtn.disabled = true;
    }
}

// Port alias save (called from alias input)
function savePortAlias() {
    if (!selectedPort) return;
    const input = document.getElementById('portAlias');
    if (!input) return;
    const alias = input.value.trim();
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'set_port_alias', port_id: selectedPort, alias }));
        showToast(alias ? `Alias set: ${alias}` : 'Alias cleared', 'success', 2000);
    }
}

// ============================================================
// Terminal Operations
// ============================================================
function clearTerminal() {
    const pt = getActivePT();
    if (pt) {
        pt.terminal.clear();
        pt.terminal.writeln('\x1b[1;36mTerminal cleared\x1b[0m\n');
    }
}

function fitTerminal() {
    const pt = getActivePT();
    if (pt && pt.fitAddon) {
        try { pt.fitAddon.fit(); } catch (e) { console.warn('fit error:', e); }
    }
}

function updateTerminalSettings() {
    const scrollback = parseInt(document.getElementById('scrollbackLines').value);
    const fontSize = parseInt(document.getElementById('terminalFontSize').value);
    const themeName = document.getElementById('terminalTheme').value;
    const fontFamily = document.getElementById('terminalFontFamily').value;

    const fvEl = document.getElementById('fontSizeValue');
    if (fvEl) fvEl.textContent = fontSize + 'px';

    const theme = TERMINAL_THEMES[themeName] || TERMINAL_THEMES.dark;

    // Apply to ALL existing terminals
    portTerminals.forEach(pt => {
        try {
            pt.terminal.options.scrollback = scrollback;
            pt.terminal.options.fontSize = fontSize;
            pt.terminal.options.fontFamily = fontFamily;
            pt.terminal.options.theme = theme;
        } catch (e) { console.warn('settings apply error:', e); }
        if (pt.fitAddon) try { pt.fitAddon.fit(); } catch(e) {}
    });

    // Persist
    localStorage.setItem('terminal_scrollback', scrollback);
    localStorage.setItem('terminal_fontsize', fontSize);
    localStorage.setItem('terminal_theme_name', themeName);
    localStorage.setItem('terminal_fontfamily', fontFamily);
}

// Font picker population: the preset <option>s in index.html name
// fonts that may not exist on this machine — an unmatched name
// silently falls back, which looks like the setting does nothing.
// Where the real list is obtainable it replaces the choices:
// inside sos-gui the Go side enumerates the OS's monospaced
// families via the bridge, and that list IS the machine's font
// inventory, so the presets are dropped entirely there. Chromium
// browsers can answer queryLocalFonts() (Local Font Access API —
// needs a user gesture and permission, so population is retried
// when the select gains focus); there, and anywhere enumeration
// fails, the presets stay as the fallback group.
let fontOptionsPopulated = false;
async function populateFontOptions() {
    const sel = document.getElementById('terminalFontFamily');
    if (!sel || fontOptionsPopulated) return;
    let families = [];
    let fromBridge = false;
    if (hasFileBridge()) {
        try {
            const res = await callFileBridge('FontService.ListFamilies');
            if (res && Array.isArray(res.families) && res.families.length) {
                families = res.families;
                fromBridge = true;
            }
        } catch (e) { /* fall through to the next source */ }
    }
    if (!families.length && typeof window.queryLocalFonts === 'function') {
        try {
            const fonts = await window.queryLocalFonts();
            const seen = new Set();
            fonts.forEach(f => { if (f && f.family) seen.add(f.family); });
            families = [...seen].sort();
        } catch (e) { /* permission denied etc.: keep presets */ }
    }
    if (!families.length) return;
    fontOptionsPopulated = true;

    const presets = Array.from(sel.options).map(o => ({ value: o.value, text: o.textContent }));
    const current = sel.value || loadTerminalSettings().fontFamily;
    sel.innerHTML = '';
    const addOption = (parent, value, text) => {
        const opt = document.createElement('option');
        opt.value = value;
        opt.textContent = text;
        parent.appendChild(opt);
    };
    if (current && !families.some(f => `'${f}', monospace` === current) && !presets.some(p => p.value === current)) {
        // A stored choice from another machine/list: keep it
        // selectable rather than silently dropping it.
        addOption(sel, current, current.split(',')[0].replace(/'/g, '').trim());
    }
    if (fromBridge) {
        // The bridge list is the picker's only source in the GUI:
        // flat options, no group header naming what is already
        // the whole list.
        families.forEach(fam => addOption(sel, `'${fam.replace(/'/g, "\\'")}', monospace`, fam));
    } else {
        const sysGroup = document.createElement('optgroup');
        sysGroup.label = 'System fonts';
        families.forEach(fam => addOption(sysGroup, `'${fam.replace(/'/g, "\\'")}', monospace`, fam));
        sel.appendChild(sysGroup);
        const presetGroup = document.createElement('optgroup');
        presetGroup.label = 'Presets';
        presets.forEach(p => addOption(presetGroup, p.value, p.text));
        sel.appendChild(presetGroup);
    }
    sel.value = current;
}

async function saveTerminalContent() {
    const pt = getActivePT();
    if (!pt) return;

    pt.terminal.selectAll();
    const content = pt.terminal.getSelection();
    pt.terminal.clearSelection();

    if (!content) {
        showToast('Terminal is empty', 'warning', 2000);
        return;
    }

    const exportName = `serial_${activePortId || 'log'}_${new Date().toISOString().replace(/[:.]/g, '-')}.txt`;

    // In the desktop GUI, Blob downloads go nowhere (the webview has
    // no download handler): save through the file bridge instead,
    // with a native save dialog. Cancelled dialog = no toast.
    if (hasFileBridge()) {
        try {
            const res = await callFileBridge('FileService.SaveTextFile', exportName, content);
            if (res && res.path) showToast(`已保存: ${res.path}`, 'success', 3000);
        } catch (e) {
            console.error('SaveTextFile failed:', e);
            showToast('Failed to save', 'error', 3000);
        }
        return;
    }

    const blob = new Blob([content], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = exportName;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);

    showToast('Terminal content saved', 'success', 2000);
}

// Resize observer for window resize
window.addEventListener('resize', () => {
    setTimeout(() => {
        const pt = getActivePT();
        if (pt && pt.fitAddon) try { pt.fitAddon.fit(); } catch(e) {}
    }, 100);
});

// ============================================================
// Search Functionality
// ============================================================
function toggleSearch() {
    const bar = document.getElementById('searchBar');
    const input = document.getElementById('searchInput');
    if (!bar) return;
    if (bar.style.display === 'none' || !bar.style.display) {
        bar.style.display = 'flex';
        if (input) { input.focus(); if (input.value) input.select(); }
    } else {
        closeSearch();
    }
}

function closeSearch() {
    const bar = document.getElementById('searchBar');
    if (bar) bar.style.display = 'none';

    const pt = getActivePT();
    if (pt && pt.searchAddon) {
        try { pt.searchAddon.clearDecorations(); } catch (e) {}
    }
    if (pt) {
        pt.terminal.clearSelection();
        pt.terminal.focus();
    }
}

const SEARCH_DECO = {
    decorations: {
        matchBackground: 'rgba(61,90,254,0.4)',
        matchBorder: 'transparent',
        matchOverviewRuler: '#3d5afe',
        activeMatchBackground: 'rgba(255,213,79,0.85)',
        activeMatchBorder: 'transparent',
        activeMatchColorOverviewRuler: '#ffd54f'
    }
};

function findNext() {
    const pt = getActivePT();
    if (!pt || !pt.searchAddon) return;
    const term = document.getElementById('searchInput').value;
    if (term) pt.searchAddon.findNext(term, SEARCH_DECO);
}

function findPrev() {
    const pt = getActivePT();
    if (!pt || !pt.searchAddon) return;
    const term = document.getElementById('searchInput').value;
    if (term) pt.searchAddon.findPrevious(term, SEARCH_DECO);
}

function handleSearchKey(e) {
    if (e.key === 'Enter') {
        if (e.shiftKey) findPrev(); else findNext();
    } else if (e.key === 'Escape') {
        closeSearch();
    }
}

// ============================================================
// Command History
// ============================================================
let commandHistory = [];
let historyIndex = -1;
const MAX_HISTORY = 50;

function loadCommandHistory() {
    try {
        const saved = localStorage.getItem('commandHistory');
        if (saved) commandHistory = JSON.parse(saved);
    } catch (e) { console.error('Failed to load history:', e); }
}

function saveCommandHistory() {
    try {
        localStorage.setItem('commandHistory', JSON.stringify(commandHistory));
    } catch (e) { console.error('Failed to save history:', e); }
}

function addToHistory(command) {
    if (!command.trim()) return;
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'add_command', command }));
    } else {
        const index = commandHistory.indexOf(command);
        if (index !== -1) commandHistory.splice(index, 1);
        commandHistory.unshift(command);
        if (commandHistory.length > MAX_HISTORY) commandHistory.length = MAX_HISTORY;
        historyIndex = 0;
        saveCommandHistory();
    }
}

function navigateHistory(direction) {
    const input = document.getElementById('terminalInput');
    if (!input) return;
    if (direction === 'up') {
        if (historyIndex > 0) { historyIndex--; input.value = commandHistory[historyIndex] || ''; }
    } else {
        if (historyIndex < commandHistory.length) { historyIndex++; input.value = commandHistory[historyIndex] || ''; }
    }
    setTimeout(() => {
        input.selectionStart = input.selectionEnd = input.value.length;
        autoResizeTextarea();
    }, 0);
}

let dropdownSelectedIndex = -1;

function updateHistoryDropdown() {
    const input = document.getElementById('terminalInput');
    const dropdown = document.getElementById('historyDropdown');
    if (!input || !dropdown) return;

    const text = input.value.toLowerCase();
    if (!text.trim()) { dropdown.style.display = 'none'; dropdownSelectedIndex = -1; return; }

    const matches = commandHistory.filter(cmd => cmd.toLowerCase().includes(text)).slice(-10).reverse();
    if (matches.length === 0) { dropdown.style.display = 'none'; dropdownSelectedIndex = -1; return; }

    dropdown.innerHTML = matches.map((cmd, i) => {
        const sel = i === dropdownSelectedIndex ? ' selected' : '';
        return `<div class="history-item${sel}" data-index="${i}">${escapeHtmlStr(cmd)}</div>`;
    }).join('');
    dropdown.style.display = 'block';
    dropdown.dataset.matches = JSON.stringify(matches);
}

function navigateDropdown(direction) {
    const dropdown = document.getElementById('historyDropdown');
    if (!dropdown) return;
    const items = dropdown.querySelectorAll('.history-item');
    if (items.length === 0) return;

    if (dropdownSelectedIndex >= 0 && dropdownSelectedIndex < items.length) {
        items[dropdownSelectedIndex].classList.remove('selected');
    }
    if (direction === 'down') {
        dropdownSelectedIndex = (dropdownSelectedIndex + 1) % items.length;
    } else {
        dropdownSelectedIndex = dropdownSelectedIndex <= 0 ? items.length - 1 : dropdownSelectedIndex - 1;
    }
    if (dropdownSelectedIndex >= 0 && dropdownSelectedIndex < items.length) {
        items[dropdownSelectedIndex].classList.add('selected');
        items[dropdownSelectedIndex].scrollIntoView({ block: 'nearest' });
    }
}

function selectHistoryItem(command) {
    const input = document.getElementById('terminalInput');
    const dropdown = document.getElementById('historyDropdown');
    if (!input) return;
    input.value = command;
    if (dropdown) dropdown.style.display = 'none';
    dropdownSelectedIndex = -1;
    input.focus();
    input.selectionStart = input.selectionEnd = input.value.length;
    autoResizeTextarea(false);
}

function autoResizeTextarea(updateDropdown = true) {
    const textarea = document.getElementById('terminalInput');
    if (!textarea) return;
    textarea.style.height = 'auto';
    textarea.style.height = Math.min(textarea.scrollHeight, 150) + 'px';
    if (updateDropdown) {
        updateHistoryDropdown();
    }
}

function handleTerminalInput(event) {
    const dropdown = document.getElementById('historyDropdown');
    const textarea = document.getElementById('terminalInput');
    const isDropdownVisible = dropdown && dropdown.style.display === 'block';

    if (event.key === 'Enter') {
        if (isDropdownVisible && dropdownSelectedIndex >= 0) {
            event.preventDefault();
            const matches = JSON.parse(dropdown.dataset.matches || '[]');
            if (matches[dropdownSelectedIndex] !== undefined) {
                selectHistoryItem(matches[dropdownSelectedIndex]);
            }
        } else if (!event.shiftKey) {
            // Enter (without Shift) -> Send command
            event.preventDefault();
            if (dropdown) dropdown.style.display = 'none';
            dropdownSelectedIndex = -1;
            sendTerminalInput();
        }
        // Shift+Enter falls through to insert newline
    } else if (event.key === 'ArrowUp') {
        if (isDropdownVisible) { event.preventDefault(); navigateDropdown('up'); }
        else if (textarea.selectionStart === 0) { event.preventDefault(); navigateHistory('up'); }
    } else if (event.key === 'ArrowDown') {
        if (isDropdownVisible) { event.preventDefault(); navigateDropdown('down'); }
        else if (textarea.selectionStart === textarea.value.length) { event.preventDefault(); navigateHistory('down'); }
    } else if (event.key === 'Escape') {
        if (dropdown) dropdown.style.display = 'none';
        dropdownSelectedIndex = -1;
    }
}

function sendTerminalInput() {
    const input = document.getElementById('terminalInput');
    if (!input) return;
    const text = input.value;
    if (!text.trim()) return;

    addToHistory(text);

    const lineEnding = localStorage.getItem('serial_line_ending') !== null ? localStorage.getItem('serial_line_ending') : '\r\n';

    const port = selectedPort || activePortId;
    const isConn = terminalConnected || isPortConnected(port);
    if (isConn && port && ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'write_data', port_id: port, data: text + lineEnding }));
    } else {
        const pt = getActivePT();
        if (pt) pt.terminal.write(`\x1b[91m[Not connected]\x1b[0m ${text}\n`);
    }

    input.value = '';
    historyIndex = 0;
    input.style.height = 'auto';
    input.focus();
}

loadCommandHistory();

// ============================================================
// Browser-side Logging
// ============================================================
let isLogging = false;
let logData = [];
let logFilename = '';
let logSize = 0;
let logStartTime = null;

// File bridge (sos-gui only): inside the desktop GUI the page can
// call Go via window.mygo. Logging then streams to a local file
// chosen in a native save dialog (opened by Go at start, written
// incrementally, closed at stop) instead of accumulating in memory
// for a Blob download. Plain browsers have no window.mygo and keep
// the original flow below, unchanged. Detection is a function, not
// a load-time constant: it must reflect the page's state at click
// time, whatever order scripts were injected in.
function hasFileBridge() {
    return typeof window !== 'undefined' && !!window.mygo && typeof window.mygo.call === 'function';
}
function callFileBridge(method, ...args) {
    return window.mygo.call(method, ...args);
}

// In the desktop GUI the service worker is a liability, not a
// feature: the page comes from a LAN server, and the SW's
// cache-first assets make WebUI updates depend on CACHE_NAME bumps
// plus an update-activation step — a stale cached app.js once made
// new GUI features silently never engage. Remove any registration
// an older build left behind (index.html no longer registers one
// in the GUI); if this page is still *controlled* by that SW, one
// guarded reload escapes its control for good — the sessionStorage
// flag makes a loop impossible. Browsers keep the PWA.
if (hasFileBridge() && 'serviceWorker' in navigator) {
    const wasControlled = !!navigator.serviceWorker.controller;
    navigator.serviceWorker.getRegistrations()
        .then(regs => Promise.all(regs.map(r => r.unregister())))
        .then(() => (window.caches ? caches.keys() : []))
        .then(keys => Promise.all((keys || []).map(k => caches.delete(k))))
        .then(() => {
            if (wasControlled && !sessionStorage.getItem('sosGuiSwPurged')) {
                sessionStorage.setItem('sosGuiSwPurged', '1');
                location.reload();
            }
        })
        .catch(() => { });
}
let logBridgeId = null;   // open stream id while bridge-logging
let logPending = '';      // batched data not yet handed to Go
let logFlushChain = Promise.resolve();
let logFlushTimer = null;
let logStarting = false;  // a BeginLog save dialog is open

function toggleLogging() {
    if (isLogging) stopLogging(); else startLogging();
}

async function startLogging() {
    if (!selectedPort) { showToast('Please select a port first', 'warning', 2000); return; }
    logStartTime = new Date();
    const ts = logStartTime.toISOString().replace(/[-:]/g, '').replace('T', '_').split('.')[0];
    logFilename = `serial_${selectedPort}_${ts}.log`;
    logData = [];
    logSize = 0;
    if (hasFileBridge()) {
        if (logStarting) return;
        logStarting = true;
        try {
            const res = await callFileBridge('FileService.BeginLog', logFilename);
            if (!res || !res.id) { logStarting = false; return; } // user cancelled the dialog
            logBridgeId = res.id;
            logFilename = res.path.split(/[\\/]/).pop();
        } catch (e) {
            console.error('BeginLog failed:', e);
            showToast('Failed to start logging', 'error', 3000);
            logStarting = false;
            return;
        }
        logStarting = false;
    }
    isLogging = true;
    updateLogButton();
    updateLogDisplay();
}

async function stopLogging() {
    if (!isLogging) return;
    isLogging = false;
    updateLogButton();
    if (hasFileBridge() && logBridgeId) {
        const id = logBridgeId;
        logBridgeId = null;
        if (logFlushTimer) { clearTimeout(logFlushTimer); logFlushTimer = null; }
        flushLogPending();
        try {
            await logFlushChain;
            const res = await callFileBridge('FileService.EndLog', id);
            showToast(`日志已保存: ${res.path} (${formatFileSize(res.size)})`, 'success', 3500);
        } catch (e) {
            console.error('EndLog failed:', e);
            showToast('Failed to save log', 'error', 3000);
        }
        hideLogDisplay();
        return;
    }
    if (logData.length === 0) { showToast('No data logged', 'warning', 2000); hideLogDisplay(); return; }
    let filename = prompt('Save log file as:', logFilename);
    if (!filename) { hideLogDisplay(); return; }
    if (!filename.endsWith('.log')) filename += '.log';
    downloadLog(filename);
    hideLogDisplay();
}

function appendToLog(data) {
    if (!isLogging) return;
    logSize += data.length;
    if (hasFileBridge() && logBridgeId) {
        logPending += data;
        if (logPending.length >= 65536) {
            flushLogPending();
        } else if (!logFlushTimer) {
            logFlushTimer = setTimeout(() => { logFlushTimer = null; flushLogPending(); }, 250);
        }
    } else {
        logData.push(data);
    }
    updateLogDisplay();
}

// Hand the batched data to Go, serialized on logFlushChain so
// batches land in order even though bridge calls are async.
function flushLogPending() {
    if (!logPending || !logBridgeId) { logPending = ''; return; }
    const chunk = logPending;
    const id = logBridgeId;
    logPending = '';
    logFlushChain = logFlushChain
        .then(() => callFileBridge('FileService.AppendLog', id, chunk))
        .catch(e => {
            console.error('AppendLog failed:', e);
            showToast('日志写入失败，已停止记录', 'error', 3000);
            if (isLogging && logBridgeId === id) {
                isLogging = false;
                logBridgeId = null;
                updateLogButton();
                hideLogDisplay();
            }
        });
}

function downloadLog(filename) {
    try {
        const blob = new Blob(logData, { type: 'text/plain' });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = filename;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
    } catch (e) {
        console.error('Failed to download log:', e);
        showToast('Failed to download log', 'error', 3000);
    }
}

function formatFileSize(bytes) {
    if (bytes < 1024) return bytes + ' B';
    if (bytes < 1048576) return (bytes / 1024).toFixed(1) + ' KB';
    return (bytes / 1048576).toFixed(1) + ' MB';
}

function updateLogDisplay() {
    // Tab bar log indicator
    const indicator = document.getElementById('logIndicator');
    const filenameEl = document.getElementById('logFilename');
    const sizeEl = document.getElementById('logSize');

    if (indicator) indicator.style.display = isLogging ? 'flex' : 'none';
    if (filenameEl) filenameEl.textContent = logFilename;
    if (sizeEl) sizeEl.textContent = formatFileSize(logSize);

    // Cmd bar log status
    const logStatus = document.getElementById('logStatus');
    const logFilenameCmd = document.getElementById('logFilenameCmd');
    const logSizeCmd = document.getElementById('logSizeCmd');
    if (logStatus) logStatus.style.display = isLogging ? 'flex' : 'none';
    if (logFilenameCmd) logFilenameCmd.textContent = logFilename;
    if (logSizeCmd) logSizeCmd.textContent = formatFileSize(logSize);
}

function hideLogDisplay() {
    const indicator = document.getElementById('logIndicator');
    const logStatus = document.getElementById('logStatus');
    if (indicator) indicator.style.display = 'none';
    if (logStatus) logStatus.style.display = 'none';
}

function updateLogButton() {
    const btn = document.getElementById('logBtn');
    if (!btn) return;
    if (isLogging) {
        btn.title = 'Stop & Save Log';
        btn.classList.add('active');
    } else {
        btn.title = 'Start Logging';
        btn.classList.remove('active');
    }
}

// ============================================================
// History Management Panel
// ============================================================
let editingIndex = -1;

function toggleHistoryManager() {
    const panel = document.getElementById('historyManager');
    if (!panel) return;
    if (panel.style.display === 'none' || !panel.style.display) {
        panel.style.display = 'flex';
        switchHMTab('history');
        renderHistoryList();
    } else {
        panel.style.display = 'none';
    }
}

function switchHMTab(tab) {
    const tabs = { history: 'hmTabHistory', sync: 'hmTabSync' };
    const panels = { history: 'hmPanelHistory', sync: 'hmPanelSync' };

    Object.keys(tabs).forEach(key => {
        const tabEl = document.getElementById(tabs[key]);
        const panelEl = document.getElementById(panels[key]);
        if (tabEl) tabEl.classList.toggle('active', key === tab);
        if (panelEl) panelEl.classList.toggle('active', key === tab);
    });

    if (tab === 'sync') {
        populateWebDAVForm();
        updateWebDAVTimerLabel();
        const lastBackup = localStorage.getItem('webdav_last_backup');
        const el = document.getElementById('webdavLastBackup');
        if (el && lastBackup) el.textContent = 'Last: ' + lastBackup;
        const listEl = document.getElementById('webdavFileList');
        if (listEl && listEl.querySelector('.webdav-empty') &&
            listEl.querySelector('.webdav-empty').textContent.includes('Refresh')) {
            refreshWebDAVFileList();
        }
    }
}

function renderHistoryList() {
    const listEl = document.getElementById('historyList');
    if (!listEl) return;
    if (commandHistory.length === 0) {
        listEl.innerHTML = '<div style="text-align:center;padding:24px;color:var(--text-muted)">No command history yet</div>';
        return;
    }
    listEl.innerHTML = commandHistory.map((cmd, index) => {
        const escaped = cmd.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
        return `
            <div class="history-list-item">
                <div class="history-list-item-content">${escaped}</div>
                <div class="history-list-item-actions">
                    <button class="history-list-item-btn" onclick="useHistoryItem(${index})">Use</button>
                    <button class="history-list-item-btn" onclick="editHistoryItem(${index})">Edit</button>
                    <button class="history-list-item-btn danger" onclick="deleteHistoryItem(${index})">Delete</button>
                </div>
            </div>
        `;
    }).join('');
}

function useHistoryItem(index) {
    const input = document.getElementById('terminalInput');
    if (input) input.value = commandHistory[index];
    autoResizeTextarea();
    toggleHistoryManager();
    if (input) input.focus();
}

function editHistoryItem(index) {
    editingIndex = index;
    const modal = document.getElementById('editModal');
    const textarea = document.getElementById('editTextarea');
    if (modal && textarea) {
        textarea.value = commandHistory[index];
        modal.style.display = 'flex';
        textarea.focus();
    }
}

function closeEditModal(event) {
    if (event && event.target !== event.currentTarget) return;
    const modal = document.getElementById('editModal');
    if (modal) modal.style.display = 'none';
    editingIndex = -1;
}

function closeEditCommandModal() {
    const modal = document.getElementById('editModal');
    if (modal) modal.style.display = 'none';
    editingIndex = -1;
}

function saveEdit() {
    if (editingIndex >= 0 && editingIndex < commandHistory.length) {
        const textarea = document.getElementById('editTextarea');
        const newValue = textarea ? textarea.value.trim() : '';
        if (newValue) {
            const realId = window.historyRecords ? window.historyRecords[editingIndex]?.id : null;
            if (ws && ws.readyState === WebSocket.OPEN && realId) {
                ws.send(JSON.stringify({ type: 'edit_command', id: realId, command: newValue }));
            } else {
                commandHistory[editingIndex] = newValue;
                saveCommandHistory();
                renderHistoryList();
            }
            showToast('Command updated', 'success', 2000);
        } else {
            showToast('Command cannot be empty', 'warning', 2000);
        }
    }
    closeEditCommandModal();
}

// Keep old name for backward compat
function saveEditedCommand() { saveEdit(); }

function deleteHistoryItem(index) {
    if (!confirm('Delete this command from history?')) return;
    const realId = window.historyRecords ? window.historyRecords[index]?.id : null;
    if (ws && ws.readyState === WebSocket.OPEN && realId) {
        ws.send(JSON.stringify({ type: 'delete_command', id: realId }));
        showToast('Command deleted', 'success', 2000);
    } else {
        commandHistory.splice(index, 1);
        saveCommandHistory();
        renderHistoryList();
        showToast('Command deleted locally', 'success', 2000);
    }
}

function clearAllHistory() {
    if (!confirm('Clear all command history? This cannot be undone.')) return;
    commandHistory = [];
    historyIndex = 0;
    saveCommandHistory();
    renderHistoryList();
    showToast('History cleared', 'success', 2000);
}

function exportHistory() {
    if (commandHistory.length === 0) { showToast('No history to export', 'warning', 2000); return; }
    const data = { version: '1.0', exportDate: new Date().toISOString(), commands: commandHistory };
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `command_history_${new Date().toISOString().split('T')[0]}.json`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
    showToast('History exported', 'success', 2000);
}

function importHistory() {
    const input = document.getElementById('importFileInput');
    if (input) input.click();
}

function handleImportFile(event) {
    const file = event.target.files[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = function (e) {
        try {
            const data = JSON.parse(e.target.result);
            if (!data.commands || !Array.isArray(data.commands)) throw new Error('Invalid history file format');
            const merge = confirm('Merge with existing history? (Cancel to replace)');
            if (ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: merge ? 'merge_history' : 'replace_history', commands: data.commands }));
                showToast('Import request sent!', 'success', 2000);
            } else {
                if (merge) {
                    const combined = [...commandHistory, ...data.commands.map(c => typeof c === 'string' ? c : c.command)];
                    commandHistory = [...new Set(combined)];
                } else {
                    commandHistory = data.commands.map(c => typeof c === 'string' ? c : c.command);
                }
                if (commandHistory.length > MAX_HISTORY) commandHistory = commandHistory.slice(-MAX_HISTORY);
                historyIndex = 0;
                saveCommandHistory();
                renderHistoryList();
                showToast(`Imported ${data.commands.length} commands`, 'success', 2000);
            }
        } catch (error) {
            showToast('Failed to import: ' + error.message, 'error', 3000);
        }
    };
    reader.readAsText(file);
    event.target.value = '';
}

// ============================================================
// WebDAV Backup System
// ============================================================
const WEBDAV_CONFIG_KEY = 'webdav_config';
const WEBDAV_BACKUP_PREFIX = 'history_backup_';
let webdavAutoBackupTimer = null;

function loadWebDAVConfig() {
    try {
        const saved = localStorage.getItem(WEBDAV_CONFIG_KEY);
        return saved ? JSON.parse(saved) : { url: '', username: '', password: '', interval: 30, maxBackups: 10 };
    } catch (e) {
        return { url: '', username: '', password: '', interval: 30, maxBackups: 10 };
    }
}

// Tracks whether the server has a stored password (set from webdav_config).
let webdavPasswordIsSet = false;

function saveWebDAVConfig(clearPassword) {
    const cfg = getWebDAVFormConfig();
    localStorage.setItem(WEBDAV_CONFIG_KEY, JSON.stringify(cfg));
    if (ws && ws.readyState === WebSocket.OPEN) {
        const msg = {
            type: 'set_webdav_config',
            url: cfg.url, username: cfg.username,
            interval: cfg.interval, maxBackups: cfg.maxBackups
        };
        if (clearPassword) {
            msg.clear_password = true;
        } else if (cfg.password) {
            // Only a newly typed password is sent; empty = keep stored.
            msg.password = cfg.password;
        }
        ws.send(JSON.stringify(msg));
    }
    // Don't leave a typed password sitting in the field.
    const pwEl = document.getElementById('webdavPassword');
    if (pwEl) pwEl.value = '';
    showToast('WebDAV settings saved', 'success', 2000);
}

function clearWebDAVPassword() {
    if (!confirm('清除服务器上保存的 WebDAV 密码?')) return;
    webdavPasswordIsSet = false;
    const pwEl = document.getElementById('webdavPassword');
    if (pwEl) { pwEl.value = ''; pwEl.placeholder = '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022'; }
    saveWebDAVConfig(true);
}

// Push the local WebDAV settings to the server before a manual proxy
// operation and wait for the server's acknowledgement. The proxy endpoints
// only use the server-side saved config, so the push must land first —
// fire-and-forget would race the HTTP request that follows.
// An empty password here means "keep whatever the server has stored".
let webdavPushSeq = 0;
const webdavPushWaiters = {};
function pushWebDAVToServer(cfg) {
    return new Promise((resolve) => {
        if (!ws || ws.readyState !== WebSocket.OPEN) { resolve(); return; }
        const ack = ++webdavPushSeq;
        webdavPushWaiters[ack] = resolve;
        const msg = {
            type: 'set_webdav_config', _ack: ack,
            url: cfg.url, username: cfg.username,
            interval: cfg.interval, maxBackups: cfg.maxBackups
        };
        if (cfg.password) msg.password = cfg.password;
        ws.send(JSON.stringify(msg));
        setTimeout(() => { delete webdavPushWaiters[ack]; resolve(); }, 2000);
    });
}

function getWebDAVFormConfig() {
    return {
        url: (document.getElementById('webdavUrl')?.value || '').trim().replace(/\/$/, ''),
        username: document.getElementById('webdavUsername')?.value || '',
        password: document.getElementById('webdavPassword')?.value || '',
        interval: parseInt(document.getElementById('webdavInterval')?.value) || 30,
        maxBackups: parseInt(document.getElementById('webdavMaxBackups')?.value) || 10
    };
}

function populateWebDAVForm() {
    const cfg = loadWebDAVConfig();
    if (document.getElementById('webdavUrl')) document.getElementById('webdavUrl').value = cfg.url;
    if (document.getElementById('webdavUsername')) document.getElementById('webdavUsername').value = cfg.username;
    if (document.getElementById('webdavPassword')) document.getElementById('webdavPassword').value = cfg.password;
    if (document.getElementById('webdavInterval')) document.getElementById('webdavInterval').value = cfg.interval;
    if (document.getElementById('webdavMaxBackups')) document.getElementById('webdavMaxBackups').value = cfg.maxBackups;
}

function webdavBody(extra) {
    // The proxy endpoints (list/upload/download/delete) always use the
    // server-side saved config; no credentials are sent.
    return JSON.stringify({ ...extra });
}

async function testWebDAVConnection() {
    const btn = document.getElementById('webdavTestBtn');
    const statusEl = document.getElementById('webdavTestStatus');
    const cfg = getWebDAVFormConfig();

    if (!cfg.url) { showToast('Please enter a WebDAV URL', 'warning', 2000); return; }

    if (btn) { btn.disabled = true; btn.textContent = 'Testing…'; }
    if (statusEl) { statusEl.textContent = ''; statusEl.className = 'webdav-test-status'; }

    // Push the form values to the server first: /api/webdav/test only
    // tests the server-side saved config (never request-supplied creds).
    await pushWebDAVToServer(cfg);

    try {
        const res = await fetch('/api/webdav/test', {
            method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}'
        });
        const json = await res.json();
        if (res.ok && json.ok) {
            if (statusEl) { statusEl.textContent = '✓ Connected successfully'; statusEl.className = 'webdav-test-status success'; }
            showToast('WebDAV connected!', 'success', 2000);
        } else {
            const msg = json.error || `HTTP ${json.status || res.status}`;
            if (statusEl) { statusEl.textContent = `✗ ${msg}`; statusEl.className = 'webdav-test-status error'; }
            showToast('WebDAV error: ' + msg, 'error', 3000);
        }
    } catch (e) {
        if (statusEl) { statusEl.textContent = `✗ ${e.message}`; statusEl.className = 'webdav-test-status error'; }
        showToast('Connection failed: ' + e.message, 'error', 3000);
    }

    if (btn) { btn.disabled = false; btn.textContent = '🔌 Test Connection'; }
}

async function uploadBackupToWebDAV(cfg, filename, content) {
    const res = await fetch('/api/webdav/upload', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: webdavBody({ filename, content })
    });
    const json = await res.json();
    if (!res.ok || !json.ok) throw new Error(json.error || `Upload failed: HTTP ${res.status}`);
}

async function listWebDAVBackups(cfg) {
    const res = await fetch('/api/webdav/list', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: webdavBody({ prefix: WEBDAV_BACKUP_PREFIX })
    });
    if (!res.ok) {
        const json = await res.json().catch(() => ({}));
        throw new Error(json.error || `List failed: HTTP ${res.status}`);
    }
    const json = await res.json();
    const files = (json.files || []).filter(f => f.name.endsWith('.json')).map(f => ({ ...f, size: parseInt(f.size) || 0 }));
    files.sort((a, b) => new Date(b.lastMod) - new Date(a.lastMod));
    return files;
}

async function deleteWebDAVFile(cfg, filename) {
    const res = await fetch('/api/webdav/delete', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: webdavBody({ filename })
    });
    if (!res.ok) {
        const json = await res.json().catch(() => ({}));
        throw new Error(json.error || `Delete failed: HTTP ${res.status}`);
    }
}

async function downloadFromWebDAV(cfg, filename) {
    const res = await fetch('/api/webdav/download', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: webdavBody({ filename })
    });
    if (!res.ok) {
        const json = await res.json().catch(() => ({}));
        throw new Error(json.error || `Download failed: HTTP ${res.status}`);
    }
    return res.json();
}

async function pruneOldBackups(cfg, files) {
    if (files.length <= cfg.maxBackups) return;
    for (const f of files.slice(cfg.maxBackups)) {
        try { await deleteWebDAVFile(cfg, f.name); } catch (e) { console.warn('Prune failed:', f.name, e); }
    }
}

function generateBackupFilename() {
    const ts = new Date().toISOString().replace(/[-:T]/g, '').split('.')[0];
    return WEBDAV_BACKUP_PREFIX + ts + '.json';
}

async function performWebDAVBackup(silent = false) {
    const cfg = loadWebDAVConfig();
    if (!cfg.url) { if (!silent) showToast('WebDAV not configured', 'warning', 2000); return; }
    await pushWebDAVToServer(loadWebDAVConfig());
    if (commandHistory.length === 0) { if (!silent) showToast('No history to backup', 'warning', 2000); return; }

    try {
        const filename = generateBackupFilename();
        const content = JSON.stringify({ version: '1.0', exportDate: new Date().toISOString(), commands: commandHistory }, null, 2);
        await uploadBackupToWebDAV(cfg, filename, content);
        try {
            const files = await listWebDAVBackups(cfg);
            await pruneOldBackups(cfg, files);
        } catch (e) { console.warn('Pruning failed:', e); }
        updateWebDAVLastBackup();
        if (!silent) showToast(`Backup saved: ${filename}`, 'success', 3000);
    } catch (e) {
        if (!silent) showToast('Backup failed: ' + e.message, 'error', 4000);
        console.error('WebDAV backup error:', e);
    }
}

// Aliases for HTML buttons
function backupNow() { performWebDAVBackup(false); }
function listWebDAVFiles() { refreshWebDAVFileList(); }

function startWebDAVTimer() {
    const cfg = loadWebDAVConfig();
    if (!cfg.url) { showToast('Configure WebDAV URL first', 'warning', 2000); return; }
    if (webdavAutoBackupTimer) clearInterval(webdavAutoBackupTimer);
    webdavAutoBackupTimer = setInterval(() => performWebDAVBackup(true), cfg.interval * 60 * 1000);
    updateWebDAVTimerLabel();
    showToast(`Auto-backup every ${cfg.interval} min`, 'success', 2000);
}

function stopWebDAVTimer() {
    if (webdavAutoBackupTimer) { clearInterval(webdavAutoBackupTimer); webdavAutoBackupTimer = null; }
    updateWebDAVTimerLabel();
    showToast('Auto-backup stopped', 'info', 2000);
}

function updateWebDAVLastBackup() {
    const el = document.getElementById('webdavLastBackup');
    if (el) {
        const now = new Date().toLocaleString();
        el.textContent = 'Last: ' + now;
        localStorage.setItem('webdav_last_backup', now);
    }
}

function updateWebDAVTimerLabel() {
    const el = document.getElementById('webdavTimerStatus');
    if (!el) return;
    const cfg = loadWebDAVConfig();
    if (webdavAutoBackupTimer && cfg.url) {
        el.textContent = `⏱ Auto-backup every ${cfg.interval} min`;
        el.className = 'webdav-timer-status active';
    } else {
        el.textContent = 'Auto-backup: off';
        el.className = 'webdav-timer-status';
    }
}

function escapeHtmlStr(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

async function refreshWebDAVFileList() {
    await pushWebDAVToServer(loadWebDAVConfig());
    const listEl = document.getElementById('webdavFileList');
    if (!listEl) return;
    listEl.innerHTML = '<div class="webdav-loading">Loading backups…</div>';

    const cfg = loadWebDAVConfig();
    if (!cfg.url) {
        listEl.innerHTML = '<div class="webdav-empty">WebDAV not configured. Enter URL and save settings first.</div>';
        return;
    }

    try {
        const files = await listWebDAVBackups(cfg);
        if (files.length === 0) {
            listEl.innerHTML = '<div class="webdav-empty">No backups found on server.</div>';
            return;
        }
        listEl.innerHTML = files.map(f => {
            const date = f.lastMod ? new Date(f.lastMod).toLocaleString() : '—';
            return `
            <div class="webdav-file-item">
                <div class="webdav-file-info">
                    <div class="webdav-file-name">📄 ${escapeHtmlStr(f.name)}</div>
                    <div class="webdav-file-meta">${date} · ${formatFileSize(f.size)}</div>
                </div>
                <div class="webdav-file-actions">
                    <button class="btn btn-ghost" onclick="importFromWebDAV('${escapeHtmlStr(f.name)}')">Import</button>
                    <button class="btn btn-danger" onclick="deleteWebDAVBackup('${escapeHtmlStr(f.name)}', this)">Delete</button>
                </div>
            </div>`;
        }).join('');
    } catch (e) {
        listEl.innerHTML = `<div class="webdav-empty error">Failed to load: ${escapeHtmlStr(e.message)}</div>`;
    }
}

async function importFromWebDAV(filename) {
    await pushWebDAVToServer(loadWebDAVConfig());
    const cfg = loadWebDAVConfig();
    try {
        const data = await downloadFromWebDAV(cfg, filename);
        if (!data.commands || !Array.isArray(data.commands)) throw new Error('Invalid backup format');
        const merge = confirm('Merge with existing history? (Cancel = replace all)');
        if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: merge ? 'merge_history' : 'replace_history', commands: data.commands }));
            showToast('Import request sent!', 'success', 2000);
        } else {
            if (merge) {
                const combined = [...commandHistory, ...data.commands.map(c => typeof c === 'string' ? c : c.command)];
                commandHistory = [...new Set(combined)];
            } else {
                commandHistory = data.commands.map(c => typeof c === 'string' ? c : c.command);
            }
            if (commandHistory.length > MAX_HISTORY) commandHistory = commandHistory.slice(-MAX_HISTORY);
            historyIndex = 0;
            saveCommandHistory();
            renderHistoryList();
            showToast(`Imported ${data.commands.length} commands locally`, 'success', 3000);
        }
        switchHMTab('history');
    } catch (e) {
        showToast('Import failed: ' + e.message, 'error', 4000);
    }
}

async function deleteWebDAVBackup(filename, btnEl) {
    if (!confirm(`Delete backup "${filename}"?`)) return;
    await pushWebDAVToServer(loadWebDAVConfig());
    const cfg = loadWebDAVConfig();
    try {
        await deleteWebDAVFile(cfg, filename);
        const item = btnEl.closest('.webdav-file-item');
        if (item) item.remove();
        showToast('Backup deleted', 'success', 2000);
        const listEl = document.getElementById('webdavFileList');
        if (!listEl.querySelector('.webdav-file-item')) {
            listEl.innerHTML = '<div class="webdav-empty">No backups found on server.</div>';
        }
    } catch (e) {
        showToast('Delete failed: ' + e.message, 'error', 3000);
    }
}

// ============================================================
// Page Init
// ============================================================
window.addEventListener('DOMContentLoaded', () => {
    // Sync UI controls from localStorage
    const settings = loadTerminalSettings();
    const fsSlider = document.getElementById('terminalFontSize');
    const fsValue = document.getElementById('fontSizeValue');
    const scrollbackEl = document.getElementById('scrollbackLines');
    const themeEl = document.getElementById('terminalTheme');
    const fontEl = document.getElementById('terminalFontFamily');

    if (fsSlider) fsSlider.value = settings.fontSize;
    if (fsValue) fsValue.textContent = settings.fontSize + 'px';
    if (scrollbackEl) scrollbackEl.value = settings.scrollback;
    if (themeEl) themeEl.value = settings.themeName;
    if (fontEl) fontEl.value = settings.fontFamily;
    if (fontEl) {
        // Replace the preset font names with this machine's real
        // fonts where obtainable; the focus retry covers the GUI
        // bridge not being injected yet and Local Font Access
        // needing a user gesture first.
        populateFontOptions();
        fontEl.addEventListener('focus', populateFontOptions);
    }

    // Font size slider live preview
    if (fsSlider) {
        fsSlider.addEventListener('input', function () {
            if (fsValue) fsValue.textContent = this.value + 'px';
            portTerminals.forEach(pt => {
                try { pt.terminal.options.fontSize = parseInt(this.value); } catch(e) {}
                if (pt.fitAddon) try { pt.fitAddon.fit(); } catch(e) {}
            });
        });
    }

    // WebDAV init
    populateWebDAVForm();
    const lastBackup = localStorage.getItem('webdav_last_backup');
    const lastBackupEl = document.getElementById('webdavLastBackup');
    if (lastBackup && lastBackupEl) lastBackupEl.textContent = 'Last: ' + lastBackup;

    // Line ending init
    const lineEndingEl = document.getElementById('lineEndingSelect');
    if (lineEndingEl) {
        const savedLE = localStorage.getItem('serial_line_ending');
        if (savedLE !== null) lineEndingEl.value = savedLE;
    }

    // Theme toggle init
    const themeBtn = document.getElementById('themeToggle');
    const savedTheme = localStorage.getItem('theme') || 'dark';
    if (themeBtn) themeBtn.textContent = savedTheme === 'dark' ? '🌙' : '☀️';

    // History dropdown click handling via event delegation
    const historyDropdown = document.getElementById('historyDropdown');
    if (historyDropdown) {
        historyDropdown.addEventListener('click', (e) => {
            const item = e.target.closest('.history-item');
            if (!item) return;
            const idx = parseInt(item.dataset.index);
            const matches = JSON.parse(historyDropdown.dataset.matches || '[]');
            if (matches[idx] !== undefined) {
                selectHistoryItem(matches[idx]);
            }
        });
    }

    // Update history badge count
    const navHistoryBadge = document.getElementById('navHistoryBadge');
    if (navHistoryBadge && typeof commandHistory !== 'undefined') {
        navHistoryBadge.textContent = commandHistory.length;
    }

    // WebSocket
    initWebSocket();
});

// ============================================================
// Metacube Multi-View Navigation Manager
// ============================================================
let currentView = 'terminal';

function switchView(viewName) {
    currentView = viewName;
    
    // Update navigation items
    document.querySelectorAll('.sidebar-nav .nav-item').forEach(item => {
        item.classList.toggle('active', item.dataset.view === viewName);
    });

    // Update view panels
    const viewMap = {
        terminal: 'viewTerminal',
        ports: 'viewPorts',
        history: 'viewHistory',
        webdav: 'viewWebDAV',
        settings: 'viewSettings'
    };

    Object.keys(viewMap).forEach(v => {
        const el = document.getElementById(viewMap[v]);
        if (el) el.classList.toggle('active', v === viewName);
    });

    if (viewName === 'terminal') {
        fitTerminal();
        const pt = getActivePT();
        if (pt) setTimeout(() => pt.terminal.focus(), 50);
    } else if (viewName === 'ports') {
        renderPortsView();
    } else if (viewName === 'history') {
        renderHistoryView();
    } else if (viewName === 'webdav') {
        populateWebDAVForm();
        refreshWebDAVFileList();
    }
}

// ============================================================
// Ports Management View
// ============================================================
function renderPortsView() {
    const grid = document.getElementById('portsCardGrid');
    if (!grid) return;

    const portIds = Object.keys(portStatus || {}).sort();
    if (portIds.length === 0) {
        grid.innerHTML = `
            <div style="grid-column: 1/-1; text-align:center; padding: 48px 20px; color: var(--text-muted);">
                <svg viewBox="0 0 24 24" style="font-size: 40px; opacity:0.4; margin-bottom: 12px;"><path d="M16 7V3h-2v4h-4V3H8v4H6c-1.1 0-2 .9-2 2v5c0 3.31 2.69 6 6 6v3h4v-3c3.31 0 6-2.69 6-6V9c0-1.1-.9-2-2-2h-2zm0 7c0 2.21-1.79 4-4 4s-4-1.79-4-4V9h8v5z"/></svg>
                <div style="font-size:15px; font-weight:600; margin-bottom:4px;">未发现可用串行端口</div>
                <div style="font-size:13px; margin-bottom:16px;">请插入 USB 转串口设备，或手动添加自定义端口路径</div>
                <button class="btn primary" onclick="showAddPortModal()">添加自定义端口</button>
            </div>
        `;
        return;
    }

    grid.innerHTML = portIds.map(portId => {
        const info = portStatus[portId] || {};
        const isConn = info.connected || info.status === 'connected';
        const isConnecting = info.status === 'connecting';
        const isActive = portId === activePortId;
        const alias = info.alias || '';
        const desc = info.description || '串行接口设备';
        const baud = info.baudrate || 115200;
        const databits = info.databits || 8;
        const stopbits = info.stopbit || 1;
        const parity = info.parity || 'N';

        return `
            <div class="port-card ${isActive ? 'active-port' : ''}" id="card-port-${escapeHtmlStr(portId)}">
                <div class="port-card-header">
                    <div class="port-card-title">
                        <svg viewBox="0 0 24 24" width="16" height="16" style="color:var(--accent);"><path d="M16 7V3h-2v4h-4V3H8v4H6c-1.1 0-2 .9-2 2v5c0 3.31 2.69 6 6 6v3h4v-3c3.31 0 6-2.69 6-6V9c0-1.1-.9-2-2-2h-2zm0 7c0 2.21-1.79 4-4 4s-4-1.79-4-4V9h8v5z"/></svg>
                        <span>${escapeHtmlStr(alias || portId)}</span>
                    </div>
                    <span class="port-status-badge ${isConn ? 'connected' : (isConnecting ? 'connecting' : 'disconnected')}">
                        <span class="status-dot ${isConn ? 'connected' : (isConnecting ? 'connecting' : '')}"></span>
                        ${isConn ? '已连接' : (isConnecting ? '连接中' : '未连接')}
                    </span>
                </div>
                <div class="port-card-sub" title="${escapeHtmlStr(portId + (desc ? ' - ' + desc : ''))}">
                    ${escapeHtmlStr(portId)}${desc ? ' · ' + escapeHtmlStr(desc) : ''}
                </div>

                <div class="form-group" style="margin-top: 4px;">
                    <label>设备别名 (Alias)</label>
                    <input type="text" class="input" value="${escapeHtmlStr(alias)}" placeholder="设置便于识别的别名"
                           onchange="savePortCardAlias('${escapeHtmlStr(portId)}', this.value)">
                </div>

                <div class="form-row">
                    <div class="form-group">
                        <label>波特率</label>
                        <select class="select" id="card-baud-${escapeHtmlStr(portId)}">
                            ${[9600, 19200, 38400, 57600, 115200, 230400, 460800, 921600].map(b => `<option value="${b}" ${b === baud ? 'selected' : ''}>${b}</option>`).join('')}
                        </select>
                    </div>
                    <div class="form-group">
                        <label>数据位</label>
                        <select class="select" id="card-data-${escapeHtmlStr(portId)}">
                            <option value="8" ${databits === 8 ? 'selected' : ''}>8</option>
                            <option value="7" ${databits === 7 ? 'selected' : ''}>7</option>
                        </select>
                    </div>
                </div>

                <div class="form-row">
                    <div class="form-group">
                        <label>停止位</label>
                        <select class="select" id="card-stop-${escapeHtmlStr(portId)}">
                            <option value="1" ${stopbits === 1 ? 'selected' : ''}>1</option>
                            <option value="2" ${stopbits === 2 ? 'selected' : ''}>2</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label>校验位</label>
                        <select class="select" id="card-parity-${escapeHtmlStr(portId)}">
                            <option value="N" ${parity === 'N' ? 'selected' : ''}>None (无)</option>
                            <option value="O" ${parity === 'O' ? 'selected' : ''}>Odd (奇)</option>
                            <option value="E" ${parity === 'E' ? 'selected' : ''}>Even (偶)</option>
                        </select>
                    </div>
                </div>

                <div class="port-card-actions">
                    ${isConn 
                        ? `<button class="btn danger" onclick="disconnectPortById('${escapeHtmlStr(portId)}')" style="flex:1;">断开连接</button>`
                        : `<button class="btn primary" onclick="connectPortById('${escapeHtmlStr(portId)}')" style="flex:1;">连接端口</button>`
                    }
                    <button class="btn" onclick="savePortCardConfig('${escapeHtmlStr(portId)}')" title="保存参数配置">保存</button>
                    <button class="btn" onclick="openPortInTerminal('${escapeHtmlStr(portId)}')" title="切换到终端">进入终端</button>
                </div>
            </div>
        `;
    }).join('');
}

function savePortCardAlias(portId, alias) {
    alias = alias.trim();
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'set_port_alias', port_id: portId, alias }));
        showToast(alias ? `别名已设置: ${alias}` : '别名已清除', 'success', 2000);
    }
}

function savePortCardConfig(portId) {
    const baudEl = document.getElementById(`card-baud-${portId}`);
    const dataEl = document.getElementById(`card-data-${portId}`);
    const stopEl = document.getElementById(`card-stop-${portId}`);
    const parityEl = document.getElementById(`card-parity-${portId}`);
    if (!baudEl || !ws || ws.readyState !== WebSocket.OPEN) return;

    ws.send(JSON.stringify({
        type: 'configure_port',
        port_id: portId,
        baudrate: parseInt(baudEl.value),
        databits: parseInt(dataEl.value),
        stopbit: parseFloat(stopEl.value),
        parity: parityEl.value,
        timeout: 10
    }));
    showToast(`端口 ${portId} 参数已更新`, 'success', 2000);
}

function connectPortById(portId) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const baudEl = document.getElementById(`card-baud-${portId}`);
    const dataEl = document.getElementById(`card-data-${portId}`);
    const stopEl = document.getElementById(`card-stop-${portId}`);
    const parityEl = document.getElementById(`card-parity-${portId}`);

    const baudrate = baudEl ? parseInt(baudEl.value) : 115200;
    const databits = dataEl ? parseInt(dataEl.value) : 8;
    const stopbit = stopEl ? parseFloat(stopEl.value) : 1;
    const parity = parityEl ? parityEl.value : 'N';

    ws.send(JSON.stringify({
        type: 'connect_port',
        port_id: portId,
        baudrate,
        databits,
        stopbit,
        parity,
        timeout: 10
    }));
    showToast(`正在连接 ${portId}...`, 'info', 2000);
}

function disconnectPortById(portId) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'disconnect_port', port_id: portId }));
    showToast(`正在断开 ${portId}...`, 'warning', 2000);
}

function openPortInTerminal(portId) {
    switchToPort(portId);
    switchView('terminal');
}

// ============================================================
// Command History View
// ============================================================
let _historyFilter = '';

function renderHistoryView(filter = _historyFilter) {
    _historyFilter = filter || '';
    const listEl = document.getElementById('historyListView');
    if (!listEl) return;

    const navBadge = document.getElementById('navHistoryBadge');
    if (navBadge) navBadge.textContent = commandHistory.length;

    let items = commandHistory;
    if (_historyFilter.trim()) {
        const q = _historyFilter.toLowerCase();
        items = items.filter(cmd => cmd.toLowerCase().includes(q));
    }

    if (items.length === 0) {
        listEl.innerHTML = `
            <div style="text-align:center; padding: 40px 20px; color: var(--text-muted); font-size: 13px;">
                ${commandHistory.length === 0 ? '暂无历史发送命令' : '没有匹配搜索条件的命令'}
            </div>
        `;
        return;
    }

    listEl.innerHTML = items.map((cmd) => {
        const realIndex = commandHistory.indexOf(cmd);
        return `
            <div class="history-card-item">
                <div class="history-cmd-text">${escapeHtmlStr(cmd)}</div>
                <div class="history-item-actions">
                    <button class="icon-btn" onclick="copyHistoryCmd(${realIndex})" title="复制命令">
                        <svg viewBox="0 0 24 24"><path d="M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z"/></svg>
                    </button>
                    <button class="icon-btn" onclick="sendHistoryCmd(${realIndex})" title="填入终端并切换" style="color:var(--accent);">
                        <svg viewBox="0 0 24 24"><path d="M2.01 21L23 12 2.01 3 2 10l15 2-15 2z"/></svg>
                    </button>
                    <button class="icon-btn" onclick="editHistoryItem(${realIndex})" title="编辑">
                        <svg viewBox="0 0 24 24"><path d="M3 17.25V21h3.75L17.81 9.94l-3.75-3.75L3 17.25zM20.71 7.04c.39-.39.39-1.02 0-1.41l-2.34-2.34c-.39-.39-1.02-.39-1.41 0l-1.83 1.83 3.75 3.75 1.83-1.83z"/></svg>
                    </button>
                    <button class="icon-btn" onclick="deleteHistoryItem(${realIndex})" title="删除" style="color:var(--danger);">
                        <svg viewBox="0 0 24 24"><path d="M6 19c0 1.1.9 2 2 2h8c1.1 0 2-.9 2-2V7H6v12zM19 4h-3.5l-1-1h-5l-1 1H5v2h14V4z"/></svg>
                    </button>
                </div>
            </div>
        `;
    }).join('');
}

function filterHistoryView(text) {
    renderHistoryView(text);
}

function copyHistoryCmd(index) {
    if (commandHistory[index]) {
        copyToClipboard(commandHistory[index]);
    }
}

function sendHistoryCmd(index) {
    if (commandHistory[index]) {
        const input = document.getElementById('terminalInput');
        if (input) input.value = commandHistory[index];
        switchView('terminal');
        if (input) {
            input.focus();
            autoResizeTextarea(false);
        }
    }
}

// Override edit modal opener to support modal-overlay.active
const _origEditHistoryItem = editHistoryItem;
editHistoryItem = function(index) {
    editingIndex = index;
    const modal = document.getElementById('editModal');
    const textarea = document.getElementById('editTextarea');
    if (modal && textarea) {
        textarea.value = commandHistory[index];
        modal.classList.add('active');
        modal.style.display = 'flex';
        textarea.focus();
    }
};

const _origCloseEditModal = closeEditModal;
closeEditModal = function(event) {
    if (event && event.target !== event.currentTarget) return;
    const modal = document.getElementById('editModal');
    if (modal) {
        modal.classList.remove('active');
        modal.style.display = 'none';
    }
    editingIndex = -1;
};

// ============================================================
// Add Custom Port Modal
// ============================================================
function showAddPortModal() {
    const m = document.getElementById('addPortModal');
    if (m) {
        m.classList.add('active');
        m.style.display = 'flex';
        const p = document.getElementById('modalPortPath');
        if (p) { p.value = ''; p.focus(); }
    }
}

function closeAddPortModal() {
    const m = document.getElementById('addPortModal');
    if (m) {
        m.classList.remove('active');
        m.style.display = 'none';
    }
}

function submitAddPortModal() {
    const pathInput = document.getElementById('modalPortPath');
    const aliasInput = document.getElementById('modalPortAlias');
    const port = pathInput ? pathInput.value.trim() : '';
    const alias = aliasInput ? aliasInput.value.trim() : '';
    if (!port) {
        showToast('请输入端口名称或路径', 'warning', 2000);
        return;
    }

    if (!portStatus[port]) {
        portStatus[port] = { connected: false, alias };
    } else if (alias) {
        portStatus[port].alias = alias;
    }

    closeAddPortModal();
    updatePortList(portStatus);
    switchToPort(port);
    switchView('terminal');

    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'discover_port', port }));
        if (alias) {
            ws.send(JSON.stringify({ type: 'set_port_alias', port_id: port, alias }));
        }
    }
    showToast(`已添加端口: ${port}`, 'success', 2000);
}

// ============================================================
// Service Reconnect & Settings Handlers
// ============================================================
function reconnectWebSocket() {
    showToast('正在重连服务...', 'info', 2000);
    if (ws) {
        try { ws.close(); } catch(e) {}
    }
    initWebSocket();
}

function updateLineEnding(val) {
    localStorage.setItem('serial_line_ending', val);
    showToast('换行符设置已更新', 'success', 1500);
}
