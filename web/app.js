// ServConsole Frontend Controller

const STATE = {
  activeTab: 'gateways',
  components: {
    ServGate: { online: false, latency: 0, details: null },
    ServQueue: { online: false, latency: 0, details: null },
    ServStore: { online: false, latency: 0, details: null },
    ServTunnel: { online: false, latency: 0, details: null }
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
  initTheme();
  checkAuthConfig();
  initTabs();
  initForms();
  initPolling();
  initRingCanvas();
  initAuditLogsUI();
  initAlertsUI();
  initLogsUI();
  initMobileMenu();
  initEnvironmentSelector();
});

function initTheme() {
  const toggleBtn = document.getElementById('btn-theme-toggle');
  if (!toggleBtn) return;

  const currentTheme = localStorage.getItem('theme') || 'dark';
  document.documentElement.setAttribute('data-theme', currentTheme);
  toggleBtn.textContent = currentTheme === 'light' ? '🌙' : '☀️';

  toggleBtn.addEventListener('click', () => {
    const activeTheme = document.documentElement.getAttribute('data-theme') || 'dark';
    const newTheme = activeTheme === 'light' ? 'dark' : 'light';
    
    document.documentElement.setAttribute('data-theme', newTheme);
    localStorage.setItem('theme', newTheme);
    toggleBtn.textContent = newTheme === 'light' ? '🌙' : '☀️';
  });
}

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
      } else if (tabId === 'graph') {
        fetchDependencyGraph();
      } else if (tabId === 'audit') {
        fetchAuditLogs();
      } else if (tabId === 'database') {
        fetchDatabaseSchemas();
        fetchMigrations();
      } else if (tabId === 'policies') {
        loadPoliciesView();
      } else if (tabId === 'cost') {
        fetchCostEstimation();
      } else if (tabId === 'slo') {
        fetchSLOTargets();
      } else if (tabId === 'deployments') {
        fetchDeployments();
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
        fetchConnections();
      } else if (STATE.activeTab === 'queues') {
        refreshQueuesList();
      } else if (STATE.activeTab === 'slo') {
        fetchSLOTargets();
      } else if (STATE.activeTab === 'deployments') {
        fetchDeployments();
      }
    } catch (err) {
      logEvent('error', `Status polling failed: ${err.message}`);
    }
  };
  
  poll();
  setInterval(poll, 3000);

  // Poll alerts every 5 seconds
  if (typeof pollAlerts === 'function') {
    pollAlerts();
    setInterval(pollAlerts, 5000);
  }
}

function updateSummaryUI() {
  // Global Status
  const statuses = Object.values(STATE.components).map(c => c.online);
  const onlineCount = statuses.filter(Boolean).length;
  const statusDot = document.getElementById('global-status-dot');
  const statusText = document.getElementById('global-status-text');
  
  if (onlineCount === statuses.length) {
    statusDot.className = 'status-indicator online';
    statusText.textContent = 'Ecosystem Online';
  } else if (onlineCount > 0) {
    statusDot.className = 'status-indicator degraded';
    statusText.textContent = 'Degraded State Detected';
  } else {
    statusDot.className = 'status-indicator offline';
    statusText.textContent = 'Ecosystem Offline';
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

  // ServTunnel Card
  const tunnel = STATE.components.ServTunnel;
  const tunnelCard = document.getElementById('tunnel-summary-card');
  const tunnelActive = document.getElementById('tunnel-active');
  if (tunnelCard && tunnelActive) {
    if (tunnel.online) {
      tunnelCard.querySelector('.badge').className = 'badge online';
      tunnelCard.querySelector('.badge').textContent = 'ONLINE';
      tunnelActive.textContent = `${tunnel.latency} ms`;
    } else {
      tunnelCard.querySelector('.badge').className = 'badge offline';
      tunnelCard.querySelector('.badge').textContent = 'OFFLINE';
      tunnelActive.textContent = '— ms';
    }
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
    
    const role = STATE.user?.role || 'admin';
    const disabledAttr = role === 'admin' ? '' : 'disabled style="opacity:0.5; cursor:not-allowed;" title="Admin privilege required"';
    routes.forEach(route => {
      const tr = document.createElement('tr');
      tr.innerHTML = `
        <td><strong style="color:var(--primary); font-family:var(--font-mono);">${route.prefix}</strong></td>
        <td><span style="font-family:var(--font-mono);">${route.target || route.targets?.join(', ')}</span></td>
        <td>${route.rate_limit_rpm ? `${route.rate_limit_rpm} rpm` : '—'}</td>
        <td>${route.prompt_guard ? '✅ Active' : '—'}</td>
        <td>${route.semantic_cache ? '✅ Active' : '—'}</td>
        <td>${route.pii_redact ? '✅ Active' : '—'}</td>
        <td>
          <button class="btn btn-danger btn-sm" onclick="deleteRoute('${route.prefix}')" ${disabledAttr}>Delete</button>
        </td>
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
  const role = STATE.user?.role || 'admin';
  const adminDisabled = role === 'admin' ? '' : 'disabled style="opacity:0.5; cursor:not-allowed;" title="Admin privilege required"';
  const tr = document.createElement('tr');
  tr.innerHTML = `
    <td><strong>stomp://localhost:61613</strong> (Default STOMP)</td>
    <td>WASM Engine Loaded</td>
    <td>
      In: ${metrics.messages_published_total || 0} <br>
      WASM Execs: ${metrics.wasm_executions_total || 0}
    </td>
    <td>
      <button class="btn btn-secondary btn-sm" onclick="clearWasmTransform('default')" ${adminDisabled}>Reset Filters</button>
    </td>
  `;
  tbody.appendChild(tr);

  // Fetch topic administration data
  fetchTopicAdmin();
  fetchWAL();
  fetchDelayedMessages();
}

async function fetchTopicAdmin() {
  const tbody = document.querySelector('#topic-admin-table tbody');
  try {
    const res = await fetch('/api/proxy/queue/api/topics');
    if (!res.ok) {
      tbody.innerHTML = `<tr><td colspan="6" class="text-center text-muted">Unable to fetch topic list</td></tr>`;
      return;
    }
    const data = await res.json();
    const topics = data.topics || [];

    if (topics.length === 0) {
      tbody.innerHTML = `<tr><td colspan="6" class="text-center text-muted">No topics registered yet</td></tr>`;
      return;
    }

    tbody.innerHTML = '';
    const role = STATE.user?.role || 'admin';
    const isOp = role === 'admin' || role === 'operator';
    const isAdmin = role === 'admin';
    const opDisabled = isOp ? '' : 'disabled style="opacity:0.5; cursor:not-allowed;" title="Operator privilege required"';
    const adminDisabled = isAdmin ? '' : 'disabled style="opacity:0.5; cursor:not-allowed;" title="Admin privilege required"';

    topics.forEach(topic => {
      const tr = document.createElement('tr');
      tr.innerHTML = `
        <td><strong>${topic.name}</strong></td>
        <td>${topic.subscribers}</td>
        <td>${topic.partitions || 0}</td>
        <td>${topic.has_transform ? '<span class="badge online">Active</span>' : '<span class="text-muted">None</span>'}</td>
        <td>${topic.dlq_topic ? `<span class="badge">${topic.dlq_topic}</span>` : '<span class="text-muted">—</span>'}</td>
        <td>
          <button class="btn btn-secondary btn-sm" onclick="configureDLQ('${topic.name}')" ${opDisabled}>DLQ</button>
          <button class="btn btn-secondary btn-sm" onclick="clearWasmTransform('${topic.name}')" ${adminDisabled}>Clear WASM</button>
        </td>
      `;
      tbody.appendChild(tr);
    });
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="6" class="text-center text-muted">Error: ${err.message}</td></tr>`;
  }
}

function configureDLQ(topic) {
  const dlqTopic = prompt(`Enter DLQ topic name for "${topic}" (e.g. ${topic}.dlq):`);
  if (!dlqTopic) return;

  fetch(`/api/proxy/queue/api/topics/${topic}/dlq`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ dlq_topic: dlqTopic })
  }).then(res => {
    const status = document.getElementById('topic-admin-status');
    if (res.ok) {
      status.className = 'status-message success';
      status.textContent = `✓ DLQ "${dlqTopic}" configured for topic "${topic}"`;
      fetchTopicAdmin();
    } else {
      status.className = 'status-message error';
      status.textContent = `✗ Failed to configure DLQ`;
    }
  });
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
  document.getElementById('btn-refresh-graph').addEventListener('click', fetchDependencyGraph);
  document.getElementById('btn-clear-traces')?.addEventListener('click', clearTraces);
  document.getElementById('trace-search')?.addEventListener('input', filterTraces);
  document.getElementById('btn-clear-logs').addEventListener('click', () => {
    document.getElementById('console-logs-screen').innerHTML = '';
  });

  // Queue WAL & Delayed triggers
  document.getElementById('btn-refresh-wal').addEventListener('click', fetchWAL);
  document.getElementById('btn-refresh-delayed').addEventListener('click', fetchDelayedMessages);
}

async function fetchWAL() {
  const tbody = document.querySelector('#wal-table tbody');
  if (!tbody) return;
  try {
    const res = await fetch('/api/proxy/queue/api/stats');
    if (!res.ok) {
      tbody.innerHTML = `<tr><td colspan="4" class="text-center text-muted">Unable to fetch WAL</td></tr>`;
      return;
    }
    const data = await res.json();
    const entries = data.wal_entries || [];

    if (entries.length === 0) {
      tbody.innerHTML = `<tr><td colspan="4" class="text-center text-muted">No WAL entries recorded</td></tr>`;
      return;
    }

    tbody.innerHTML = '';
    entries.forEach(entry => {
      const tr = document.createElement('tr');
      const date = new Date(entry.Timestamp / 1000000);
      const timeStr = date.toLocaleTimeString() + '.' + String(entry.Timestamp % 1000000).padStart(6, '0').slice(0, 3);
      const preview = entry.Payload.length > 60 ? entry.Payload.slice(0, 60) + '...' : entry.Payload;
      tr.innerHTML = `
        <td style="font-family:var(--font-mono); font-size:0.85rem;">${timeStr}</td>
        <td><strong>${entry.Topic}</strong></td>
        <td>${entry.Payload.length} bytes</td>
        <td style="font-family:var(--font-mono); font-size:0.85rem;">${escapeHtml(preview)}</td>
      `;
      tbody.appendChild(tr);
    });
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="4" class="text-center text-muted">Error: ${err.message}</td></tr>`;
  }
}

async function fetchDelayedMessages() {
  const tbody = document.querySelector('#delayed-messages-table tbody');
  if (!tbody) return;
  try {
    const res = await fetch('/api/proxy/queue/api/stats');
    if (!res.ok) {
      tbody.innerHTML = `<tr><td colspan="4" class="text-center text-muted">Unable to fetch delayed messages</td></tr>`;
      return;
    }
    const data = await res.json();
    const msgs = data.delayed_messages || [];

    if (msgs.length === 0) {
      tbody.innerHTML = `<tr><td colspan="4" class="text-center text-muted">No delayed messages scheduled</td></tr>`;
      return;
    }

    tbody.innerHTML = '';
    msgs.forEach(msg => {
      const tr = document.createElement('tr');
      const targetTime = new Date(msg.target_time);
      const diffMs = Math.max(0, targetTime.getTime() - Date.now());
      const delayStr = (diffMs / 1000).toFixed(1) + 's';
      const preview = msg.payload.length > 40 ? msg.payload.slice(0, 40) + '...' : msg.payload;
      tr.innerHTML = `
        <td style="font-family:var(--font-mono); font-size:0.85rem;">${msg.id}</td>
        <td><strong>${msg.topic}</strong></td>
        <td><span class="badge" style="background-color: var(--warning); color: #000;">${delayStr}</span></td>
        <td style="font-family:var(--font-mono); font-size:0.85rem;">${escapeHtml(preview)}</td>
      `;
      tbody.appendChild(tr);
    });
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="4" class="text-center text-muted">Error: ${err.message}</td></tr>`;
  }
}

function escapeHtml(text) {
  if (!text) return '';
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
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
      <td><button class="btn btn-secondary btn-sm" onclick="switchToTracesTab('S3')">View Traces</button></td>
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
    
    // Fetch user and role from backend
    const meRes = await fetch('/api/auth/me');
    if (meRes.status === 401) {
      showLoginScreen();
      return;
    }
    if (meRes.ok) {
      const meData = await meRes.json();
      STATE.user = meData;
      displayUserProfile(meData.username, meData.role);
      applyRBACRules(meData.role);
    } else {
      // Fallback: decode cookie
      const decoded = getDecodedToken();
      if (decoded) {
        STATE.user = decoded;
        displayUserProfile(decoded.username, decoded.role);
        applyRBACRules(decoded.role);
      } else if (SSO_ENABLED) {
        showLoginScreen();
      } else {
        STATE.user = { username: 'anonymous', role: 'admin' };
        displayUserProfile('anonymous', 'admin');
        applyRBACRules('admin');
      }
    }
  } catch (err) {
    console.error("Auth config check failed:", err);
    // If auth is not configured at all, default to admin
    STATE.user = { username: 'anonymous', role: 'admin' };
    displayUserProfile('anonymous', 'admin');
    applyRBACRules('admin');
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

function displayUserProfile(username, role) {
  const profileSection = document.getElementById('user-profile-section');
  const userText = document.getElementById('logged-in-username');
  if (profileSection && userText) {
    userText.textContent = `${username} (${role.toUpperCase()})`;
    profileSection.style.display = 'flex';
  }
}

function getDecodedToken() {
  const cookie = document.cookie.split('; ').find(row => row.startsWith('token='));
  if (!cookie) return null;
  const token = cookie.split('=')[1];
  try {
    const payload = JSON.parse(atob(token.split('.')[1]));
    return {
      username: payload.username,
      role: payload.role || (payload.username === 'admin' ? 'admin' : (payload.username === 'operator' ? 'operator' : 'viewer'))
    };
  } catch (e) {
    return null;
  }
}

function applyRBACRules(role) {
  const isAdmin = role === 'admin';
  const isOperator = role === 'operator' || isAdmin;
  
  // 1. Admin-only UI Controls
  const adminSelectors = [
    '#btn-add-route',
    '#wasm-upload-form button[type="submit"]',
    '#queue-transform-form button[type="submit"]',
    '#btn-apply-migration',
    '#btn-save-policy',
    '#btn-delete-policy'
  ];
  
  // 2. Operator/Admin UI Controls
  const operatorSelectors = [
    '#btn-create-bucket',
    '#publish-message-form button[type="submit"]',
    '#btn-trigger-rebalance'
  ];

  adminSelectors.forEach(sel => {
    document.querySelectorAll(sel).forEach(el => {
      el.disabled = !isAdmin;
      if (!isAdmin) {
        el.title = "Admin privilege required";
        el.style.opacity = '0.5';
        el.style.cursor = 'not-allowed';
      } else {
        el.title = "";
        el.style.opacity = '';
        el.style.cursor = '';
      }
    });
  });

  operatorSelectors.forEach(sel => {
    document.querySelectorAll(sel).forEach(el => {
      el.disabled = !isOperator;
      if (!isOperator) {
        el.title = "Operator/Admin privilege required";
        el.style.opacity = '0.5';
        el.style.cursor = 'not-allowed';
      } else {
        el.title = "";
        el.style.opacity = '';
        el.style.cursor = '';
      }
    });
  });
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

  initQueryWorkbench();
});

// ─── SQL Query Workbench ───────────────────────────────────────────────────

function initQueryWorkbench() {
  const driverSelect = document.getElementById('wb-driver-select');
  const connStrInput = document.getElementById('wb-conn-str');
  const connHint = document.getElementById('wb-conn-hint');
  const queryText = document.getElementById('wb-query-text');
  const runQueryBtn = document.getElementById('btn-run-query');

  if (!driverSelect || !connStrInput || !queryText || !runQueryBtn) return;

  const dsnDefaults = {
    sqlite: {
      dsn: './dev.db',
      hint: 'Filepath for SQLite database (e.g. ./dev.db or C:/path/to/db.db)',
      query: 'SELECT name FROM sqlite_master WHERE type="table" AND name NOT LIKE "sqlite_%";'
    },
    postgres: {
      dsn: 'postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable',
      hint: 'PostgreSQL connection URL DSN.',
      query: 'SELECT table_name, table_schema FROM information_schema.tables WHERE table_schema = \'public\';'
    },
    mysql: {
      dsn: 'root:secret@tcp(localhost:3306)/mysql',
      hint: 'MySQL/MariaDB connection DSN (username:password@tcp(host:port)/dbname).',
      query: 'SHOW TABLES;'
    },
    oracle: {
      dsn: 'oracle://user:pass@localhost:1521/xe',
      hint: 'Oracle connection string DSN.',
      query: 'SELECT table_name FROM user_tables;'
    }
  };

  // Set initial default query
  queryText.value = dsnDefaults.sqlite.query;

  driverSelect.addEventListener('change', () => {
    const driver = driverSelect.value;
    const config = dsnDefaults[driver];
    if (config) {
      connStrInput.value = config.dsn;
      connHint.textContent = config.hint;
      queryText.value = config.query;
    }
  });

  runQueryBtn.addEventListener('click', runSqlQuery);

  // Ctrl+Enter shortcut in textarea
  queryText.addEventListener('keydown', (e) => {
    if (e.ctrlKey && e.key === 'Enter') {
      e.preventDefault();
      runSqlQuery();
    }
  });
}

async function runSqlQuery() {
  const driverSelect = document.getElementById('wb-driver-select');
  const connStrInput = document.getElementById('wb-conn-str');
  const queryText = document.getElementById('wb-query-text');
  const runQueryBtn = document.getElementById('btn-run-query');
  const resultsContainer = document.getElementById('wb-results-container');
  const errorAlert = document.getElementById('wb-error-alert');
  const successAlert = document.getElementById('wb-success-alert');
  const tableWrapper = document.getElementById('wb-table-wrapper');
  const resultsTable = document.getElementById('wb-results-table');
  const resultsStats = document.getElementById('wb-results-stats');

  if (!driverSelect || !connStrInput || !queryText || !runQueryBtn) return;

  const driver = driverSelect.value;
  const connStr = connStrInput.value.trim();
  const query = queryText.value.trim();

  if (!connStr) {
    alert('Please enter a connection string / DSN.');
    return;
  }
  if (!query) {
    alert('Please enter an SQL query to execute.');
    return;
  }

  // Reset display
  resultsContainer.style.display = 'block';
  errorAlert.style.display = 'none';
  successAlert.style.display = 'none';
  tableWrapper.style.display = 'none';
  resultsStats.textContent = 'Executing...';
  runQueryBtn.disabled = true;
  runQueryBtn.textContent = 'Running...';

  try {
    const res = await fetch('/api/db/query', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ driver, connStr, query })
    });

    if (res.status === 401) {
      showLoginScreen();
      return;
    }

    if (!res.ok) {
      const errMsg = await res.text();
      throw new Error(errMsg || `HTTP Error ${res.status}`);
    }

    const data = await res.json();
    if (!data.success) {
      errorAlert.style.display = 'block';
      errorAlert.textContent = data.error || 'Unknown query execution error.';
      resultsStats.textContent = 'Failed';
      return;
    }

    resultsStats.textContent = `Executed in ${data.executionTimeMs}ms`;

    if (data.isSelect) {
      successAlert.style.display = 'block';
      successAlert.textContent = `Success: Query returned ${data.rows ? data.rows.length : 0} rows.`;
      
      // Render Table
      tableWrapper.style.display = 'block';
      
      // Headers
      const thead = resultsTable.querySelector('thead');
      thead.innerHTML = '';
      const trHead = document.createElement('tr');
      data.columns.forEach(col => {
        const th = document.createElement('th');
        th.textContent = col;
        trHead.appendChild(th);
      });
      thead.appendChild(trHead);

      // Rows
      const tbody = resultsTable.querySelector('tbody');
      tbody.innerHTML = '';
      if (!data.rows || data.rows.length === 0) {
        const trEmpty = document.createElement('tr');
        trEmpty.innerHTML = `<td colspan="${data.columns.length}" class="text-center text-muted" style="padding: 2rem;">No rows returned.</td>`;
        tbody.appendChild(trEmpty);
      } else {
        data.rows.forEach(row => {
          const tr = document.createElement('tr');
          row.forEach(cell => {
            const td = document.createElement('td');
            td.textContent = cell !== null ? String(cell) : 'NULL';
            if (cell === null) td.style.fontStyle = 'italic';
            tr.appendChild(td);
          });
          tbody.appendChild(tr);
        });
      }
    } else {
      successAlert.style.display = 'block';
      successAlert.textContent = `Success: Query executed. Rows Affected: ${data.rowsAffected !== undefined ? data.rowsAffected : 0}. Last Insert ID: ${data.lastInsertId !== undefined ? data.lastInsertId : '—'}.`;
    }
  } catch (err) {
    errorAlert.style.display = 'block';
    errorAlert.textContent = err.message;
    resultsStats.textContent = 'Error';
  } finally {
    runQueryBtn.disabled = false;
    runQueryBtn.textContent = 'Run Query';
  }
}

// --- Cross-Service Dependency Graph (Item 6) ---
let graphNodes = [];
let graphEdges = [];
let draggedNode = null;
let selectedNode = null;
let graphAnimationId = null;
let flowParticles = [];

async function fetchDependencyGraph() {
  const canvas = document.getElementById('graph-canvas');
  if (!canvas) return;
  const ctx = canvas.getContext('2d');
  
  // Render loading state
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  ctx.font = '16px Outfit';
  ctx.fillStyle = 'rgba(255, 255, 255, 0.5)';
  ctx.textAlign = 'center';
  ctx.fillText('Auto-discovering service topology...', canvas.width / 2, canvas.height / 2);
  
  // Setup mouse events once
  if (!canvas.dataset.eventsAttached) {
    canvas.dataset.eventsAttached = 'true';
    
    canvas.addEventListener('mousedown', (e) => {
      const rect = canvas.getBoundingClientRect();
      const mouseX = e.clientX - rect.left;
      const mouseY = e.clientY - rect.top;
      
      draggedNode = null;
      selectedNode = null;
      
      for (const node of graphNodes) {
        const dx = mouseX - node.x;
        const dy = mouseY - node.y;
        if (Math.sqrt(dx * dx + dy * dy) <= 45) {
          draggedNode = node;
          selectedNode = node;
          logEvent('console', `Selected topology node: ${node.id}`);
          break;
        }
      }
    });
    
    canvas.addEventListener('mousemove', (e) => {
      if (!draggedNode) return;
      const rect = canvas.getBoundingClientRect();
      draggedNode.x = Math.max(45, Math.min(canvas.width - 45, e.clientX - rect.left));
      draggedNode.y = Math.max(45, Math.min(canvas.height - 45, e.clientY - rect.top));
    });
    
    canvas.addEventListener('mouseup', () => { draggedNode = null; });
    canvas.addEventListener('mouseleave', () => { draggedNode = null; });
  }
  
  try {
    const res = await fetch('/api/topology');
    if (!res.ok) {
      loadMockGraphData();
      startGraphRenderLoop(ctx, canvas);
      return;
    }
    const data = await res.json();
    if (!data.nodes || data.nodes.length === 0) {
      loadMockGraphData();
      startGraphRenderLoop(ctx, canvas);
      return;
    }

    const currentNodesMap = new Map(graphNodes.map(n => [n.id, n]));
    graphNodes = data.nodes.map((node, index) => {
      const existing = currentNodesMap.get(node.id);
      if (existing) {
        return { ...node, x: existing.x, y: existing.y };
      }
      let x = 400;
      let y = 250;
      if (node.id === 'ServGate') { x = 150; y = 250; }
      else if (node.id === 'ServStore') { x = 650; y = 150; }
      else if (node.id === 'ServQueue') { x = 650; y = 350; }
      else {
        x = 400;
        y = 150 + (index * 90);
      }
      return { ...node, x, y };
    });

    graphEdges = data.edges || [];
    startGraphRenderLoop(ctx, canvas);
  } catch (err) {
    loadMockGraphData();
    startGraphRenderLoop(ctx, canvas);
  }
}

// --- OpenTelemetry Distributed Tracing (Observability Link) ---
let loadedTraces = [];

async function fetchTraces() {
  const tbody = document.querySelector('#traces-table tbody');
  if (!tbody) return;
  try {
    tbody.innerHTML = `<tr><td colspan="7" class="text-center text-muted">Loading traces...</td></tr>`;
    const res = await fetch('/api/proxy/trace/api/traces');
    if (!res.ok) {
      tbody.innerHTML = `<tr><td colspan="7" class="text-center text-muted">Failed to load traces from ServTrace</td></tr>`;
      return;
    }
    loadedTraces = await res.json();
    // Sort descending by timestamp
    loadedTraces.sort((a, b) => b.timestampUnixNano - a.timestampUnixNano);
    renderTraceList(loadedTraces);
    logEvent('trace', `Loaded ${loadedTraces.length} traces`);
  } catch (err) {
    tbody.innerHTML = `<tr><td colspan="7" class="text-center text-muted">Error: ${err.message}</td></tr>`;
    logEvent('error', `Failed to fetch traces: ${err.message}`);
  }
}

function renderTraceList(traces) {
  const tbody = document.querySelector('#traces-table tbody');
  if (!tbody) return;
  tbody.innerHTML = '';
  
  if (traces.length === 0) {
    tbody.innerHTML = `<tr><td colspan="7" class="text-center text-muted">No traces found in ServTrace</td></tr>`;
    return;
  }
  
  traces.forEach(t => {
    const tr = document.createElement('tr');
    tr.className = 'trace-item-row';
    tr.dataset.traceId = t.traceId;
    
    // Formatting timestamp
    const date = new Date(t.timestampUnixNano / 1000000);
    const timeStr = date.toLocaleTimeString() + '.' + String(t.timestampUnixNano % 1000000).padStart(6, '0').slice(0, 3);
    
    const statusClass = t.errorCount > 0 ? 'badge offline' : 'badge online';
    const statusLabel = t.errorCount > 0 ? `${t.errorCount} ERR` : 'OK';
    const serviceClass = `waterfall-span-service service-${t.service.toLowerCase()}`;
    
    tr.innerHTML = `
      <td style="font-family:var(--font-mono); font-size:0.8rem;">${timeStr}</td>
      <td style="font-family:var(--font-mono); font-size:0.8rem; max-width: 100px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;" title="${t.traceId}">${t.traceId}</td>
      <td><strong>${escapeHtml(t.rootName)}</strong></td>
      <td><span class="${serviceClass}">${escapeHtml(t.service)}</span></td>
      <td style="font-family:var(--font-mono);">${t.durationMs.toFixed(2)} ms</td>
      <td>${t.totalSpans}</td>
      <td><span class="${statusClass}">${statusLabel}</span></td>
    `;
    
    tr.addEventListener('click', () => {
      document.querySelectorAll('.trace-item-row').forEach(r => r.classList.remove('active'));
      tr.classList.add('active');
      showTraceDetail(t.traceId);
    });
    
    tbody.appendChild(tr);
  });
}

function filterTraces() {
  const query = document.getElementById('trace-search').value.toLowerCase();
  const filtered = loadedTraces.filter(t => 
    t.traceId.toLowerCase().includes(query) || 
    t.rootName.toLowerCase().includes(query) || 
    t.service.toLowerCase().includes(query)
  );
  renderTraceList(filtered);
}

async function clearTraces() {
  if (!confirm('Are you sure you want to clear all trace records from ServTrace memory?')) return;
  try {
    const res = await fetch('/api/proxy/trace/api/traces/', { method: 'DELETE' });
    if (res.ok) {
      logEvent('trace', 'Traces cleared successfully');
      fetchTraces();
      document.getElementById('traces-timeline').innerHTML = `
        <div class="text-center text-muted" style="padding: 4rem 0;">Select a trace from the logs on the left to view the distributed spans execution tree.</div>
      `;
      document.getElementById('waterfall-trace-id-badge').textContent = 'No Trace Selected';
    } else {
      alert('Failed to clear traces');
    }
  } catch (err) {
    logEvent('error', `Failed to clear traces: ${err.message}`);
  }
}

async function showTraceDetail(traceId) {
  const timeline = document.getElementById('traces-timeline');
  const badge = document.getElementById('waterfall-trace-id-badge');
  const replayBtn = document.getElementById('btn-replay-trace');
  if (!timeline) return;
  
  if (replayBtn) replayBtn.style.display = 'none';
  badge.textContent = `TRACE ID: ${traceId.slice(0, 8)}...`;
  timeline.innerHTML = `<div class="text-center text-muted" style="padding: 4rem 0;">Loading trace tree...</div>`;
  
  try {
    const res = await fetch(`/api/proxy/trace/api/traces/${traceId}`);
    if (!res.ok) {
      timeline.innerHTML = `<div class="text-center text-muted">Failed to load trace detail</div>`;
      return;
    }
    const rootNode = await res.json();
    timeline.innerHTML = '';
    
    // We want to find the max duration of the trace to compute bar widths relative to total duration
    const totalDuration = findMaxDuration(rootNode);
    renderSpanNodeWaterfall(timeline, rootNode, totalDuration, 0);

    STATE.selectedTraceId = traceId;
    if (replayBtn) replayBtn.style.display = 'inline-block';
  } catch (err) {
    timeline.innerHTML = `<div class="text-center text-muted">Error loading tree: ${err.message}</div>`;
  }
}

function findMaxDuration(node) {
  let max = node.offsetMs + node.durationMs;
  if (node.children) {
    node.children.forEach(c => {
      const childMax = findMaxDuration(c);
      if (childMax > max) max = childMax;
    });
  }
  return max;
}

function renderSpanNodeWaterfall(container, node, totalDuration, depth) {
  if (!node) return;
  
  const row = document.createElement('div');
  row.className = 'waterfall-span-row';
  
  const service = (node.span.service || 'unknown').toLowerCase();
  const serviceClass = `waterfall-span-service service-${service}`;
  const barClass = `waterfall-bar waterfall-bar-${service} ${node.span.status === 2 ? 'error' : 'success'}`;
  
  // Calculate width and offset %
  const pctWidth = totalDuration > 0 ? (node.durationMs / totalDuration) * 100 : 100;
  const pctOffset = totalDuration > 0 ? (node.offsetMs / totalDuration) * 100 : 0;
  
  const indent = depth * 15;
  const hasAttributes = node.span.attributes && Object.keys(node.span.attributes).length > 0;
  const uniqueId = `attr-${node.span.spanId}`;
  
  row.innerHTML = `
    <div class="waterfall-span-main">
      <div class="waterfall-span-info" style="padding-left: ${indent}px;">
        <span class="${serviceClass}">${escapeHtml(node.span.service || 'unknown')}</span>
        <span class="waterfall-span-name" title="${escapeHtml(node.span.name)}">${escapeHtml(node.span.name)}</span>
        ${hasAttributes ? `<span style="font-size:0.75rem; cursor:pointer; color:var(--primary);" onclick="toggleSpanAttr('${uniqueId}')">ⓘ</span>` : ''}
      </div>
      <div class="waterfall-timeline-track">
        <div class="${barClass}" style="left: ${pctOffset}%; width: ${pctWidth}%;"></div>
        <div class="waterfall-duration-label" style="left: ${pctOffset + pctWidth}%;">${node.durationMs.toFixed(1)}ms</div>
      </div>
    </div>
    ${hasAttributes ? `
      <div class="waterfall-span-details" id="${uniqueId}" style="display: none;">
        <div class="attributes-grid">
          ${Object.entries(node.span.attributes).map(([k, v]) => `
            <div class="attr-key">${escapeHtml(k)}</div>
            <div class="attr-val">${escapeHtml(String(v))}</div>
          `).join('')}
        </div>
      </div>
    ` : ''}
  `;
  
  container.appendChild(row);
  
  if (node.children) {
    // Sort children by start offset
    node.children.sort((a, b) => a.offsetMs - b.offsetMs);
    node.children.forEach(child => {
      renderSpanNodeWaterfall(container, child, totalDuration, depth + 1);
    });
  }
}

function toggleSpanAttr(id) {
  const el = document.getElementById(id);
  if (el) {
    el.style.display = el.style.display === 'none' ? 'block' : 'none';
  }
}
window.toggleSpanAttr = toggleSpanAttr;

function switchToTracesTab(filterQuery) {
  const tracesBtn = document.querySelector('.tab-btn[data-tab="traces"]');
  if (tracesBtn) {
    tracesBtn.click();
    if (filterQuery) {
      const searchInput = document.getElementById('trace-search');
      if (searchInput) {
        searchInput.value = filterQuery;
        filterTraces();
      }
    }
  }
}
window.switchToTracesTab = switchToTracesTab;

function loadMockGraphData() {
  graphNodes = [
    { id: 'ServGate', label: 'ServGate', x: 150, y: 250, color: '#06b6d4' },
    { id: 'OrderService', label: 'OrderService', x: 400, y: 150, color: '#a855f7' },
    { id: 'BillingService', label: 'BillingService', x: 400, y: 350, color: '#a855f7' },
    { id: 'ServStore', label: 'ServStore', x: 650, y: 150, color: '#10b981' },
    { id: 'ServQueue', label: 'ServQueue', x: 650, y: 350, color: '#f59e0b' }
  ];
  
  graphEdges = [
    { from: 'ServGate', to: 'OrderService', label: 'HTTP' },
    { from: 'ServGate', to: 'BillingService', label: 'HTTP' },
    { from: 'OrderService', to: 'ServStore', label: 'PUT' },
    { from: 'BillingService', to: 'ServQueue', label: 'STOMP' }
  ];
}

function startGraphRenderLoop(ctx, canvas) {
  if (graphAnimationId) {
    cancelAnimationFrame(graphAnimationId);
  }
  
  flowParticles = [];
  
  function updateAndDraw() {
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    
    // Draw connection edges
    graphEdges.forEach(edge => {
      const fromNode = graphNodes.find(n => n.id === edge.from);
      const toNode = graphNodes.find(n => n.id === edge.to);
      if (fromNode && toNode) {
        drawArrow(ctx, fromNode.x, fromNode.y, toNode.x, toNode.y, '#475569', edge.label);
      }
    });
    
    // Periodically spawn particles on random edges
    if (Math.random() < 0.03 && graphEdges.length > 0) {
      const randomEdge = graphEdges[Math.floor(Math.random() * graphEdges.length)];
      flowParticles.push({
        edge: randomEdge,
        progress: 0.0,
        speed: 0.01 + Math.random() * 0.015
      });
    }
    
    // Update and draw flow particles
    flowParticles.forEach((p, idx) => {
      p.progress += p.speed;
      if (p.progress >= 1.0) {
        flowParticles.splice(idx, 1);
        return;
      }
      
      const fromNode = graphNodes.find(n => n.id === p.edge.from);
      const toNode = graphNodes.find(n => n.id === p.edge.to);
      if (fromNode && toNode) {
        const angle = Math.atan2(toNode.y - fromNode.y, toNode.x - fromNode.x);
        const startX = fromNode.x + 45 * Math.cos(angle);
        const startY = fromNode.y + 45 * Math.sin(angle);
        const endX = toNode.x - 45 * Math.cos(angle);
        const endY = toNode.y - 45 * Math.sin(angle);
        
        const px = startX + (endX - startX) * p.progress;
        const py = startY + (endY - startY) * p.progress;
        
        ctx.beginPath();
        ctx.arc(px, py, 4, 0, 2 * Math.PI);
        ctx.fillStyle = '#6366f1';
        ctx.shadowBlur = 8;
        ctx.shadowColor = '#6366f1';
        ctx.fill();
        ctx.shadowBlur = 0;
      }
    });
    
    // Draw nodes
    graphNodes.forEach(node => {
      ctx.beginPath();
      ctx.arc(node.x, node.y, 45, 0, 2 * Math.PI);
      ctx.fillStyle = 'rgba(15, 17, 32, 0.85)';
      ctx.strokeStyle = node.color;
      ctx.lineWidth = selectedNode === node ? 5 : 3;
      ctx.shadowBlur = selectedNode === node ? 18 : 10;
      ctx.shadowColor = node.color;
      ctx.fill();
      ctx.stroke();
      ctx.shadowBlur = 0;
      
      ctx.font = 'bold 12px Outfit';
      ctx.fillStyle = '#ffffff';
      ctx.textAlign = 'center';
      ctx.fillText(node.id, node.x, node.y + 4);
    });
    
    graphAnimationId = requestAnimationFrame(updateAndDraw);
  }
  
  updateAndDraw();
}

function drawArrow(ctx, fromX, fromY, toX, toY, color, label) {
  const headlen = 12; 
  const angle = Math.atan2(toY - fromY, toX - fromX);
  
  const startX = fromX + 45 * Math.cos(angle);
  const startY = fromY + 45 * Math.sin(angle);
  const endX = toX - 45 * Math.cos(angle);
  const endY = toY - 45 * Math.sin(angle);
  
  ctx.beginPath();
  ctx.moveTo(startX, startY);
  ctx.lineTo(endX, endY);
  ctx.strokeStyle = color;
  ctx.lineWidth = 2;
  ctx.stroke();
  
  ctx.beginPath();
  ctx.moveTo(endX, endY);
  ctx.lineTo(endX - headlen * Math.cos(angle - Math.PI / 6), endY - headlen * Math.sin(angle - Math.PI / 6));
  ctx.lineTo(endX - headlen * Math.cos(angle + Math.PI / 6), endY - headlen * Math.sin(angle + Math.PI / 6));
  ctx.fillStyle = color;
  ctx.fill();
  
  if (label) {
    const midX = (startX + endX) / 2;
    const midY = (startY + endY) / 2;
    ctx.font = '10px JetBrains Mono';
    ctx.fillStyle = '#94a3b8';
    ctx.textAlign = 'center';
    ctx.fillText(label, midX, midY - 8);
  }
}

// --- Access Control / Policy Editor Logic ---
let defaultUsers = ['admin', 'developer-bob', 'anonymous'];
let selectedUser = '';

function getStorageUsers() {
  const stored = localStorage.getItem('console_users');
  if (stored) {
    try {
      return JSON.parse(stored);
    } catch (e) {
      return defaultUsers;
    }
  }
  localStorage.setItem('console_users', JSON.stringify(defaultUsers));
  return defaultUsers;
}

function saveStorageUser(username) {
  const users = getStorageUsers();
  if (!users.includes(username)) {
    users.push(username);
    localStorage.setItem('console_users', JSON.stringify(users));
  }
}

function loadPoliciesView() {
  const users = getStorageUsers();
  const listContainer = document.getElementById('policy-users-list');
  if (!listContainer) return;
  listContainer.innerHTML = '';

  users.forEach(u => {
    const item = document.createElement('div');
    item.className = `user-item ${selectedUser === u ? 'active' : ''}`;
    item.style.padding = '10px 12px';
    item.style.borderRadius = '8px';
    item.style.cursor = 'pointer';
    item.style.color = selectedUser === u ? 'var(--text-primary)' : 'var(--text-secondary)';
    item.style.backgroundColor = selectedUser === u ? 'rgba(99, 102, 241, 0.15)' : 'transparent';
    item.style.borderLeft = selectedUser === u ? '3px solid var(--primary)' : 'none';
    item.style.marginBottom = '4px';
    item.style.transition = 'all 0.2s ease';
    item.style.fontWeight = '500';

    item.innerHTML = `👤 ${u}`;
    item.addEventListener('click', () => selectUserPolicy(u));
    listContainer.appendChild(item);
  });

  // Wire once helper buttons and selection
  if (!window.policiesWired) {
    window.policiesWired = true;

    document.getElementById('btn-select-user').addEventListener('click', () => {
      const usernameInput = document.getElementById('new-user-name');
      const username = usernameInput.value.trim();
      if (!username) {
        alert('Username cannot be empty');
        return;
      }
      saveStorageUser(username);
      usernameInput.value = '';
      selectUserPolicy(username);
    });

    document.getElementById('btn-save-policy').addEventListener('click', async () => {
      if (!selectedUser) return;
      const jsonArea = document.getElementById('policy-json-area');
      const rawJson = jsonArea.value.trim();

      try {
        JSON.parse(rawJson);
      } catch (e) {
        alert('Invalid JSON format: ' + e.message);
        return;
      }

      try {
        const res = await fetch(`/api/proxy/store/console/users/${selectedUser}/policy`, {
          method: 'PUT',
          body: rawJson,
          headers: { 'Content-Type': 'application/json' }
        });
        if (res.ok) {
          logEvent('store', `Saved IAM policy for user: ${selectedUser}`);
          alert(`Policy for "${selectedUser}" saved successfully!`);
        } else {
          const text = await res.text();
          alert('Failed to save policy: ' + text);
        }
      } catch (err) {
        alert('Network Error: ' + err.message);
      }
    });

    document.getElementById('btn-delete-policy').addEventListener('click', async () => {
      if (!selectedUser) return;
      if (!confirm(`Are you sure you want to delete policy for user "${selectedUser}"?`)) return;

      try {
        const res = await fetch(`/api/proxy/store/console/users/${selectedUser}/policy`, {
          method: 'DELETE'
        });
        if (res.ok) {
          logEvent('store', `Deleted IAM policy for user: ${selectedUser}`);
          alert(`Policy for "${selectedUser}" deleted`);
          selectUserPolicy(selectedUser);
        } else {
          alert('Failed to delete policy');
        }
      } catch (err) {
        alert('Network Error: ' + err.message);
      }
    });

    // Quick Templates
    document.getElementById('tpl-s3-allow').addEventListener('click', () => {
      const jsonArea = document.getElementById('policy-json-area');
      jsonArea.value = JSON.stringify({
        Version: "2012-10-17",
        Statement: [
          {
            Effect: "Allow",
            Action: ["s3:GetObject", "s3:PutObject"],
            Resource: ["arn:aws:s3:::*"]
          }
        ]
      }, null, 2);
    });

    document.getElementById('tpl-stomp-allow').addEventListener('click', () => {
      const jsonArea = document.getElementById('policy-json-area');
      jsonArea.value = JSON.stringify({
        Version: "2012-10-17",
        Statement: [
          {
            Effect: "Allow",
            Action: ["stomp:Publish", "stomp:Subscribe"],
            Resource: ["arn:aws:stomp:::topic/*"]
          }
        ]
      }, null, 2);
    });

    document.getElementById('tpl-full-access').addEventListener('click', () => {
      const jsonArea = document.getElementById('policy-json-area');
      jsonArea.value = JSON.stringify({
        Version: "2012-10-17",
        Statement: [
          {
            Effect: "Allow",
            Action: ["s3:*", "stomp:*"],
            Resource: ["*"]
          }
        ]
      }, null, 2);
    });
  }
}

async function selectUserPolicy(username) {
  selectedUser = username;
  loadPoliciesView();

  const titleEl = document.getElementById('policy-current-user');
  const jsonArea = document.getElementById('policy-json-area');
  const btnSave = document.getElementById('btn-save-policy');
  const btnDelete = document.getElementById('btn-delete-policy');

  if (titleEl) titleEl.textContent = username;
  if (jsonArea) jsonArea.value = '';
  if (btnSave) btnSave.disabled = true;
  if (btnDelete) btnDelete.disabled = true;

  try {
    const res = await fetch(`/api/proxy/store/console/users/${username}/policy`);
    if (res.status === 404) {
      jsonArea.value = JSON.stringify({
        Version: "2012-10-17",
        Statement: []
      }, null, 2);
    } else if (!res.ok) {
      jsonArea.value = "Failed to load policy";
    } else {
      const data = await res.json();
      jsonArea.value = JSON.stringify(data, null, 2);
    }
    const isAdmin = STATE.user?.role === 'admin';
    if (btnSave) btnSave.disabled = !isAdmin;
    if (btnDelete) btnDelete.disabled = !isAdmin;
  } catch (err) {
    if (jsonArea) jsonArea.value = "Error: " + err.message;
  }
}

// ─── Migration Auditing ────────────────────────────────────────────────────

async function fetchMigrations() {
  try {
    const res = await fetch('/api/db/migrations');
    if (res.status === 401) {
      showLoginScreen();
      return;
    }
    if (!res.ok) return;
    const data = await res.json();
    STATE.migrations = data || [];
    renderMigrations();
  } catch (err) {
    console.error('Failed to fetch migrations:', err);
  }
}

function renderMigrations() {
  const tbody = document.querySelector('#migrations-table tbody');
  if (!tbody) return;
  tbody.innerHTML = '';

  const migrations = STATE.migrations || [];
  if (migrations.length === 0) {
    tbody.innerHTML = `<tr><td colspan="5" class="text-center text-muted">No migrations applied yet. Use the form to apply your first schema revision.</td></tr>`;
    return;
  }

  migrations.forEach(mig => {
    const tr = document.createElement('tr');
    const timeStr = new Date(mig.timestamp).toLocaleString();
    const statusClass = mig.status === 'success' ? 'mig-success' : 'mig-failed';
    const statusIcon = mig.status === 'success' ? '✓' : '✗';
    const statusLabel = mig.status === 'success' ? 'Success' : 'Failed';
    const deltaHtml = mig.delta ? escapeHtml(mig.delta) : '—';
    const errorTip = mig.error ? ` title="${escapeHtml(mig.error)}"` : '';

    tr.innerHTML = `
      <td><span class="mig-revision-badge">${escapeHtml(mig.revision)}</span></td>
      <td>
        <div style="font-weight:500;">${escapeHtml(mig.description || '—')}</div>
        <div style="font-size:0.7rem; color:var(--text-muted); margin-top:0.15rem;">
          ${escapeHtml(mig.driver)} · ${escapeHtml(mig.user)} · ${mig.duration_ms}ms
        </div>
      </td>
      <td><span class="mig-status-badge ${statusClass}"${errorTip}>${statusIcon} ${statusLabel}</span></td>
      <td style="font-family:var(--font-mono); font-size:0.78rem; max-width:200px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;" title="${escapeHtml(mig.delta || '')}">${deltaHtml}</td>
      <td style="font-size:0.78rem; color:var(--text-muted); white-space:nowrap;">${timeStr}</td>
    `;
    tbody.appendChild(tr);
  });

  logEvent('store', `Loaded ${migrations.length} migration audit entries`);
}

async function applyMigration() {
  const driverEl = document.getElementById('mig-driver');
  const dsnEl = document.getElementById('mig-dsn');
  const revisionEl = document.getElementById('mig-revision');
  const descEl = document.getElementById('mig-description');
  const sqlEl = document.getElementById('mig-sql');
  const statusEl = document.getElementById('migration-status');
  const btnEl = document.getElementById('btn-apply-migration');

  if (!driverEl || !dsnEl || !revisionEl || !sqlEl) return;

  const driver = driverEl.value;
  const dsn = dsnEl.value.trim();
  const revision = revisionEl.value.trim();
  const description = descEl.value.trim();
  const sql = sqlEl.value.trim();

  if (!dsn || !revision || !sql) {
    statusEl.className = 'status-message error';
    statusEl.textContent = 'Please fill in Driver, DSN, Revision ID, and SQL script.';
    return;
  }

  statusEl.className = 'status-message';
  statusEl.textContent = '⚡ Applying migration...';
  btnEl.disabled = true;
  btnEl.textContent = 'Applying...';

  try {
    const res = await fetch('/api/db/migrations', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ driver, dsn, revision, description, sql })
    });

    if (res.status === 401) {
      showLoginScreen();
      return;
    }

    const data = await res.json();

    if (data.status === 'success') {
      statusEl.className = 'status-message success';
      statusEl.textContent = `✓ Migration ${data.revision} applied successfully (${data.duration_ms}ms). Delta: ${data.delta}`;
      logEvent('store', `Migration applied: rev ${data.revision} — ${data.delta}`);
      // Reset form fields (except driver & DSN for convenience)
      revisionEl.value = '';
      descEl.value = '';
      sqlEl.value = '';
    } else {
      statusEl.className = 'status-message error';
      statusEl.textContent = `✗ Migration ${data.revision} failed: ${data.error}`;
      logEvent('error', `Migration failed: rev ${data.revision} — ${data.error}`);
    }

    // Refresh the revisions table
    fetchMigrations();
  } catch (err) {
    statusEl.className = 'status-message error';
    statusEl.textContent = `Network error: ${err.message}`;
  } finally {
    btnEl.disabled = false;
    btnEl.textContent = '⚡ Apply Migration';
  }
}

// Wire up migration UI on DOM ready
document.addEventListener('DOMContentLoaded', () => {
  const migForm = document.getElementById('migration-form');
  if (migForm) {
    migForm.addEventListener('submit', (e) => {
      e.preventDefault();
      applyMigration();
    });
  }

  const refreshMigBtn = document.getElementById('btn-refresh-migrations');
  if (refreshMigBtn) {
    refreshMigBtn.addEventListener('click', fetchMigrations);
  }
});

// --- Gateways Connection Stats & Route Deletion ---
async function fetchConnections() {
  try {
    const res = await fetch('/api/proxy/gate/api/admin/connections');
    if (!res.ok) return;
    const conns = await res.json();
    const tbody = document.querySelector('#connections-table tbody');
    if (!tbody) return;
    tbody.innerHTML = '';
    
    const keys = Object.keys(conns);
    if (keys.length === 0) {
      tbody.innerHTML = `<tr><td colspan="3" class="text-center text-muted">No active connections recorded</td></tr>`;
      return;
    }
    
    keys.forEach(target => {
      const count = conns[target];
      const tr = document.createElement('tr');
      
      let loadBadge = '<span class="badge online">LOW</span>';
      if (count > 50) {
        loadBadge = '<span class="badge offline">HIGH</span>';
      } else if (count > 10) {
        loadBadge = '<span class="badge degraded">MODERATE</span>';
      }
      
      tr.innerHTML = `
        <td><span style="font-family:var(--font-mono);">${target}</span></td>
        <td><strong>${count}</strong></td>
        <td>${loadBadge}</td>
      `;
      tbody.appendChild(tr);
    });
  } catch (err) {
    // silent fallback
  }
}

async function deleteRoute(prefix) {
  if (!confirm(`Are you sure you want to delete route "${prefix}"?`)) {
    return;
  }
  try {
    const res = await fetch(`/api/routes?prefix=${encodeURIComponent(prefix)}`, {
      method: 'DELETE'
    });
    if (res.ok) {
      logEvent('system', `Deleted route prefix: ${prefix}`);
      refreshRoutesList();
      fetchConnections();
    } else {
      const errData = await res.json();
      logEvent('error', `Failed to delete route: ${errData.error || res.statusText}`);
    }
  } catch (err) {
    logEvent('error', `Failed to delete route: ${err.message}`);
  }
}

// Bind manual refresh connection button
document.addEventListener('DOMContentLoaded', () => {
  setTimeout(() => {
    document.getElementById('btn-refresh-connections')?.addEventListener('click', fetchConnections);
  }, 1000);

  // Wire up summary card click events
  document.getElementById('gate-summary-card')?.addEventListener('click', () => {
    document.querySelector('.tab-btn[data-tab="gateways"]')?.click();
  });
  document.getElementById('queue-summary-card')?.addEventListener('click', () => {
    document.querySelector('.tab-btn[data-tab="queues"]')?.click();
  });
  document.getElementById('store-summary-card')?.addEventListener('click', () => {
    document.querySelector('.tab-btn[data-tab="storage"]')?.click();
  });
});

// Global navigation helpers
window.switchToTracesTab = function(filterText) {
  const tracesTabBtn = document.querySelector('.tab-btn[data-tab="traces"]');
  if (tracesTabBtn) {
    tracesTabBtn.click();
  }
  setTimeout(() => {
    const searchInput = document.getElementById('trace-search');
    if (searchInput) {
      searchInput.value = filterText;
      searchInput.dispatchEvent(new Event('input'));
    }
  }, 50);
};

// Alerts & Replay Logic
function initAlertsUI() {
  const alertsToggle = document.getElementById('btn-alerts-toggle');
  const alertsPanel = document.getElementById('alerts-dropdown-panel');
  const dismissAllBtn = document.getElementById('btn-alerts-clear');

  if (alertsToggle && alertsPanel) {
    alertsToggle.addEventListener('click', (e) => {
      e.stopPropagation();
      alertsPanel.style.display = alertsPanel.style.display === 'none' ? 'block' : 'none';
    });

    document.addEventListener('click', (e) => {
      if (!alertsPanel.contains(e.target) && e.target !== alertsToggle) {
        alertsPanel.style.display = 'none';
      }
    });
  }

  if (dismissAllBtn) {
    dismissAllBtn.addEventListener('click', async () => {
      const activeAlerts = STATE.alerts || [];
      const unackAlerts = activeAlerts.filter(a => !a.acknowledged);
      for (const alert of unackAlerts) {
        await ackAlert(alert.id);
      }
      pollAlerts();
    });
  }

  const replayBtn = document.getElementById('btn-replay-trace');
  if (replayBtn) {
    replayBtn.addEventListener('click', replaySelectedTrace);
  }
}

async function pollAlerts() {
  try {
    const res = await fetch('/api/alerts');
    if (!res.ok) return;
    const alertsList = await res.json();
    STATE.alerts = alertsList;
    renderAlertsDropdown(alertsList);
  } catch (err) {
    console.error('Failed to poll alerts:', err);
  }
}

function renderAlertsDropdown(alertsList) {
  const badge = document.getElementById('alerts-count-badge');
  const container = document.getElementById('alerts-list-container');
  if (!badge || !container) return;

  const unackAlerts = alertsList.filter(a => !a.acknowledged);
  
  if (unackAlerts.length > 0) {
    badge.textContent = unackAlerts.length;
    badge.style.display = 'flex';
  } else {
    badge.style.display = 'none';
  }

  container.innerHTML = '';
  if (alertsList.length === 0) {
    container.innerHTML = `<div class="text-center text-muted" style="padding: 1rem 0; font-size: 0.85rem;">No active alerts</div>`;
    return;
  }

  alertsList.forEach(alert => {
    const item = document.createElement('div');
    item.className = `alert-item ${alert.severity} ${alert.acknowledged ? 'acknowledged' : ''}`;
    item.style.cursor = 'pointer';
    if (alert.acknowledged) {
      item.style.opacity = '0.5';
    }

    const time = new Date(alert.timestamp).toLocaleTimeString();
    
    item.innerHTML = `
      <div class="alert-item-header">
        <span>${alert.component}</span>
        <span class="alert-item-time">${time}</span>
      </div>
      <div style="font-size: 0.85rem; color: #e2e8f0; margin-bottom: 0.25rem;">${alert.message}</div>
      <div style="display:flex; justify-content:space-between; align-items:center; margin-top:0.25rem;">
        <span style="font-size: 0.7rem; color: var(--primary); text-decoration: underline;">Investigate Timeline</span>
        ${!alert.acknowledged ? `
          <button class="btn btn-secondary btn-sm" style="font-size: 0.7rem; padding: 0.1rem 0.3rem;" onclick="event.stopPropagation(); ackAlert('${alert.id}')">Acknowledge</button>
        ` : ''}
      </div>
    `;
    item.addEventListener('click', () => {
      openIncidentTimeline(alert.id);
    });
    container.appendChild(item);
  });
}

async function ackAlert(id) {
  try {
    const res = await fetch('/api/alerts/ack', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id })
    });
    if (res.ok) {
      logEvent('system', `Alert ${id} acknowledged`);
      pollAlerts();
    }
  } catch (err) {
    console.error('Failed to ack alert:', err);
  }
}
window.ackAlert = ackAlert;

async function replaySelectedTrace() {
  const traceId = STATE.selectedTraceId;
  if (!traceId) return;

  const replayBtn = document.getElementById('btn-replay-trace');
  if (replayBtn) {
    replayBtn.disabled = true;
    replayBtn.textContent = '⚡ Replaying...';
  }

  logEvent('trace', `Replaying request from trace: ${traceId}`);

  try {
    const res = await fetch('/api/traces/replay', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ traceId })
    });

    if (res.ok) {
      const data = await res.json();
      if (data.success) {
        logEvent('trace', `✓ Replay Succeeded! Code: ${data.statusCode}. Response: ${data.body.slice(0, 100)}`);
      } else {
        logEvent('error', `✗ Replay Failed! Code: ${data.statusCode}. Response: ${data.body.slice(0, 100)}`);
      }
    } else {
      const errText = await res.text();
      logEvent('error', `✗ Replay Failed: ${errText}`);
    }
  } catch (err) {
    logEvent('error', `✗ Replay Network Error: ${err.message}`);
  } finally {
    if (replayBtn) {
      replayBtn.disabled = false;
      replayBtn.textContent = '⚡ Replay Request';
    }
  }
}

// Logs UI & Cost Optimization Logic
function initLogsUI() {
  const svcFilter = document.getElementById('log-filter-service');
  const lvlFilter = document.getElementById('log-filter-level');
  const searchInput = document.getElementById('log-search-text');

  if (svcFilter) svcFilter.addEventListener('change', fetchLogs);
  if (lvlFilter) lvlFilter.addEventListener('change', fetchLogs);
  if (searchInput) {
    let debounce;
    searchInput.addEventListener('input', () => {
      clearTimeout(debounce);
      debounce = setTimeout(fetchLogs, 300);
    });
  }

  setInterval(fetchLogs, 4000);
  fetchLogs();
}

async function fetchLogs() {
  const svc = document.getElementById('log-filter-service')?.value || '';
  const lvl = document.getElementById('log-filter-level')?.value || '';
  const search = document.getElementById('log-search-text')?.value || '';

  try {
    const url = `/api/logs?service=${encodeURIComponent(svc)}&level=${encodeURIComponent(lvl)}&search=${encodeURIComponent(search)}`;
    const res = await fetch(url);
    if (!res.ok) return;
    const logs = await res.json();
    
    const consoleScreen = document.getElementById('console-logs-screen');
    if (!consoleScreen) return;
    
    consoleScreen.innerHTML = '';
    
    if (logs.length === 0) {
      consoleScreen.innerHTML = '<div class="text-muted text-center" style="padding: 1rem 0;">No matching logs found</div>';
      return;
    }
    
    logs.forEach(log => {
      const line = document.createElement('div');
      const service = (log.service || 'unknown').toLowerCase();
      line.className = `log-line ${service} ${log.level || 'info'}`;
      
      const time = new Date(log.timestamp).toLocaleTimeString();
      const traceBadge = log.traceId ? ` <span style="font-family:var(--font-mono); font-size:0.7rem; opacity:0.6; cursor:pointer;" onclick="switchToTracesTab('${log.traceId}')">[trace:${log.traceId.slice(0,6)}]</span>` : '';
      
      line.innerHTML = `[${time}] <span class="tag">[${(log.service || 'unknown').toUpperCase()}]</span> <span class="level-tag">[${(log.level || 'info').toUpperCase()}]</span> ${escapeHtml(log.message)}${traceBadge}`;
      consoleScreen.appendChild(line);
    });
    
    consoleScreen.scrollTop = consoleScreen.scrollHeight;
  } catch (err) {
    console.error('Failed to fetch logs:', err);
  }
}

async function fetchCostEstimation() {
  const estTotal = document.getElementById('cost-estimated-total');
  const limitLabel = document.getElementById('cost-budget-limit');
  const percentLabel = document.getElementById('cost-budget-percent');
  const progressBar = document.getElementById('cost-budget-progress');
  const badge = document.getElementById('cost-budget-badge');
  const breakdownList = document.getElementById('cost-breakdown-list');
  const recContainer = document.getElementById('cost-recommendations-container');

  if (!estTotal) return;

  try {
    const res = await fetch('/api/cost-estimation');
    if (!res.ok) return;
    const data = await res.json();

    estTotal.textContent = `$${data.monthly.total.toFixed(2)}`;
    limitLabel.textContent = `$${data.monthly.budget.toFixed(2)}`;
    percentLabel.textContent = `${data.monthly.percent.toFixed(1)}%`;
    progressBar.style.width = `${Math.min(100, data.monthly.percent)}%`;

    if (data.monthly.total > data.monthly.budget) {
      badge.className = 'badge offline';
      badge.textContent = 'Budget Exceeded';
      progressBar.style.backgroundColor = 'var(--danger, #ef4444)';
    } else {
      badge.className = 'badge online';
      badge.textContent = 'Within Budget';
      progressBar.style.backgroundColor = 'var(--primary)';
    }

    breakdownList.innerHTML = '';
    data.breakdown.forEach(item => {
      const row = document.createElement('div');
      row.style.display = 'flex';
      row.style.justifyContent = 'space-between';
      row.style.alignItems = 'center';
      row.style.padding = '0.5rem';
      row.style.background = 'rgba(255,255,255,0.02)';
      row.style.borderRadius = '4px';

      row.innerHTML = `
        <div style="display:flex; align-items:center; gap:0.5rem;">
          <span style="display:inline-block; width:12px; height:12px; border-radius:50%; background-color:${item.color};"></span>
          <span>${item.name}</span>
        </div>
        <strong>$${item.value.toFixed(2)}</strong>
      `;
      breakdownList.appendChild(row);
    });

    recContainer.innerHTML = '';
    data.recommendations.forEach(rec => {
      const item = document.createElement('div');
      item.style.padding = '0.75rem';
      item.style.background = 'rgba(255, 193, 7, 0.05)';
      item.style.borderLeft = '3px solid var(--warning, #f59e0b)';
      item.style.borderRadius = '4px';
      item.style.fontSize = '0.85rem';
      item.style.color = '#e2e8f0';
      item.textContent = rec;
      recContainer.appendChild(item);
    });

  } catch (err) {
    console.error('Failed to fetch cost estimation:', err);
  }
}

async function fetchSLOTargets() {
  const container = document.getElementById('slo-indicators-container');
  const statusEl = document.getElementById('slo-overall-status');
  const recsContainer = document.getElementById('slo-recs-container');
  if (!container) return;

  try {
    const res = await fetch('/api/slo');
    if (!res.ok) throw new Error('API returned error status');
    const data = await res.json();

    let breachedCount = 0;
    let warningCount = 0;

    container.innerHTML = '';
    data.forEach(slo => {
      let statusColor = 'var(--success)';
      let statusText = 'HEALTHY';
      let progressColor = 'var(--success)';
      
      if (slo.status === 'breached') {
        statusColor = 'var(--danger, #ef4444)';
        statusText = 'BREACHED';
        progressColor = 'var(--danger, #ef4444)';
        breachedCount++;
      } else if (slo.status === 'warning') {
        statusColor = 'var(--warning, #f59e0b)';
        statusText = 'WARNING';
        progressColor = 'var(--warning, #f59e0b)';
        warningCount++;
      }

      const card = document.createElement('div');
      card.style.background = 'rgba(255, 255, 255, 0.02)';
      card.style.border = '1px solid rgba(255, 255, 255, 0.05)';
      card.style.borderRadius = '8px';
      card.style.padding = '1.25rem';
      card.style.display = 'flex';
      card.style.flexDirection = 'column';
      card.style.gap = '1rem';

      card.innerHTML = `
        <div style="display:flex; justify-content:space-between; align-items:center;">
          <div>
            <h3 style="margin:0; font-size:1rem; color:#fff;">${slo.name}</h3>
            <span style="font-size:0.75rem; color:var(--text-secondary); font-family:var(--font-mono);">${slo.serviceId}</span>
          </div>
          <span class="badge" style="background:${statusColor}15; color:${statusColor}; border:1px solid ${statusColor}30;">${statusText}</span>
        </div>

        <div style="display:grid; grid-template-columns: repeat(3, 1fr); gap:1rem; font-size:0.85rem; border-top: 1px solid rgba(255,255,255,0.03); padding-top:0.75rem;">
          <div>
            <div style="color:var(--text-muted); font-size:0.75rem; margin-bottom:0.25rem;">Target Compliance</div>
            <strong>${slo.targetPercent.toFixed(2)}%</strong>
          </div>
          <div>
            <div style="color:var(--text-muted); font-size:0.75rem; margin-bottom:0.25rem;">Actual Compliance</div>
            <strong style="color:${statusColor}">${slo.actualPercent.toFixed(2)}%</strong>
          </div>
          <div>
            <div style="color:var(--text-muted); font-size:0.75rem; margin-bottom:0.25rem;">Latency (Actual / Target)</div>
            <strong>${slo.actualLatencyMs}ms / ${slo.targetLatencyMs}ms</strong>
          </div>
        </div>

        <div>
          <div style="display:flex; justify-content:space-between; font-size:0.75rem; margin-bottom:0.4rem;">
            <span style="color:var(--text-secondary);">Error Budget Remaining</span>
            <span style="font-weight:600; color:${progressColor};">${slo.budgetRemainingPercent.toFixed(1)}%</span>
          </div>
          <div style="background:rgba(255,255,255,0.05); height:8px; border-radius:4px; overflow:hidden;">
            <div style="background:${progressColor}; width:${slo.budgetRemainingPercent}%; height:100%; transition:width 0.5s ease;"></div>
          </div>
          <div style="display:flex; justify-content:space-between; font-size:0.7rem; color:var(--text-muted); margin-top:0.3rem;">
            <span>Burn Rate: ${slo.burnRate.toFixed(1)}x</span>
            <span>Est. Exhaustion: ${slo.burnRate > 1.0 ? Math.round(30 * (slo.budgetRemainingPercent / 100) / slo.burnRate) : '30+'} days</span>
          </div>
        </div>
      `;
      container.appendChild(card);
    });

    // Update summary details
    if (breachedCount > 0) {
      statusEl.textContent = `${breachedCount} SLO(s) Breached`;
      statusEl.style.color = 'var(--danger, #ef4444)';
    } else if (warningCount > 0) {
      statusEl.textContent = `${warningCount} SLO(s) in Warning State`;
      statusEl.style.color = 'var(--warning, #f59e0b)';
    } else {
      statusEl.textContent = '100% Operational & Healthy';
      statusEl.style.color = 'var(--success)';
    }

    recsContainer.innerHTML = '';
    let adviceList = [];
    data.forEach(slo => {
      if (slo.status === 'breached') {
        adviceList.push(`[CRITICAL] ${slo.serviceId} is exhausting its error budget rapidly! Uptime is below the ${slo.targetPercent}% target threshold. Investigate immediately.`);
      } else if (slo.status === 'warning') {
        adviceList.push(`[WARNING] Latency on ${slo.serviceId} is exceeding the targeted ${slo.targetLatencyMs}ms. Burn rate is currently ${slo.burnRate.toFixed(1)}x.`);
      }
    });

    if (adviceList.length === 0) {
      adviceList.push("No immediate action required. All system metrics comply with targets.");
      adviceList.push("Protip: Keep an eye on consistent storage shards distribution to maintain sub-100ms read latency.");
    }

    adviceList.forEach(adv => {
      const card = document.createElement('div');
      card.style.padding = '0.75rem';
      card.style.borderRadius = '4px';
      card.style.fontSize = '0.8rem';
      if (adv.startsWith('[CRITICAL]')) {
        card.style.background = 'rgba(239, 68, 68, 0.05)';
        card.style.borderLeft = '3px solid var(--danger, #ef4444)';
        card.style.color = '#fca5a5';
      } else if (adv.startsWith('[WARNING]')) {
        card.style.background = 'rgba(245, 158, 11, 0.05)';
        card.style.borderLeft = '3px solid var(--warning, #f59e0b)';
        card.style.color = '#fef3c7';
      } else {
        card.style.background = 'rgba(255, 255, 255, 0.02)';
        card.style.borderLeft = '3px solid var(--primary)';
        card.style.color = 'var(--text-secondary)';
      }
      card.textContent = adv;
      recsContainer.appendChild(card);
    });

  } catch (err) {
    console.error('Failed to fetch SLO targets:', err);
  }
}

function initMobileMenu() {
  const menuBtn = document.getElementById('mobile-menu-btn');
  const navTabs = document.getElementById('main-nav-tabs');
  if (!menuBtn || !navTabs) return;

  menuBtn.addEventListener('click', () => {
    navTabs.classList.toggle('mobile-active');
  });

  // Close nav menu on tab clicks on mobile
  navTabs.querySelectorAll('.tab-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      navTabs.classList.remove('mobile-active');
    });
  });
}

async function initEnvironmentSelector() {
  const selector = document.getElementById('env-selector');
  if (!selector) return;

  try {
    const res = await fetch('/api/environments');
    if (!res.ok) throw new Error();
    const data = await res.json();

    selector.innerHTML = '';
    data.environments.forEach(env => {
      const opt = document.createElement('option');
      opt.value = env.id;
      opt.textContent = env.name;
      opt.selected = env.id === data.active;
      selector.appendChild(opt);
    });

    updateEnvironmentTheme(data.active);

    selector.addEventListener('change', async () => {
      const selectedEnv = selector.value;
      try {
        const postRes = await fetch('/api/environments/select', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ environmentId: selectedEnv })
        });
        if (postRes.ok) {
          logEvent('system', `Switched environment to: ${selectedEnv}`);
          updateEnvironmentTheme(selectedEnv);
          // Refresh cost metrics if active tab is cost
          if (STATE.activeTab === 'cost') {
            fetchCostEstimation();
          } else if (STATE.activeTab === 'deployments') {
            fetchDeployments();
          }
        }
      } catch (err) {
        console.error('Failed to change environment:', err);
      }
    });

  } catch (err) {
    console.error('Failed to initialize environment selector:', err);
  }
}

function updateEnvironmentTheme(envId) {
  const displayEl = document.getElementById('active-env-display');
  if (displayEl) {
    displayEl.textContent = envId.charAt(0).toUpperCase() + envId.slice(1);
  }

  // Set visual theme dot color based on active env
  const wrapper = document.querySelector('.env-selector-wrapper');
  if (wrapper) {
    let dotColor = '#06b6d4'; // default dev
    if (envId === 'staging') dotColor = '#f59e0b';
    else if (envId === 'production') dotColor = '#a855f7';
    wrapper.style.borderColor = dotColor + '40';
    wrapper.style.boxShadow = `0 0 8px ${dotColor}20`;
  }
}

async function fetchDeployments() {
  const listContainer = document.getElementById('deployments-list');
  const activeVerEl = document.getElementById('active-deployment-version');
  const activeAuthorEl = document.getElementById('active-env-author');
  const activeTimeEl = document.getElementById('active-env-time');
  if (!listContainer) return;

  try {
    const res = await fetch('/api/deployments');
    if (!res.ok) throw new Error();
    const data = await res.json();

    listContainer.innerHTML = '';
    data.forEach(dep => {
      const tr = document.createElement('tr');
      tr.style.borderBottom = '1px solid rgba(255, 255, 255, 0.03)';
      tr.style.transition = 'background-color 0.2s';
      
      let statusBadge = `<span class="badge" style="background:rgba(148, 163, 184, 0.1); color:#cbd5e1;">Historical</span>`;
      let actionBtn = `<button class="btn btn-secondary btn-sm" onclick="triggerRollback('${dep.id}')" style="font-size:0.75rem; padding:0.2rem 0.5rem;">Rollback</button>`;
      
      if (dep.status === 'active') {
        statusBadge = `<span class="badge online" style="background:rgba(16, 185, 129, 0.1); color:var(--success);">Active</span>`;
        actionBtn = `<span style="font-size:0.75rem; color:var(--text-muted);">Current</span>`;
        
        if (activeVerEl) activeVerEl.textContent = dep.version;
        if (activeAuthorEl) activeAuthorEl.textContent = dep.author;
        if (activeTimeEl) activeTimeEl.textContent = new Date(dep.timestamp).toLocaleTimeString();
      } else if (dep.status === 'rolled_back') {
        statusBadge = `<span class="badge offline" style="background:rgba(239, 68, 68, 0.1); color:var(--danger);">Rolled Back</span>`;
      }

      tr.innerHTML = `
        <td style="padding: 0.75rem; font-weight:600; color:#fff;">${dep.version}</td>
        <td style="padding: 0.75rem; color:var(--text-secondary);">${new Date(dep.timestamp).toLocaleTimeString()}</td>
        <td style="padding: 0.75rem; font-family:var(--font-mono);">${dep.author}</td>
        <td style="padding: 0.75rem; color:var(--text-secondary); max-width:300px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">${dep.changelog}</td>
        <td style="padding: 0.75rem;">${statusBadge}</td>
        <td style="padding: 0.75rem; text-align: right;">${actionBtn}</td>
      `;
      listContainer.appendChild(tr);
    });

  } catch (err) {
    console.error('Failed to fetch deployments:', err);
  }
}

async function triggerRollback(targetId) {
  if (!confirm('Are you sure you want to rollback to this deployment version? This will compile and swap the active binary.')) {
    return;
  }
  
  try {
    const res = await fetch('/api/deployments/rollback', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ targetId })
    });
    if (res.ok) {
      logEvent('system', `Successfully triggered rollback to ${targetId}`);
      fetchDeployments();
    } else {
      alert('Rollback failed. Verify authorization scopes.');
    }
  } catch (err) {
    console.error('Error during rollback:', err);
  }
}

async function openIncidentTimeline(alertId) {
  const modal = document.getElementById('incident-timeline-modal');
  const closeBtn = document.getElementById('incident-modal-close-btn');
  const eventsContainer = document.getElementById('incident-timeline-events');
  const titleEl = document.getElementById('incident-modal-title');
  const compEl = document.getElementById('incident-modal-component');
  const sevEl = document.getElementById('incident-modal-severity');

  if (!modal || !eventsContainer) return;

  eventsContainer.innerHTML = '<div class="text-center text-muted" style="padding: 2rem;">Analyzing logs & compiling timeline events...</div>';
  modal.style.display = 'flex';

  const closeModal = () => { modal.style.display = 'none'; };
  closeBtn.onclick = closeModal;
  modal.onclick = (e) => { if (e.target === modal) closeModal(); };

  try {
    const res = await fetch(`/api/incidents/analyze?alertId=${alertId}`);
    if (!res.ok) throw new Error();
    const data = await res.json();

    titleEl.textContent = data.title;
    compEl.textContent = data.component;
    
    sevEl.textContent = data.severity.toUpperCase();
    if (data.severity === 'critical') {
      sevEl.className = 'badge offline';
      sevEl.style.background = 'rgba(239,68,68,0.1)';
      sevEl.style.color = 'var(--danger)';
    } else {
      sevEl.className = 'badge warning';
      sevEl.style.background = 'rgba(245,158,11,0.1)';
      sevEl.style.color = 'var(--warning)';
    }

    eventsContainer.innerHTML = '';
    data.events.forEach(evt => {
      const card = document.createElement('div');
      card.className = 'timeline-event-card';
      card.style.position = 'relative';
      card.style.background = 'rgba(255,255,255,0.02)';
      card.style.border = '1px solid rgba(255,255,255,0.05)';
      card.style.borderRadius = '8px';
      card.style.padding = '1rem';
      card.style.marginLeft = '1rem';

      let typeBadge = `<span class="badge" style="background:${evt.color}20; color:${evt.color}; border: 1px solid ${evt.color}30; text-transform:uppercase; font-size:0.65rem;">${evt.type}</span>`;

      const tStr = new Date(evt.timestamp).toLocaleTimeString();

      card.innerHTML = `
        <div class="timeline-dot" style="position:absolute; left:-2.45rem; top:1.25rem; width:14px; height:14px; border-radius:50%; background:${evt.color}; box-shadow: 0 0 10px ${evt.color}; border:2px solid var(--bg-primary);"></div>
        <div style="display:flex; justify-content:space-between; align-items:center; margin-bottom:0.4rem;">
          <h4 style="margin:0; font-size:0.9rem; color:#fff;">${evt.title}</h4>
          <span style="font-size:0.75rem; color:var(--text-muted); font-family:var(--font-mono);">${tStr}</span>
        </div>
        <p style="margin:0; font-size:0.82rem; color:var(--text-secondary); line-height:1.4;">${evt.description}</p>
        <div style="margin-top:0.6rem; display:flex; align-items:center; gap:0.5rem;">
          ${typeBadge}
        </div>
      `;
      eventsContainer.appendChild(card);
    });

  } catch (err) {
    eventsContainer.innerHTML = '<div class="text-center text-muted" style="padding: 2rem; color:var(--danger);">Failed to auto-generate incident timeline.</div>';
  }
}

