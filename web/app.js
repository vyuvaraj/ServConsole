// ServConsole Frontend Controller

const STATE = {
  activeTab: 'gateways',
  components: {
    ServGate: { online: false, latency: 0, details: null },
    ServQueue: { online: false, latency: 0, details: null },
    ServStore: { online: false, latency: 0, details: null }
  },
  routes: [],
  buckets: [],
  selectedBucket: null,
  objects: [],
  traces: [],
  logs: []
};

// Colors for storage nodes in ring visualizer
const NODE_COLORS = [
  '#a855f7', // purple
  '#06b6d4', // cyan
  '#10b981', // green
  '#f59e0b', // orange
  '#ef4444'  // red
];

document.addEventListener('DOMContentLoaded', () => {
  checkAuthConfig();
  initTabs();
  initForms();
  initPolling();
  initRingCanvas();
  initAuditLogsUI();
});

// --- Tab Switching ---
function initTabs() {
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const tabId = btn.getAttribute('data-tab');
      
      // Update UI active states
      document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
      document.querySelectorAll('.tab-pane').forEach(p => p.classList.remove('active'));
      
      btn.classList.add('active');
      const pane = document.getElementById(`tab-${tabId}`);
      if (pane) pane.classList.add('active');
      
      STATE.activeTab = tabId;
      logEvent('system', `Switched to tab: ${tabId}`);
      
      if (tabId === 'storage') {
        fetchStorageRing();
        fetchBuckets();
        fetchClusterHealth();
      } else if (tabId === 'traces') {
        fetchTraces();
      } else if (tabId === 'audit') {
        fetchAuditLogs();
      } else if (tabId === 'database') {
        fetchDatabaseSchemas();
      }
    });
  });
}

// --- Polling & Status Aggregation ---
function initPolling() {
  const poll = async () => {
    try {
      const res = await fetch('/api/status');
      if (res.status === 401) {
        showLoginScreen();
        return;
      }
      const data = await res.json();
      
      // Update local state
      data.components.forEach(comp => {
        STATE.components[comp.name] = {
          online: comp.online,
          latency: comp.latency_ms,
          details: comp.details
        };
      });
      
      updateSummaryUI();
      
      // If we are on gateways tab, refresh routes list if details changed
      if (STATE.activeTab === 'gateways') {
        refreshRoutesList();
      } else if (STATE.activeTab === 'queues') {
        refreshQueuesList();
      }
    } catch (err) {
      logEvent('error', `Status polling failed: ${err.message}`);
    }
  };
  
  poll();
  setInterval(poll, 3000);
}

function updateSummaryUI() {
  // Global Status
  const allOnline = Object.values(STATE.components).every(c => c.online);
  const statusDot = document.getElementById('global-status-dot');
  const statusText = document.getElementById('global-status-text');
  
  if (allOnline) {
    statusDot.className = 'status-indicator online';
    statusText.textContent = 'Ecosystem Online';
  } else {
    statusDot.className = 'status-indicator offline';
    statusText.textContent = 'Degraded State Detected';
  }
  
  // ServGate Card
  const gate = STATE.components.ServGate;
  const gateCard = document.getElementById('gate-summary-card');
  const gateLatency = document.getElementById('gate-latency');
  if (gate.online) {
    gateCard.querySelector('.badge').className = 'badge online';
    gateCard.querySelector('.badge').textContent = 'ONLINE';
    gateLatency.textContent = `${gate.latency} ms`;
  } else {
    gateCard.querySelector('.badge').className = 'badge offline';
    gateCard.querySelector('.badge').textContent = 'OFFLINE';
    gateLatency.textContent = '— ms';
  }
  
  // ServQueue Card
  const queue = STATE.components.ServQueue;
  const queueCard = document.getElementById('queue-summary-card');
  const queueMsgs = document.getElementById('queue-messages');
  if (queue.online) {
    queueCard.querySelector('.badge').className = 'badge online';
    queueCard.querySelector('.badge').textContent = 'ONLINE';
    const published = queue.details?.metrics?.messages_published_total || 0;
    queueMsgs.textContent = `${published} msg`;
  } else {
    queueCard.querySelector('.badge').className = 'badge offline';
    queueCard.querySelector('.badge').textContent = 'OFFLINE';
    queueMsgs.textContent = '— msg';
  }
  
  // ServStore Card
  const store = STATE.components.ServStore;
  const storeCard = document.getElementById('store-summary-card');
  const storeBuckets = document.getElementById('store-buckets');
  if (store.online) {
    storeCard.querySelector('.badge').className = 'badge online';
    storeCard.querySelector('.badge').textContent = 'ONLINE';
    const metricBkts = store.details?.Metrics?.BucketsCount || 0;
    storeBuckets.textContent = `${metricBkts} bkts`;
  } else {
    storeCard.querySelector('.badge').className = 'badge offline';
    storeCard.querySelector('.badge').textContent = 'OFFLINE';
    storeBuckets.textContent = '— bkts';
  }
}

// --- Gateways Tab: Routes & WASM ---
async function refreshRoutesList() {
  try {
    const res = await fetch('/api/routes');
    if (!res.ok) return;
    const routes = await res.json();
    STATE.routes = routes;
    
    const tbody = document.querySelector('#routes-table tbody');
    tbody.innerHTML = '';
    
    if (routes.length === 0) {
      tbody.innerHTML = `<tr><td colspan="6" class="text-center text-muted">No routes configured</td></tr>`;
      return;
    }
    
    routes.forEach(route => {
      const tr = document.createElement('tr');
      tr.innerHTML = `
        <td><strong style="color:var(--primary); font-family:var(--font-mono);">${route.prefix}</strong></td>
        <td><span style="font-family:var(--font-mono);">${route.target || route.targets?.join(', ')}</span></td>
        <td>${route.rate_limit_rpm ? `${route.rate_limit_rpm} rpm` : '—'}</td>
        <td>${route.prompt_guard ? '✅ Active' : '—'}</td>
        <td>${route.semantic_cache ? '✅ Active' : '—'}</td>
        <td>${route.pii_redact ? '✅ Active' : '—'}</td>
      `;
      tbody.appendChild(tr);
    });
  } catch (err) {
    // Graceful fallback if /api/routes is not fully integrated yet
  }
}

// --- Queues Tab: Transforms & Messaging ---
function refreshQueuesList() {
  const queue = STATE.components.ServQueue;
  const tbody = document.querySelector('#queues-table tbody');
  tbody.innerHTML = '';
  
  if (!queue.online || !queue.details) {
    tbody.innerHTML = `<tr><td colspan="4" class="text-center text-muted">ServQueue is offline</td></tr>`;
    return;
  }
  
  // Render STOMP / general queue stats
  const metrics = queue.details.metrics || {};
  const tr = document.createElement('tr');
  tr.innerHTML = `
    <td><strong>stomp://localhost:61613</strong> (Default STOMP)</td>
    <td>WASM Engine Loaded</td>
    <td>
      In: ${metrics.messages_published_total || 0} <br>
      WASM Execs: ${metrics.wasm_executions_total || 0}
    </td>
    <td>
      <button class="btn btn-secondary btn-sm" onclick="clearWasmTransform('default')">Reset Filters</button>
    </td>
  `;
  tbody.appendChild(tr);
}

// --- Storage Tab: Consistent Ring, Buckets, Objects ---
async function fetchStorageRing() {
  try {
    const res = await fetch('/api/proxy/store/console/cluster/ring');
    if (!res.ok) return;
    const ringData = await res.json();
    
    // Draw the ring
    drawHashRing(ringData);
    
    // Render Node List legend
    const list = document.getElementById('ring-nodes-list');
    list.innerHTML = '';
    
    const nodes = ringData.Nodes || [];
    if (nodes.length === 0) {
      list.innerHTML = `<li class="text-muted">No nodes active in storage cluster</li>`;
      return;
    }
    
    nodes.forEach((node, index) => {
      const color = NODE_COLORS[index % NODE_COLORS.length];
      const li = document.createElement('li');
      li.className = 'ring-node-item';
      li.innerHTML = `
        <span class="node-color-dot" style="background-color: ${color}"></span>
        <span>Node: <strong>${node}</strong></span>
      `;
      list.appendChild(li);
    });
  } catch (err) {
    logEvent('error', `Failed to load consistent ring: ${err.message}`);
  }
}

async function fetchBuckets() {
  try {
    // S3 client bucket listing fallback
    const res = await fetch('/api/proxy/store/console/metrics');
    if (!res.ok) return;
    const metrics = await res.json();
    
    // In our simplified mock or local store, let's render buckets
    const containers = document.getElementById('buckets-container');
    containers.innerHTML = '';
    
    const bucketsCount = metrics.BucketsCount || 0;
    if (bucketsCount === 0) {
      containers.innerHTML = `<span class="text-muted p-2">No buckets</span>`;
      return;
    }
    
    // If there are buckets, fetch/render names
    // For local convenience, let's load dummy buckets based on count or mock
    const dummyBuckets = ['media-assets', 'logs', 'user-documents'].slice(0, bucketsCount);
    if (dummyBuckets.length === 0) dummyBuckets.push('default-bucket');
    
    dummyBuckets.forEach(b => {
      const div = document.createElement('div');
      div.className = `bucket-item ${STATE.selectedBucket === b ? 'active' : ''}`;
      div.innerHTML = `📁 ${b}`;
      div.addEventListener('click', () => selectBucket(b));
      containers.appendChild(div);
    });
  } catch (err) {
    logEvent('error', `Failed to load buckets: ${err.message}`);
  }
}

function selectBucket(name) {
  STATE.selectedBucket = name;
  document.getElementById('current-bucket-name').textContent = name;
  document.querySelectorAll('.bucket-item').forEach(el => {
    el.classList.toggle('active', el.textContent.includes(name));
  });
  fetchObjects(name);
}

async function fetchObjects(bucket) {
  const tbody = document.querySelector('#objects-table tbody');
  tbody.innerHTML = `<tr><td colspan="3" class="text-center">Loading objects...</td></tr>`;
  
  try {
    // Simulate S3 object listing via proxy
    const dummyObjects = [
      { key: 'images/hero.png', size: '2.4 MB', modified: '2026-06-17 12:04' },
      { key: 'docs/guide.pdf', size: '420 KB', modified: '2026-06-17 14:15' },
      { key: 'backup.zip', size: '84.2 MB', modified: '2026-06-17 15:02' }
    ];
    
    tbody.innerHTML = '';
    dummyObjects.forEach(obj => {
      const tr = document.createElement('tr');
      tr.innerHTML = `
        <td style="font-family:var(--font-mono);">${obj.key}</td>
        <td>${obj.size}</td>
        <td>${obj.modified}</td>
      `;
      tbody.appendChild(tr);
    });
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="3" class="text-center text-danger">Failed to load objects</td></tr>`;
  }
}

// --- Traces Tab: OpenTelemetry waterfall ---
async function fetchTraces() {
  const container = document.getElementById('traces-timeline');
  container.innerHTML = `<p class="text-center">Fetching OpenTelemetry trace spans...</p>`;
  
  try {
    const res = await fetch('/api/proxy/store/console/traces');
    if (!res.ok) {
      container.innerHTML = `<p class="text-center text-muted">OTel endpoints not responding</p>`;
      return;
    }
    const spans = await res.json();
    STATE.traces = spans || [];
    
    renderTracesTimeline();
  } catch (err) {
    container.innerHTML = `<p class="text-center text-danger">Error: ${err.message}</p>`;
  }
}

function renderTracesTimeline() {
  const container = document.getElementById('traces-timeline');
  container.innerHTML = '';
  
  if (STATE.traces.length === 0) {
    container.innerHTML = `<p class="text-center text-muted">No recent trace spans captured</p>`;
    return;
  }
  
  // Calculate relative widths based on latency
  let maxDur = 1;
  STATE.traces.forEach(s => {
    if (s.DurationNs > maxDur) maxDur = s.DurationNs;
  });
  
  STATE.traces.forEach(span => {
    const row = document.createElement('div');
    row.className = 'trace-span-row';
    
    const durMs = (span.DurationNs / 1000000).toFixed(2);
    const widthPct = Math.max(2, (span.DurationNs / maxDur) * 100);
    const startStr = new Date(span.StartTime).toLocaleTimeString();
    
    row.innerHTML = `
      <div class="span-header">
        <div>
          <span class="span-name">${span.Name}</span>
          <span class="span-service">${span.ServiceName || 'servstore'}</span>
        </div>
        <div style="font-family:var(--font-mono); font-size:0.75rem;">
          ${durMs} ms <span style="color:var(--text-muted)">@ ${startStr}</span>
        </div>
      </div>
      <div class="span-bar-wrapper">
        <div class="span-bar" style="width: ${widthPct}%"></div>
      </div>
    `;
    container.appendChild(row);
  });
}

// --- Consistent Hash Ring Drawing ---
function drawHashRing(ringData) {
  const canvas = document.getElementById('ring-canvas');
  if (!canvas) return;
  const ctx = canvas.getContext('2d');
  
  const width = canvas.width;
  const height = canvas.height;
  const centerX = width / 2;
  const centerY = height / 2;
  const radius = width / 2 - 30;
  
  ctx.clearRect(0, 0, width, height);
  
  // Draw circular track
  ctx.beginPath();
  ctx.arc(centerX, centerY, radius, 0, 2 * Math.PI);
  ctx.strokeStyle = 'rgba(255, 255, 255, 0.08)';
  ctx.lineWidth = 4;
  ctx.stroke();
  
  const nodes = ringData.Nodes || [];
  const tokens = ringData.Tokens || []; // token keys
  const tokenToNode = ringData.TokenToNode || {};
  
  if (nodes.length === 0) {
    // Draw empty ring text
    ctx.font = '14px Outfit';
    ctx.fillStyle = 'rgba(255,255,255,0.3)';
    ctx.textAlign = 'center';
    ctx.fillText('No active storage cluster', centerX, centerY);
    return;
  }
  
  // Distribute nodes visually on the ring
  nodes.forEach((node, index) => {
    const angle = (index / nodes.length) * 2 * Math.PI - Math.PI / 2;
    const x = centerX + radius * Math.cos(angle);
    const y = centerY + radius * Math.sin(angle);
    
    const color = NODE_COLORS[index % NODE_COLORS.length];
    
    // Draw glowing node dot
    ctx.beginPath();
    ctx.arc(x, y, 10, 0, 2 * Math.PI);
    ctx.fillStyle = color;
    ctx.shadowBlur = 15;
    ctx.shadowColor = color;
    ctx.fill();
    ctx.shadowBlur = 0; // reset
    
    // Draw node name label
    ctx.font = 'bold 10px Outfit';
    ctx.fillStyle = '#fff';
    ctx.textAlign = 'center';
    ctx.fillText(node.split(':')[0], x, y - 16);
  });
}

function initRingCanvas() {
  // Mock ring visualizer if cluster is down
  drawHashRing({
    Nodes: ['node1:8081', 'node2:8081', 'node3:8081']
  });
}

// --- Interactive Forms & Submissions ---
function initForms() {
  // Modal toggle
  const modal = document.getElementById('add-route-modal');
  document.getElementById('btn-add-route').addEventListener('click', () => {
    modal.classList.add('active');
  });
  document.getElementById('modal-close-btn').addEventListener('click', () => {
    modal.classList.remove('active');
  });
  
  // Register API Route
  document.getElementById('add-route-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const prefix = document.getElementById('route-prefix').value;
    const target = document.getElementById('route-target').value;
    const rpm = parseInt(document.getElementById('route-rpm').value) || 0;
    const prompt_guard = document.getElementById('route-ai-guard').checked;
    const semantic_cache = document.getElementById('route-ai-cache').checked;
    const pii_redact = document.getElementById('route-ai-pii').checked;
    
    try {
      const res = await fetch('/api/routes', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          prefix, target, rate_limit_rpm: rpm, prompt_guard, semantic_cache, pii_redact
        })
      });
      
      if (res.ok) {
        logEvent('gateway', `Added API Route: ${prefix} -> ${target}`);
        modal.classList.remove('active');
        e.target.reset();
        refreshRoutesList();
      } else {
        alert('Failed to register route');
      }
    } catch (err) {
      logEvent('error', `Route creation error: ${err.message}`);
    }
  });

  // Hot-swap WASM Middleware
  document.getElementById('wasm-upload-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const fileInput = document.getElementById('wasm-file-input');
    const nameInput = document.getElementById('wasm-name-input');
    const statusMsg = document.getElementById('wasm-status-message');
    
    if (fileInput.files.length === 0) {
      statusMsg.className = 'status-message error';
      statusMsg.textContent = 'Please select a WASM file first';
      return;
    }
    
    const file = fileInput.files[0];
    const name = nameInput.value.trim();
    
    try {
      statusMsg.className = 'status-message';
      statusMsg.textContent = 'Uploading and compiling WASM...';
      
      const fileBytes = await file.arrayBuffer();
      const res = await fetch(`/api/proxy/gate/api/admin/middleware/${name}`, {
        method: 'POST',
        body: fileBytes
      });
      
      if (res.ok) {
        statusMsg.className = 'status-message success';
        statusMsg.textContent = `✓ Middleware "${name}" successfully registered & hot-swapped!`;
        logEvent('gateway', `WASM middleware "${name}" hot-swapped successfully.`);
        nameInput.value = '';
        fileInput.value = '';
      } else {
        const text = await res.text();
        statusMsg.className = 'status-message error';
        statusMsg.textContent = `Error: ${text}`;
        logEvent('error', `WASM compilation failed: ${text}`);
      }
    } catch (err) {
      statusMsg.className = 'status-message error';
      statusMsg.textContent = `Network Error: ${err.message}`;
    }
  });

  // Attach Queue transform
  document.getElementById('queue-transform-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const topic = document.getElementById('transform-topic').value.trim();
    const fileInput = document.getElementById('transform-file');
    const status = document.getElementById('queue-transform-status');
    
    if (fileInput.files.length === 0) return;
    
    try {
      status.className = 'status-message';
      status.textContent = 'Registering WASM transform filter...';
      
      const fileBytes = await fileInput.files[0].arrayBuffer();
      const res = await fetch(`/api/proxy/queue/api/topics/${topic}/transform`, {
        method: 'POST',
        body: fileBytes
      });
      
      if (res.ok) {
        status.className = 'status-message success';
        status.textContent = `✓ Transform filter registered for topic: ${topic}`;
        logEvent('queue', `WASM transform attached to queue: ${topic}`);
      } else {
        const errText = await res.text();
        status.className = 'status-message error';
        status.textContent = `Error: ${errText}`;
      }
    } catch (err) {
      status.className = 'status-message error';
      status.textContent = `Error: ${err.message}`;
    }
  });

  // Publish STOMP Message
  document.getElementById('publish-message-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const topic = document.getElementById('pub-topic').value;
    const payload = document.getElementById('pub-payload').value;
    const status = document.getElementById('publish-status');
    
    try {
      const res = await fetch('/api/proxy/queue/api/publish', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ topic, payload })
      });
      
      if (res.ok) {
        const data = await res.json();
        status.className = 'status-message success';
        status.textContent = `✓ Delivered: ${data.delivered_payload}`;
        logEvent('queue', `STOMP message published to topic [${topic}]: ${payload}`);
      } else {
        const errText = await res.text();
        status.className = 'status-message error';
        status.textContent = `Failed: ${errText}`;
      }
    } catch (err) {
      status.className = 'status-message error';
      status.textContent = `Error: ${err.message}`;
    }
  });

  // Hash Ring Placement Trace Checker
  document.getElementById('btn-check-placement').addEventListener('click', async () => {
    const bucket = document.getElementById('placement-bucket').value.trim();
    const key = document.getElementById('placement-key').value.trim();
    const resultBox = document.getElementById('placement-result');
    
    if (!bucket || !key) {
      alert('Please fill out both bucket and key');
      return;
    }
    
    try {
      const res = await fetch(`/api/proxy/store/console/cluster/placement?bucket=${encodeURIComponent(bucket)}&key=${encodeURIComponent(key)}`);
      if (res.ok) {
        const data = await res.json();
        document.getElementById('placement-node-id').textContent = data.node_id;
        document.getElementById('placement-node-addr').textContent = data.address;
        resultBox.style.display = 'block';
        logEvent('store', `Traced key [${bucket}/${key}] -> Node: ${data.node_id}`);
      } else {
        alert('Placement check failed');
      }
    } catch (err) {
      alert('Network Error during placement check');
    }
  });
  
  // Telemetry triggers
  document.getElementById('btn-refresh-traces').addEventListener('click', fetchTraces);
  document.getElementById('btn-clear-logs').addEventListener('click', () => {
    document.getElementById('console-logs-screen').innerHTML = '';
  });
}

// --- Console Log Event helper ---
function logEvent(service, message) {
  const consoleScreen = document.getElementById('console-logs-screen');
  if (!consoleScreen) return;
  
  const line = document.createElement('div');
  line.className = `log-line ${service}`;
  
  const time = new Date().toLocaleTimeString();
  line.innerHTML = `[${time}] <span class="tag">[${service.toUpperCase()}]</span> ${message}`;
  
  consoleScreen.appendChild(line);
  consoleScreen.scrollTop = consoleScreen.scrollHeight;
}

// ─── Phase 3: Cluster Operations & Repair Panel ───────────────────────────

async function fetchClusterHealth() {
  try {
    const res = await fetch('/api/cluster');
    if (!res.ok) return;
    const data = await res.json();
    renderClusterNodes(data);
    renderErasureCoding(data);
  } catch (err) {
    logEvent('error', `Cluster health fetch failed: ${err.message}`);
  }
}

function renderClusterNodes(data) {
  const tbody = document.getElementById('cluster-nodes-body');
  if (!tbody) return;
  tbody.innerHTML = '';

  const nodes = data.nodes || [];
  if (nodes.length === 0) {
    tbody.innerHTML = `<tr><td colspan="6" class="text-center text-muted">No cluster nodes detected — ServStore may be running in single-node mode</td></tr>`;
    return;
  }

  nodes.forEach(node => {
    const lagSec = (node.last_seen_ago_ms / 1000).toFixed(1);
    const lagLabel = node.last_seen_ago_ms === 0 ? 'Local node' : `${lagSec}s ago`;
    const statusDot = node.status === 'online'
      ? `<span style="color:var(--success)">● Online</span>`
      : `<span style="color:var(--danger)">● Offline</span>`;
    const region = node.region || '—';

    const tr = document.createElement('tr');
    tr.innerHTML = `
      <td style="font-family:var(--font-mono);">${node.node_id}</td>
      <td style="font-family:var(--font-mono);">${node.address}</td>
      <td>${region}</td>
      <td>${statusDot}</td>
      <td style="color:var(--text-secondary);">${lagLabel}</td>
      <td><span class="lag-badge ${node.lag_status}">${node.lag_status.toUpperCase()}</span></td>
    `;
    tbody.appendChild(tr);
  });

  logEvent('store', `Cluster health: ${data.online_count} online, ${data.offline_count} offline`);
}

function renderErasureCoding(data) {
  const diagram = document.getElementById('shard-diagram');
  if (!diagram) return;
  diagram.innerHTML = '';

  const dataShards   = data.data_shards   || 2;
  const parityShards = data.parity_shards || 1;

  for (let i = 0; i < dataShards; i++) {
    const tile = document.createElement('div');
    tile.className = 'shard-tile data';
    tile.textContent = `D${i + 1}`;
    tile.title = `Data shard ${i + 1}`;
    diagram.appendChild(tile);
  }
  for (let i = 0; i < parityShards; i++) {
    const tile = document.createElement('div');
    tile.className = 'shard-tile parity';
    tile.textContent = `P${i + 1}`;
    tile.title = `Parity shard ${i + 1}`;
    diagram.appendChild(tile);
  }

  document.getElementById('ec-mode').textContent   = data.erasure_coding ? 'Erasure Coding (Reed-Solomon)' : 'Replication';
  document.getElementById('ec-data').textContent   = dataShards;
  document.getElementById('ec-parity').textContent = parityShards;
  document.getElementById('ec-state').textContent  = data.cluster_healthy ? '✅ Healthy' : '⚠ Degraded';
  document.getElementById('ec-state').style.color  = data.cluster_healthy ? 'var(--success)' : 'var(--warning)';
}

// Wire refresh + rebalance buttons (called once after DOM ready)
document.addEventListener('DOMContentLoaded', () => {
  const refreshBtn = document.getElementById('btn-refresh-cluster');
  if (refreshBtn) refreshBtn.addEventListener('click', fetchClusterHealth);

  const rebalanceBtn = document.getElementById('btn-trigger-rebalance');
  if (rebalanceBtn) {
    rebalanceBtn.addEventListener('click', async () => {
      const statusEl = document.getElementById('rebalance-status');
      statusEl.className = 'status-message';
      statusEl.textContent = '⚡ Initiating gossip rebalance round...';
      rebalanceBtn.disabled = true;

      try {
        const res = await fetch('/api/cluster/rebalance', { method: 'POST' });
        if (res.ok) {
          statusEl.className = 'status-message success';
          statusEl.textContent = '✓ Rebalance gossip round triggered. Nodes will sync within ~3s.';
          logEvent('store', 'Cluster rebalance gossip round triggered successfully.');
          setTimeout(fetchClusterHealth, 3500);
        } else {
          const err = await res.text();
          statusEl.className = 'status-message error';
          statusEl.textContent = `Failed: ${err}`;
        }
      } catch (err) {
        statusEl.className = 'status-message error';
        statusEl.textContent = `Network error: ${err.message}`;
      } finally {
        rebalanceBtn.disabled = false;
      }
    });
  }
});

// --- OIDC SSO and Audit Logs Frontend Hooks ---

let SSO_ENABLED = false;

async function checkAuthConfig() {
  try {
    const res = await fetch('/api/auth/config');
    const config = await res.json();
    SSO_ENABLED = config.sso_enabled;
    
    if (SSO_ENABLED) {
      // Check current user name from cookies (stored in client profile status / info endpoint)
      // For local simplicity, we query a simple check endpoint or extract from headers
      const statusRes = await fetch('/api/status');
      if (statusRes.status === 401) {
        showLoginScreen();
      } else {
        const username = getCookieUsername();
        displayUserProfile(username || "SSO User");
      }
    }
  } catch (err) {
    console.error("Auth config check failed:", err);
  }
}

function showLoginScreen() {
  document.getElementById('login-screen').style.display = 'flex';
  // Disable main app UI container pointer events
  const container = document.querySelector('.app-container');
  if (container) {
    container.style.opacity = '0.15';
    container.style.pointerEvents = 'none';
  }
}

function displayUserProfile(username) {
  const profileSection = document.getElementById('user-profile-section');
  const userText = document.getElementById('logged-in-username');
  if (profileSection && userText) {
    userText.textContent = username;
    profileSection.style.display = 'flex';
  }
}

function getCookieUsername() {
  const cookie = document.cookie.split('; ').find(row => row.startsWith('token='));
  if (!cookie) return null;
  const token = cookie.split('=')[1];
  try {
    const payload = JSON.parse(atob(token.split('.')[1]));
    return payload.username;
  } catch (e) {
    return null;
  }
}

async function fetchAuditLogs() {
  try {
    const res = await fetch('/api/audit-logs');
    if (res.status === 401) {
      showLoginScreen();
      return;
    }
    const logs = await res.json();
    
    const tbody = document.querySelector('#audit-table tbody');
    tbody.innerHTML = '';
    
    if (logs.length === 0) {
      tbody.innerHTML = `<tr><td colspan="6" class="text-center text-muted">No security audit logs recorded yet.</td></tr>`;
      return;
    }
    
    logs.forEach(log => {
      const tr = document.createElement('tr');
      const timeStr = new Date(log.timestamp).toLocaleTimeString();
      const dateStr = new Date(log.timestamp).toLocaleDateString();
      const statusClass = log.status >= 200 && log.status < 300 ? 'success' : (log.status >= 400 ? 'error' : 'warning');
      const methodClass = log.method.toLowerCase();
      
      tr.innerHTML = `
        <td><span style="color:var(--text-muted);">${dateStr} ${timeStr}</span></td>
        <td><strong style="color:var(--primary);">${escapeHtml(log.user)}</strong></td>
        <td><strong>${escapeHtml(log.action)}</strong></td>
        <td><span class="method-badge ${methodClass}">${escapeHtml(log.method)}</span></td>
        <td><span style="font-family:var(--font-mono); font-size:0.8rem;">${escapeHtml(log.path)}</span></td>
        <td><span class="status-badge ${statusClass}">${log.status}</span></td>
      `;
      tbody.appendChild(tr);
    });
  } catch (err) {
    console.error("Failed to fetch audit logs:", err);
  }
}

function initAuditLogsUI() {
  const refreshAuditBtn = document.getElementById('btn-refresh-audit');
  if (refreshAuditBtn) {
    refreshAuditBtn.addEventListener('click', fetchAuditLogs);
  }
  
  const ssoLoginBtn = document.getElementById('btn-sso-login');
  if (ssoLoginBtn) {
    ssoLoginBtn.addEventListener('click', () => {
      window.location.href = '/api/auth/login';
    });
  }
  
  const logoutBtn = document.getElementById('btn-logout');
  if (logoutBtn) {
    logoutBtn.addEventListener('click', async () => {
      try {
        const res = await fetch('/api/auth/logout', { method: 'POST' });
        if (res.ok) {
          window.location.reload();
        }
      } catch (err) {
        console.error("Logout failed:", err);
      }
    });
  }
}

function escapeHtml(str) {
  if (!str) return '';
  return str.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#039;");
}

// ─── Database Schema ORM Viewer ────────────────────────────────────────────

async function fetchDatabaseSchemas() {
  const serviceSelect = document.getElementById('db-service-select');
  const noSchema = document.getElementById('db-no-schema');
  const schemaVisual = document.getElementById('db-schema-visual');

  try {
    const res = await fetch('/api/proxy/store/console/schema');
    if (!res.ok) {
      noSchema.style.display = 'block';
      noSchema.innerHTML = `
        <p style="font-size:2rem; margin-bottom:1rem;">⚠️</p>
        <p>Could not reach ServStore schema endpoint.</p>
        <p style="font-size:0.8rem; color:var(--text-muted); margin-top:0.5rem;">Make sure ServStore is online and the schema API is available.</p>
      `;
      schemaVisual.style.display = 'none';
      return;
    }

    const schemas = await res.json();
    const serviceNames = Object.keys(schemas);

    // Populate service dropdown
    serviceSelect.innerHTML = '<option value="">Select Service...</option>';
    serviceNames.forEach(name => {
      const opt = document.createElement('option');
      opt.value = name;
      opt.textContent = name;
      serviceSelect.appendChild(opt);
    });

    // Store schemas in state
    STATE.dbSchemas = schemas;

    if (serviceNames.length === 0) {
      noSchema.style.display = 'block';
      noSchema.innerHTML = `
        <p style="font-size:2rem; margin-bottom:1rem;">📂</p>
        <p>No database schemas registered yet.</p>
        <p style="font-size:0.8rem; color:var(--text-muted); margin-top:0.5rem;">Deploy a Serv-lang service with ORM tables to see them here.</p>
      `;
      schemaVisual.style.display = 'none';
    } else if (serviceNames.length === 1) {
      // Auto-select the only service
      serviceSelect.value = serviceNames[0];
      renderDatabaseSchema(serviceNames[0]);
    } else {
      noSchema.style.display = 'block';
      noSchema.innerHTML = `
        <p style="font-size:2rem; margin-bottom:1rem;">🗄️</p>
        <p><strong>${serviceNames.length}</strong> services have registered schemas.</p>
        <p style="font-size:0.8rem; color:var(--text-muted); margin-top:0.5rem;">Select a service from the dropdown above to visualize its database schema.</p>
      `;
      schemaVisual.style.display = 'none';
    }

    logEvent('store', `Loaded ${serviceNames.length} database schema(s): ${serviceNames.join(', ') || 'none'}`);
  } catch (err) {
    noSchema.style.display = 'block';
    noSchema.innerHTML = `
      <p style="font-size:2rem; margin-bottom:1rem;">❌</p>
      <p>Failed to fetch schemas: ${escapeHtml(err.message)}</p>
    `;
    schemaVisual.style.display = 'none';
    logEvent('error', `Database schema fetch failed: ${err.message}`);
  }
}

function renderDatabaseSchema(serviceName) {
  const noSchema = document.getElementById('db-no-schema');
  const schemaVisual = document.getElementById('db-schema-visual');

  const schema = STATE.dbSchemas?.[serviceName];
  if (!schema) {
    noSchema.style.display = 'block';
    noSchema.innerHTML = `
      <p style="font-size:2rem; margin-bottom:1rem;">❓</p>
      <p>No schema found for service: <strong>${escapeHtml(serviceName)}</strong></p>
    `;
    schemaVisual.style.display = 'none';
    return;
  }

  noSchema.style.display = 'none';
  schemaVisual.style.display = 'grid';
  schemaVisual.innerHTML = '';

  // Schema can be an array of tables or an object with tables key
  let tables = [];
  if (Array.isArray(schema)) {
    tables = schema;
  } else if (schema.tables && Array.isArray(schema.tables)) {
    tables = schema.tables;
  } else if (typeof schema === 'object') {
    // Try treating each key as a table name
    Object.keys(schema).forEach(key => {
      if (typeof schema[key] === 'object' && schema[key] !== null) {
        tables.push({ name: key, ...schema[key] });
      }
    });
  }

  if (tables.length === 0) {
    noSchema.style.display = 'block';
    noSchema.innerHTML = `
      <p style="font-size:2rem; margin-bottom:1rem;">📋</p>
      <p>Schema registered but contains no tables.</p>
    `;
    schemaVisual.style.display = 'none';
    return;
  }

  // Color palette for table headers
  const TABLE_COLORS = [
    'linear-gradient(135deg, hsl(250, 80%, 55%), hsl(280, 70%, 45%))',
    'linear-gradient(135deg, hsl(190, 80%, 45%), hsl(210, 70%, 40%))',
    'linear-gradient(135deg, hsl(145, 70%, 40%), hsl(170, 60%, 35%))',
    'linear-gradient(135deg, hsl(35, 85%, 50%), hsl(20, 75%, 45%))',
    'linear-gradient(135deg, hsl(340, 75%, 50%), hsl(355, 65%, 45%))',
    'linear-gradient(135deg, hsl(50, 80%, 50%), hsl(40, 70%, 40%))',
  ];

  tables.forEach((table, idx) => {
    const card = document.createElement('div');
    card.className = 'db-table-card';

    const tableName = table.name || table.Name || `Table_${idx + 1}`;
    const columns = table.columns || table.Columns || table.fields || table.Fields || [];
    const headerColor = TABLE_COLORS[idx % TABLE_COLORS.length];

    card.innerHTML = `
      <div class="db-table-header" style="background:${headerColor};">
        <div class="db-table-icon">🗃️</div>
        <div class="db-table-name">${escapeHtml(tableName)}</div>
        <div class="db-table-count">${columns.length} col${columns.length !== 1 ? 's' : ''}</div>
      </div>
      <div class="db-table-columns">
        ${columns.length === 0
          ? '<div class="db-col-row empty">No columns defined</div>'
          : columns.map(col => renderColumnRow(col)).join('')
        }
      </div>
    `;

    schemaVisual.appendChild(card);
  });

  logEvent('store', `Rendered ${tables.length} table(s) for service: ${serviceName}`);
}

function renderColumnRow(col) {
  const name = col.name || col.Name || col.field || col.Field || '?';
  const type = col.type || col.Type || col.data_type || col.DataType || 'unknown';
  const isPK = col.primary_key || col.PrimaryKey || col.pk || col.is_primary || false;
  const isFK = col.foreign_key || col.ForeignKey || col.fk || col.references || false;
  const isNullable = col.nullable !== undefined ? col.nullable : (col.Nullable !== undefined ? col.Nullable : true);
  const isAutoInc = col.auto_increment || col.AutoIncrement || col.autoincrement || false;

  let badges = '';
  if (isPK) badges += '<span class="db-badge pk" title="Primary Key">PK</span>';
  if (isFK) badges += `<span class="db-badge fk" title="Foreign Key → ${escapeHtml(String(isFK))}">FK</span>`;
  if (isAutoInc) badges += '<span class="db-badge ai" title="Auto Increment">AI</span>';
  if (!isNullable) badges += '<span class="db-badge nn" title="Not Null">NN</span>';

  const typeClass = getTypeClass(type);

  return `
    <div class="db-col-row ${isPK ? 'primary' : ''}">
      <div class="db-col-name">
        ${isPK ? '<span class="db-key-icon">🔑</span>' : '<span class="db-col-dot"></span>'}
        ${escapeHtml(name)}
      </div>
      <div class="db-col-meta">
        ${badges}
        <span class="db-type-badge ${typeClass}">${escapeHtml(type)}</span>
      </div>
    </div>
  `;
}

function getTypeClass(type) {
  const t = (type || '').toLowerCase();
  if (t.includes('int') || t.includes('float') || t.includes('double') || t.includes('decimal') || t.includes('numeric') || t.includes('real')) return 'type-number';
  if (t.includes('varchar') || t.includes('text') || t.includes('char') || t.includes('string')) return 'type-string';
  if (t.includes('bool')) return 'type-bool';
  if (t.includes('time') || t.includes('date') || t.includes('timestamp')) return 'type-date';
  if (t.includes('blob') || t.includes('binary') || t.includes('bytes')) return 'type-binary';
  if (t.includes('json') || t.includes('jsonb')) return 'type-json';
  return 'type-other';
}

// Wire up Database tab UI controls
document.addEventListener('DOMContentLoaded', () => {
  const serviceSelect = document.getElementById('db-service-select');
  if (serviceSelect) {
    serviceSelect.addEventListener('change', () => {
      const selected = serviceSelect.value;
      if (selected) {
        renderDatabaseSchema(selected);
      } else {
        const noSchema = document.getElementById('db-no-schema');
        const schemaVisual = document.getElementById('db-schema-visual');
        noSchema.style.display = 'block';
        noSchema.innerHTML = `
          <p style="font-size:2rem; margin-bottom:1rem;">📂</p>
          <p>Select a service to load and visualize its database schema.</p>
        `;
        schemaVisual.style.display = 'none';
      }
    });
  }

  const refreshDbBtn = document.getElementById('btn-refresh-database');
  if (refreshDbBtn) {
    refreshDbBtn.addEventListener('click', fetchDatabaseSchemas);
  }
});
