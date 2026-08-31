const state = {
  config: null,
  secretStatus: null,
  secrets: null,
  secretUpdates: { clients: {}, providers: {}, clientRenames: {} },
  activeView: "overview",
  discovery: null,
  usage: null,
  dirty: false,
};

const content = document.querySelector("#content");
const title = document.querySelector("#page-title");
const notice = document.querySelector("#notice");
const dialog = document.querySelector("#model-dialog");

const api = async (path, options = {}) => {
  const response = await fetch(path, { headers: { "Content-Type": "application/json", ...(options.headers || {}) }, ...options });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error?.message || `请求失败 (${response.status})`);
  return body;
};

const escapeHTML = value => String(value ?? "").replace(/[&<>'"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" })[c]);
const providerType = type => ({ openai_responses: "Standard API", codex_exec: "Codex", workbuddy_exec: "WorkBuddy" })[type] || type;
const byID = (items, id) => items.find(item => item.id === id);

function showNotice(message, error = false) {
  notice.textContent = message;
  notice.className = `notice${error ? " error" : ""}`;
  notice.hidden = false;
  clearTimeout(showNotice.timer);
  showNotice.timer = setTimeout(() => notice.hidden = true, 4600);
}

function markDirty() {
  state.dirty = true;
  const button = document.querySelector("#save-all");
  button.textContent = "保存并生效 ·";
}

async function loadState() {
  const payload = await api("/admin/api/state");
  state.config = payload.config;
  state.secretStatus = payload.secret_status;
  state.secrets = null;
  state.secretUpdates = { clients: {}, providers: {}, clientRenames: {} };
  state.dirty = false;
  document.querySelector("#save-all").textContent = "保存并生效";
  document.querySelector("#admin-address").textContent = state.config.server.admin_listen;
  render();
}

async function saveAll() {
  const button = document.querySelector("#save-all");
  button.disabled = true;
  button.textContent = "正在验证…";
  try {
    await api("/admin/api/config", {
      method: "PUT",
      body: JSON.stringify({
        config: state.config,
        client_token_updates: state.secretUpdates.clients,
        client_token_renames: state.secretUpdates.clientRenames,
        provider_key_updates: state.secretUpdates.providers,
      }),
    });
    await loadState();
    showNotice("配置已保存，新请求已使用新配置。")
    return true;
  } catch (error) {
    showNotice(error.message, true);
    return false;
  } finally {
    button.disabled = false;
    if (state.dirty) button.textContent = "保存并生效 ·";
  }
}

function render() {
  document.querySelectorAll(".nav-item").forEach(item => item.classList.toggle("active", item.dataset.view === state.activeView));
  const views = {
    overview: ["运行概览", renderOverview],
    providers: ["供应商", renderProviders],
    models: ["模型与定价", renderModels],
    usage: ["消耗统计", renderUsage],
    access: ["访问设备", renderAccess],
  };
  const [label, renderer] = views[state.activeView];
  title.textContent = label;
  renderer();
}

function renderOverview() {
  const enabled = state.config.deployments.filter(item => item.enabled).length;
  const missingSecrets = state.config.providers.filter(item => item.type === "openai_responses" && !state.secretStatus.provider_api_keys[item.id]).length;
  content.innerHTML = `
    <div class="metric-grid">
      <article class="metric"><span>已配置供应商</span><strong>${state.config.providers.length}</strong><small>${missingSecrets ? `${missingSecrets} 个缺少密钥` : "凭据状态正常"}</small></article>
      <article class="metric"><span>公开模型</span><strong>${enabled}</strong><small>共配置 ${state.config.deployments.length} 个 Deployment</small></article>
      <article class="metric"><span>访问设备</span><strong>${state.config.clients.length}</strong><small>独立 Token 与模型范围</small></article>
      <article class="metric"><span>API 入口</span><strong>${escapeHTML(state.config.server.listen.split(":").pop())}</strong><small>${escapeHTML(state.config.server.listen)}</small></article>
    </div>
    <div class="section-head"><div><h2>供应商状态</h2><p>刷新模型目录，然后决定公开哪些模型及其定价。</p></div></div>
    ${providerCards(false)}
    <div class="section-head"><div><h2>已启用模型</h2><p>公开模型 ID 与上游模型保持解耦。</p></div><button class="button" data-go="models">管理全部</button></div>
    ${readonlyModelTable(state.config.deployments.filter(item => item.enabled).slice(0, 8))}`;
  bindProviderActions();
  document.querySelector("[data-go='models']")?.addEventListener("click", () => switchView("models"));
}

function providerCards(editable) {
  return `<div class="provider-grid">${state.config.providers.map((provider, index) => {
    const hasKey = provider.type !== "openai_responses" || state.secretStatus.provider_api_keys[provider.id];
    const location = provider.base_url || provider.executable || "本机运行时";
    return `<article class="card provider-card" data-provider-index="${index}">
      <header><div><h3>${escapeHTML(provider.display_name || provider.id)}</h3><span class="type">${escapeHTML(providerType(provider.type))} · ${escapeHTML(provider.id)}</span></div><i class="provider-state ${hasKey ? "" : "missing"}"></i></header>
      ${editable ? providerEditor(provider, index, hasKey) : `<div class="meta">${escapeHTML(location)}<br>${provider.type === "openai_responses" ? (hasKey ? "API Key 已配置" : "缺少 API Key") : "使用当前用户登录态"}</div>`}
      <footer><button class="button discover-models" data-provider="${escapeHTML(provider.id)}">刷新可用模型</button>${editable ? `<button class="button danger remove-provider" data-index="${index}">删除</button>` : ""}</footer>
    </article>`;
  }).join("")}</div>`;
}

function providerEditor(provider, index, hasKey) {
  const keyField = provider.type === "openai_responses" ? `<label class="field"><span>API Key</span><div class="secret-row"><input type="password" data-provider-key="${escapeHTML(provider.id)}" placeholder="${hasKey ? "已配置，留空保持不变" : "输入 API Key"}"><button class="button reveal-secret" type="button" data-kind="provider" data-id="${escapeHTML(provider.id)}">显示</button></div></label>` : "";
  return `<div class="edit-stack">
    <label class="field"><span>显示名称</span><input data-provider-field="display_name" data-index="${index}" value="${escapeHTML(provider.display_name)}"></label>
    ${provider.type === "openai_responses" ? `<label class="field"><span>Base URL</span><input data-provider-field="base_url" data-index="${index}" value="${escapeHTML(provider.base_url)}"></label>${keyField}` : `<div class="meta">${escapeHTML(provider.executable)}<br>登录态由官方本机程序维护</div>`}
    <div class="price-pair">
      <label class="field"><span>新模型默认输入价 / 1M</span><input data-default-price-field="input_per_million" data-index="${index}" value="${escapeHTML(provider.default_public_price?.input_per_million || "0.000000")}"></label>
      <label class="field"><span>新模型默认输出价 / 1M</span><input data-default-price-field="output_per_million" data-index="${index}" value="${escapeHTML(provider.default_public_price?.output_per_million || "0.000000")}"></label>
    </div>
  </div>`;
}

function renderProviders() {
  content.innerHTML = `<div class="section-head"><div><h2>供应商连接</h2><p>非秘密写入 config.yaml；API Key 只写入上层 xconfig.yaml。</p></div><button class="button primary" id="add-api-provider">添加 API 供应商</button></div>${providerCards(true)}`;
  bindProviderActions();
  bindProviderEditors();
  document.querySelector("#add-api-provider").addEventListener("click", addAPIProvider);
}

function bindProviderEditors() {
  document.querySelectorAll("[data-provider-field]").forEach(input => input.addEventListener("input", () => {
    state.config.providers[Number(input.dataset.index)][input.dataset.providerField] = input.value;
    markDirty();
  }));
  document.querySelectorAll("[data-provider-key]").forEach(input => input.addEventListener("input", () => {
    state.secretUpdates.providers[input.dataset.providerKey] = input.value;
    markDirty();
  }));
  document.querySelectorAll("[data-default-price-field]").forEach(input => input.addEventListener("input", () => {
    const provider = state.config.providers[Number(input.dataset.index)];
    provider.default_public_price ||= { revision: `${provider.id}-default-public-v1`, currency: "USD", source: "configured" };
    provider.default_public_price[input.dataset.defaultPriceField] = input.value;
    provider.default_public_price.revision = `${provider.id}-default-public-${Date.now()}`;
    markDirty();
  }));
  document.querySelectorAll(".reveal-secret").forEach(button => button.addEventListener("click", () => revealSecret(button)));
  document.querySelectorAll(".remove-provider").forEach(button => button.addEventListener("click", () => {
    const index = Number(button.dataset.index);
    const provider = state.config.providers[index];
    if (!confirm(`删除 ${provider.display_name || provider.id} 及其全部模型？`)) return;
    state.config.deployments = state.config.deployments.filter(item => item.provider_id !== provider.id);
    state.config.providers.splice(index, 1);
    state.config.clients.forEach(client => client.allowed_deployments = client.allowed_deployments.filter(id => byID(state.config.deployments, id)));
    markDirty(); renderProviders();
  }));
}

function addAPIProvider() {
  let suffix = 1;
  while (byID(state.config.providers, `api-provider-${suffix}`)) suffix++;
  const id = `api-provider-${suffix}`;
  state.config.providers.push({
    id, type: "openai_responses", display_name: "New API Provider", base_url: "https://api.example.com/v1", wire_api: "responses",
    default_public_price: { revision: `${id}-default-v1`, currency: "USD", input_per_million: "2.000000", output_per_million: "12.000000", source: "configured" },
  });
  state.secretStatus.provider_api_keys[id] = false;
  markDirty(); renderProviders();
}

async function revealSecret(button) {
  try {
    if (!state.secrets) state.secrets = await api("/admin/api/secrets");
    const value = button.dataset.kind === "provider" ? state.secrets.provider_api_keys?.[button.dataset.id] : state.secrets.client_tokens?.[button.dataset.id];
    const input = button.parentElement.querySelector("input");
    if (!value) return showNotice("当前没有已保存的密钥。", true);
    input.value = value;
    input.type = input.type === "password" ? "text" : "password";
    button.textContent = input.type === "password" ? "显示" : "隐藏";
  } catch (error) { showNotice(error.message, true); }
}

function readonlyModelTable(models) {
  if (!models.length) return `<div class="card empty">还没有启用模型</div>`;
  return `<div class="card table-card"><table><thead><tr><th>公开 ID</th><th>供应商</th><th>上游模型</th><th>输入 / 1M</th><th>输出 / 1M</th></tr></thead><tbody>${models.map(item => `<tr><td><strong>${escapeHTML(item.id)}</strong></td><td><span class="tag">${escapeHTML(item.provider_id)}</span></td><td><code>${escapeHTML(item.upstream_model)}</code></td><td>${escapeHTML(item.price.currency)} ${escapeHTML(item.price.input_per_million)}</td><td>${escapeHTML(item.price.currency)} ${escapeHTML(item.price.output_per_million)}</td></tr>`).join("")}</tbody></table></div>`;
}

function renderModels() {
  content.innerHTML = `<div class="section-head"><div><h2>公开模型与定价</h2><p>公开价格用于调用方结算；供应商成本/积分单独记录，不互相覆盖。</p></div></div>
    <div class="card table-card model-editor"><table><thead><tr><th>启用</th><th>公开 ID / 上游</th><th>供应商</th><th>公开输入 / 1M</th><th>公开输出 / 1M</th><th>实际成本或积分</th><th></th></tr></thead><tbody>${state.config.deployments.map(modelEditorRow).join("")}</tbody></table></div>`;
  bindModelEditors();
}

function modelEditorRow(item, index) {
  const actualKind = item.actual_price ? "currency" : item.actual_points ? "points" : "none";
  const actualUnit = item.actual_price?.currency || "POINTS";
  const actualInput = item.actual_price?.input_per_million || item.actual_points?.input_per_million || "0.000000";
  const actualOutput = item.actual_price?.output_per_million || item.actual_points?.output_per_million || "0.000000";
  return `<tr data-model-index="${index}"><td><input type="checkbox" data-model-field="enabled" ${item.enabled ? "checked" : ""}></td><td><input class="compact" data-model-field="id" value="${escapeHTML(item.id)}"><small>${escapeHTML(item.upstream_model)}</small></td><td><span class="tag">${escapeHTML(item.provider_id)}</span></td><td><input class="compact" data-price-field="input_per_million" value="${escapeHTML(item.price.input_per_million)}"></td><td><input class="compact" data-price-field="output_per_million" value="${escapeHTML(item.price.output_per_million)}"></td><td><div class="actual-editor"><select data-actual-kind><option value="none" ${actualKind === "none" ? "selected" : ""}>不记录</option><option value="currency" ${actualKind === "currency" ? "selected" : ""}>货币估算</option><option value="points" ${actualKind === "points" ? "selected" : ""}>积分估算</option></select><input class="compact actual-unit" value="${escapeHTML(actualUnit)}" aria-label="实际消耗单位" ${actualKind === "points" ? "disabled" : ""}><input class="compact" data-actual-rate="input_per_million" value="${escapeHTML(actualInput)}" aria-label="实际输入费率"><input class="compact" data-actual-rate="output_per_million" value="${escapeHTML(actualOutput)}" aria-label="实际输出费率"></div></td><td><button class="button danger remove-model">删除</button></td></tr>`;
}

function bindModelEditors() {
  document.querySelectorAll("[data-model-index]").forEach(row => {
    const index = Number(row.dataset.modelIndex);
    row.querySelectorAll("[data-model-field]").forEach(input => input.addEventListener("change", () => {
      const item = state.config.deployments[index];
      if (input.dataset.modelField === "id") {
        const oldID = item.id;
        item.id = input.value.trim();
        state.config.clients.forEach(client => client.allowed_deployments = client.allowed_deployments.map(id => id === oldID ? item.id : id));
      } else {
        item[input.dataset.modelField] = input.type === "checkbox" ? input.checked : input.value;
      }
      markDirty();
    }));
    row.querySelectorAll("[data-price-field]").forEach(input => input.addEventListener("input", () => {
      state.config.deployments[index].price[input.dataset.priceField] = input.value;
      state.config.deployments[index].price.revision = `${state.config.deployments[index].id}-public-${Date.now()}`;
      markDirty();
    }));
    const syncActual = () => {
      const item = state.config.deployments[index];
      const kind = row.querySelector("[data-actual-kind]").value;
      const unitInput = row.querySelector(".actual-unit");
      const inputRate = row.querySelector("[data-actual-rate='input_per_million']").value;
      const outputRate = row.querySelector("[data-actual-rate='output_per_million']").value;
      unitInput.disabled = kind === "points";
      if (kind === "none") {
        delete item.actual_price; delete item.actual_points;
      } else if (kind === "currency") {
        item.actual_price = { revision: `${item.id}-actual-${Date.now()}`, currency: unitInput.value.trim().toUpperCase() || "USD", input_per_million: inputRate, output_per_million: outputRate, source: "configured_estimate" };
        delete item.actual_points;
      } else {
        unitInput.value = "POINTS";
        item.actual_points = { input_per_million: inputRate, output_per_million: outputRate, source: "configured_estimate" };
        delete item.actual_price;
      }
      markDirty();
    };
    row.querySelector("[data-actual-kind]").addEventListener("change", syncActual);
    row.querySelector(".actual-unit").addEventListener("input", syncActual);
    row.querySelectorAll("[data-actual-rate]").forEach(input => input.addEventListener("input", syncActual));
    row.querySelector(".remove-model").addEventListener("click", () => {
      const id = state.config.deployments[index].id;
      state.config.deployments.splice(index, 1);
      state.config.clients.forEach(client => client.allowed_deployments = client.allowed_deployments.filter(item => item !== id));
      markDirty(); renderModels();
    });
  });
}

async function renderUsage() {
  content.innerHTML = `<div class="loading"><span></span><p>正在汇总 SQLite 结算记录…</p></div>`;
  try {
    state.usage = await api("/admin/api/usage");
    const publicRows = state.usage.deployments || [];
    const actualRows = state.usage.actual || [];
    const quotaRows = state.usage.quotas || [];
    content.innerHTML = `
      <div class="section-head"><div><h2>公开定价消耗</h2><p>调用方确认结算，按 Deployment 与货币汇总。</p></div></div>
      ${usageTable(publicRows)}
      <div class="section-head"><div><h2>供应商实际口径</h2><p>供应商未实报时明确显示“配置估算”；POINTS 与货币不换算。</p></div></div>
      ${actualTable(actualRows)}
      <div class="section-head"><div><h2>订阅额度快照</h2><p>Codex 多窗口按供应商快照展示；WorkBuddy 未返回积分时保持不可用。</p></div></div>
      ${quotaTable(quotaRows)}`;
  } catch (error) { content.innerHTML = `<div class="card empty">${escapeHTML(error.message)}</div>`; }
}

function usageTable(rows) {
  if (!rows.length) return `<div class="card empty">还没有已确认的公开结算</div>`;
  return `<div class="card table-card"><table><thead><tr><th>模型</th><th>请求</th><th>输入 Token</th><th>输出 Token</th><th>公开总额</th></tr></thead><tbody>${rows.map(row => `<tr><td><strong>${escapeHTML(row.deployment_id)}</strong></td><td>${row.runs}</td><td>${Number(row.input_tokens).toLocaleString()}</td><td>${Number(row.output_tokens).toLocaleString()}</td><td><strong>${escapeHTML(row.currency)} ${escapeHTML(row.public_total)}</strong></td></tr>`).join("")}</tbody></table></div>`;
}

function actualTable(rows) {
  if (!rows.length) return `<div class="card empty">暂无供应商实报或配置估算记录</div>`;
  return `<div class="card table-card"><table><thead><tr><th>供应商</th><th>模型</th><th>单位</th><th>总消耗</th><th>来源</th></tr></thead><tbody>${rows.map(row => `<tr><td><span class="tag">${escapeHTML(row.provider_id)}</span></td><td>${escapeHTML(row.deployment_id)}</td><td>${escapeHTML(row.unit)}</td><td><strong>${escapeHTML(row.total)}</strong></td><td>${row.source === "configured_estimate" ? "配置估算" : "供应商实报"}</td></tr>`).join("")}</tbody></table></div>`;
}

function quotaTable(rows) {
  if (!rows.length) return `<div class="card empty">暂无订阅额度快照</div>`;
  const flattened = rows.flatMap(row => (row.observations || []).map(observation => ({ ...observation, deployment: row.deployment_id, observed: row.observed_at })));
  return `<div class="card table-card"><table><thead><tr><th>模型</th><th>额度窗口</th><th>调用前</th><th>调用后</th><th>变化</th><th>观测时间</th></tr></thead><tbody>${flattened.slice(0, 24).map(row => `<tr><td>${escapeHTML(row.deployment)}</td><td><code>${escapeHTML(row.limit_id)}</code></td><td>${row.before ?? "—"}%</td><td>${row.after ?? "—"}%</td><td>${row.delta ?? "—"}</td><td>${new Date(row.observed).toLocaleString()}</td></tr>`).join("")}</tbody></table></div>`;
}

function renderAccess() {
  const deploymentIDs = state.config.deployments.map(item => item.id);
  content.innerHTML = `<div class="section-head"><div><h2>访问设备</h2><p>每个设备独立 Token、模型白名单和额度返回开关。</p></div><button class="button primary" id="add-client">添加设备</button></div>
    <div class="access-grid">${state.config.clients.map((client, index) => `<article class="card access-card" data-client-index="${index}">
      <header><input class="client-name" value="${escapeHTML(client.id)}" aria-label="设备 ID"><button class="button danger remove-client">删除</button></header>
      <label class="field"><span>访问 Token</span><div class="secret-row"><input type="password" data-client-token="${escapeHTML(client.id)}" placeholder="已配置，留空保持不变"><button type="button" class="button reveal-secret" data-kind="client" data-id="${escapeHTML(client.id)}">显示</button><button type="button" class="button generate-token">重新生成</button></div></label>
      <label class="check-line"><input type="checkbox" class="quota-toggle" ${client.include_quota_observations ? "checked" : ""}>允许响应返回订阅额度观测</label>
      <div class="model-checks">${deploymentIDs.map(id => `<label><input type="checkbox" value="${escapeHTML(id)}" ${client.allowed_deployments.includes(id) ? "checked" : ""}>${escapeHTML(id)}</label>`).join("")}</div>
    </article>`).join("")}</div>`;
  bindAccessEditors();
  document.querySelector("#add-client").addEventListener("click", addClient);
}

function bindAccessEditors() {
  document.querySelectorAll("[data-client-index]").forEach(card => {
    const index = Number(card.dataset.clientIndex);
    const client = state.config.clients[index];
    card.querySelector(".client-name").addEventListener("change", event => {
      const oldID = client.id;
      client.id = event.target.value.trim();
      if (oldID !== client.id) {
        const originalID = state.secretUpdates.clientRenames[oldID] || oldID;
        delete state.secretUpdates.clientRenames[oldID];
        state.secretUpdates.clientRenames[client.id] = originalID;
      }
      markDirty();
    });
    card.querySelector("[data-client-token]").addEventListener("input", event => { state.secretUpdates.clients[client.id] = event.target.value; markDirty(); });
    card.querySelector(".quota-toggle").addEventListener("change", event => { client.include_quota_observations = event.target.checked; markDirty(); });
    card.querySelectorAll(".model-checks input").forEach(input => input.addEventListener("change", () => {
      client.allowed_deployments = [...card.querySelectorAll(".model-checks input:checked")].map(item => item.value);
      markDirty();
    }));
    card.querySelector(".generate-token").addEventListener("click", async () => {
      try {
        const result = await api("/admin/api/tokens/generate", { method: "POST", body: "{}" });
        const input = card.querySelector("[data-client-token]");
        input.value = result.token; input.type = "text";
        state.secretUpdates.clients[client.id] = result.token; markDirty();
        showNotice("新 Token 已生成；保存后旧 Token 立即失效。")
      } catch (error) { showNotice(error.message, true); }
    });
    card.querySelector(".remove-client").addEventListener("click", () => { if (confirm(`删除设备 ${client.id}？`)) { state.config.clients.splice(index, 1); markDirty(); renderAccess(); } });
  });
  document.querySelectorAll(".reveal-secret").forEach(button => button.addEventListener("click", () => revealSecret(button)));
}

async function addClient() {
  let suffix = 1;
  while (byID(state.config.clients, `device-${suffix}`)) suffix++;
  const id = `device-${suffix}`;
  try {
    const result = await api("/admin/api/tokens/generate", { method: "POST", body: "{}" });
    state.config.clients.push({ id, allowed_deployments: [], include_quota_observations: false });
    state.secretUpdates.clients[id] = result.token;
    markDirty(); renderAccess();
    const tokenInput = document.querySelector(`[data-client-token="${id}"]`);
    if (tokenInput) { tokenInput.value = result.token; tokenInput.type = "text"; }
    showNotice("设备及持久 Token 已加入草稿；设置模型权限后保存。")
  } catch (error) { showNotice(error.message, true); }
}

function bindProviderActions() {
  document.querySelectorAll(".discover-models").forEach(button => button.addEventListener("click", () => discoverModels(button.dataset.provider, button)));
}

async function discoverModels(providerID, button) {
  const old = button.textContent;
  button.disabled = true; button.textContent = "正在刷新…";
  try {
    if (state.dirty && !(await saveAll())) return;
    state.discovery = await api(`/admin/api/providers/${encodeURIComponent(providerID)}/models`, { method: "POST", body: "{}" });
    const existing = new Set(state.config.deployments.filter(item => item.provider_id === providerID).map(item => item.upstream_model));
    const provider = byID(state.config.providers, providerID);
    const defaults = provider.default_public_price || { currency: "USD", input_per_million: "2.000000", output_per_million: "12.000000" };
    document.querySelector("#model-dialog-body").innerHTML = state.discovery.models.map((model, index) => `<label class="model-choice" data-discovery-index="${index}" data-model-id="${escapeHTML(model.id)}">
      <input type="checkbox" ${existing.has(model.id) ? "checked disabled" : ""}>
      <span><strong>${escapeHTML(model.display_name || model.id)}</strong><br><small class="muted">${escapeHTML(model.id)}</small></span>
      <input type="text" value="${escapeHTML(model.id)}" aria-label="公开模型 ID">
      <input type="text" value="${escapeHTML(defaults.input_per_million)}" aria-label="输入价格">
      <input type="text" value="${escapeHTML(defaults.output_per_million)}" aria-label="输出价格">
    </label>`).join("") || `<div class="empty">供应商没有返回模型</div>`;
    dialog.showModal();
  } catch (error) { showNotice(error.message, true); }
  finally { button.disabled = false; button.textContent = old; }
}

function applyDiscoveredModels() {
  if (!state.discovery) return;
  const provider = byID(state.config.providers, state.discovery.provider_id);
  document.querySelectorAll(".model-choice").forEach(row => {
    const checkbox = row.querySelector("input[type='checkbox']");
    if (!checkbox.checked || checkbox.disabled) return;
    const inputs = row.querySelectorAll("input[type='text']");
    const model = state.discovery.models[Number(row.dataset.discoveryIndex)];
    const id = inputs[0].value.trim();
    const price = { revision: `${id}-public-${Date.now()}`, currency: provider.default_public_price?.currency || "USD", input_per_million: inputs[1].value, output_per_million: inputs[2].value, source: "configured" };
    const deployment = { id, provider_id: provider.id, upstream_model: model.id, supported_reasoning_efforts: model.supported_reasoning_efforts || [], price, hard_budget_supported: provider.type === "openai_responses", enabled: true };
    if (provider.type === "openai_responses") deployment.actual_price = { ...price, revision: `${id}-actual-${Date.now()}`, source: "configured_estimate" };
    state.config.deployments.push(deployment);
  });
  markDirty(); dialog.close(); switchView("models"); showNotice("模型已加入草稿；点击“保存并生效”完成发布。")
}

function switchView(view) { state.activeView = view; render(); }
document.querySelectorAll(".nav-item").forEach(item => item.addEventListener("click", () => switchView(item.dataset.view)));
document.querySelector("#save-all").addEventListener("click", saveAll);
document.querySelector("#apply-models").addEventListener("click", applyDiscoveredModels);
window.addEventListener("beforeunload", event => { if (state.dirty) { event.preventDefault(); event.returnValue = ""; } });
loadState().catch(error => { content.innerHTML = `<div class="card empty">${escapeHTML(error.message)}</div>`; showNotice(error.message, true); });
