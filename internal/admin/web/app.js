const state = {
  config: null,
  secretStatus: null,
  secrets: null,
  secretUpdates: { clients: {}, providers: {}, clientRenames: {} },
  activeView: "overview",
  dirty: false,
  providerDraft: null,
  modelRows: [],
  modelProviderID: "",
  usageRange: { value: 1, unit: "days" },
  platformProvider: "",
  usageClient: "",
};

const content = document.querySelector("#content");
const pageTitle = document.querySelector("#page-title");
const toast = document.querySelector("#toast");
const toastMessage = document.querySelector("#toast-message");
const modelDialog = document.querySelector("#model-dialog");
const providerDialog = document.querySelector("#provider-dialog");

async function api(path, options = {}) {
  const { headers = {}, ...rest } = options;
  const response = await fetch(path, { ...rest, headers: { "Content-Type": "application/json", ...headers } });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error?.message || `请求失败 (${response.status})`);
  return body;
}

const escapeHTML = value => String(value ?? "").replace(/[&<>'"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" })[c]);
const byID = (items, id) => items.find(item => item.id === id);
const clone = value => JSON.parse(JSON.stringify(value));
const providerType = type => type === "openai_responses" ? "API 供应商" : "系统供应商";
const providerName = id => byID(state.config.providers, id)?.display_name || id;
const usageProviderName = id => {
  const provider = byID(state.config.providers, id);
  if (provider?.type === "codex_exec") return "Codex";
  if (provider?.type === "workbuddy_exec") return "WorkBuddy";
  return provider?.display_name || id;
};
const secretHint = (kind, id) => kind === "provider" ? state.secretStatus.provider_api_key_hints?.[id] : state.secretStatus.client_token_hints?.[id];
const fixedDecimal = (value, digits) => {
  const number = Number(value ?? 0);
  return Number.isFinite(number) ? number.toFixed(digits) : Number(0).toFixed(digits);
};
const compactDecimal = (value, digits) => fixedDecimal(value, digits).replace(/\.?0+$/, "");

function normalizedPrice(value, label) {
  const number = Number(value);
  if (!Number.isFinite(number) || number < 0) {
    showToast(`${label}必须是非负数字。`, true);
    return null;
  }
  return number.toFixed(2);
}

function hideToast() {
  clearTimeout(showToast.timer);
  clearTimeout(showToast.hideTimer);
  toast.classList.remove("visible");
  toast.setAttribute("aria-hidden", "true");
  showToast.hideTimer = setTimeout(() => {
    if (typeof toast.hidePopover === "function" && toast.matches(":popover-open")) toast.hidePopover();
  }, 180);
}

function showToast(message, error = false) {
  clearTimeout(showToast.timer);
  clearTimeout(showToast.hideTimer);
  toastMessage.textContent = message;
  toast.className = `toast${error ? " error" : ""}`;
  toast.setAttribute("role", error ? "alert" : "status");
  toast.setAttribute("aria-live", error ? "assertive" : "polite");
  toast.setAttribute("aria-hidden", "false");
  if (typeof toast.showPopover === "function" && !toast.matches(":popover-open")) toast.showPopover();
  requestAnimationFrame(() => toast.classList.add("visible"));
  showToast.timer = setTimeout(hideToast, error ? 7000 : 4200);
}

function markDirty() {
  state.dirty = true;
  document.querySelector("#save-all").textContent = "保存并生效 ·";
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
    showToast("配置已保存，新请求已使用新配置。")
    return true;
  } catch (error) {
    showToast(error.message, true);
    return false;
  } finally {
    button.disabled = false;
    if (state.dirty) button.textContent = "保存并生效 ·";
  }
}

function render() {
  document.querySelectorAll(".nav-item").forEach(item => item.classList.toggle("active", item.dataset.view === state.activeView));
  const views = {
    overview: ["概览", renderOverview],
    providers: ["供应商", renderProviders],
    platform_usage: ["平台消耗", () => renderUsage("provider")],
    access: ["访问密钥", renderAccess],
    user_usage: ["用户消耗", () => renderUsage("client")],
  };
  const [title, renderer] = views[state.activeView];
  pageTitle.textContent = title;
  renderer();
}

function renderOverview() {
  const enabled = state.config.deployments.filter(item => item.enabled);
  const apiProviders = state.config.providers.filter(item => item.type === "openai_responses");
  const missingKeys = apiProviders.filter(item => !state.secretStatus.provider_api_keys[item.id]).length;
  content.innerHTML = `
    <div class="metric-grid">
      <article class="metric"><span>供应商</span><strong>${state.config.providers.length}</strong><small>${apiProviders.length} 个 API · ${state.config.providers.length - apiProviders.length} 个系统</small></article>
      <article class="metric"><span>启用模型</span><strong>${enabled.length}</strong><small>共配置 ${state.config.deployments.length} 个模型</small></article>
      <article class="metric"><span>访问密钥</span><strong>${state.config.clients.length}</strong><small>独立模型访问权限</small></article>
      <article class="metric"><span>连接状态</span><strong>${missingKeys ? "待配置" : "正常"}</strong><small>${missingKeys ? `${missingKeys} 个 API 缺少 Key` : state.config.server.listen}</small></article>
    </div>
    <div class="section-head"><div><h2>供应商与模型</h2><p>刷新目录后可在同一窗口启停模型、修改公开价格和后台实际消耗口径。</p></div><button class="button" data-go="providers">管理供应商</button></div>
    ${providerCards(state.config.providers, false)}
    <div class="section-head"><div><h2>已启用模型</h2><p>这里只展示当前可被访问密钥授权的公开模型。</p></div></div>
    ${enabledModelTable(enabled)}`;
  bindProviderCardActions();
  document.querySelector("[data-go='providers']")?.addEventListener("click", () => switchView("providers"));
}

function enabledModelTable(models) {
  if (!models.length) return `<div class="empty-state">还没有启用模型</div>`;
  return `<div class="table-card"><table><thead><tr><th>公开模型</th><th>供应商</th><th>上游模型</th><th>平台输入 / 1M</th><th>平台输出 / 1M</th></tr></thead><tbody>${models.map(item => `<tr><td><strong>${escapeHTML(item.id)}</strong></td><td>${escapeHTML(providerName(item.provider_id))}</td><td><code>${escapeHTML(item.upstream_model)}</code></td><td>${escapeHTML(item.price.currency)} ${fixedDecimal(item.price.input_per_million, 2)}</td><td>${escapeHTML(item.price.currency)} ${fixedDecimal(item.price.output_per_million, 2)}</td></tr>`).join("")}</tbody></table></div>`;
}

function providerCards(providers, editable) {
  if (!providers.length) return `<div class="empty-state">还没有供应商</div>`;
  return `<div class="provider-grid">${providers.map(provider => {
    const models = state.config.deployments.filter(item => item.provider_id === provider.id);
    const enabled = models.filter(item => item.enabled).length;
    const isAPI = provider.type === "openai_responses";
    const key = isAPI ? (secretHint("provider", provider.id) || "未配置 API Key") : "使用当前系统登录态";
    return `<article class="provider-card">
      <header><div><span class="category">${providerType(provider.type)}</span><h3 title="${escapeHTML(provider.display_name || provider.id)}">${escapeHTML(provider.display_name || provider.id)}</h3></div><i class="state-dot ${isAPI && !state.secretStatus.provider_api_keys[provider.id] ? "warning" : ""}"></i></header>
      <p class="provider-id">${escapeHTML(provider.id)}</p>
      <dl><div><dt>连接</dt><dd>${escapeHTML(provider.base_url || provider.executable)}</dd></div><div><dt>${isAPI ? "API Key" : "凭据"}</dt><dd class="mono">${escapeHTML(key)}</dd></div><div><dt>模型</dt><dd>${enabled} 已启用 / ${models.length} 已配置</dd></div></dl>
      <footer><button class="button primary provider-models" data-provider="${escapeHTML(provider.id)}">刷新 / 设置模型</button>${editable ? `<button class="button edit-provider" data-provider="${escapeHTML(provider.id)}">编辑</button>` : ""}${isAPI ? `<button class="icon-copy copy-secret" data-kind="provider" data-id="${escapeHTML(provider.id)}" title="复制 API Key">复制 Key</button>` : ""}</footer>
    </article>`;
  }).join("")}</div>`;
}

function renderProviders() {
  const apiProviders = state.config.providers.filter(item => item.type === "openai_responses");
  const systemProviders = state.config.providers.filter(item => item.type !== "openai_responses");
  content.innerHTML = `
    <div class="section-head first"><div><h2>API 供应商</h2><p>可添加、删除和配置。卡片显示基础状态，详细兼容参数在“编辑”中设置。</p></div><button class="button primary" id="add-api-provider">添加 API 供应商</button></div>
    ${providerCards(apiProviders, true)}
    <div class="section-head"><div><h2>系统供应商</h2><p>Codex 与 WorkBuddy 使用当前 macOS 用户登录态，只允许修改连接和定价设置，不能在页面新增。</p></div></div>
    ${providerCards(systemProviders, true)}`;
  bindProviderCardActions();
  document.querySelector("#add-api-provider").addEventListener("click", addAPIProvider);
}

function bindProviderCardActions() {
  document.querySelectorAll(".provider-models").forEach(button => button.addEventListener("click", () => discoverModels(button.dataset.provider, button)));
  document.querySelectorAll(".edit-provider").forEach(button => button.addEventListener("click", () => openProviderEditor(button.dataset.provider)));
  document.querySelectorAll(".copy-secret").forEach(button => button.addEventListener("click", () => copySecret(button.dataset.kind, button.dataset.id)));
}

function addAPIProvider() {
  let suffix = 1;
  while (byID(state.config.providers, `api-provider-${suffix}`)) suffix++;
  state.providerDraft = {
    isNew: true,
    index: -1,
    provider: {
      id: `api-provider-${suffix}`, type: "openai_responses", display_name: "New API Provider",
      base_url: "https://api.example.com/v1", wire_api: "responses", api_key_header: "Authorization", api_key_prefix: "Bearer",
      default_public_price: { revision: `api-provider-${suffix}-default-v1`, currency: "USD", input_per_million: "2.000000", output_per_million: "12.000000", source: "configured" },
    },
  };
  renderProviderDialog();
}

function openProviderEditor(providerID) {
  const index = state.config.providers.findIndex(item => item.id === providerID);
  state.providerDraft = { isNew: false, index, provider: clone(state.config.providers[index]) };
  renderProviderDialog();
}

function renderProviderDialog() {
  const { provider, isNew } = state.providerDraft;
  const isAPI = provider.type === "openai_responses";
  document.querySelector("#provider-dialog-title").textContent = isNew ? "添加 API 供应商" : `编辑 ${provider.display_name || provider.id}`;
  document.querySelector("#provider-dialog-caption").textContent = isAPI ? "Base URL 只覆盖标准约定；特殊供应商可单独设置端点和鉴权 Header。" : "系统供应商直接复用当前用户登录态，不复制凭据。";
  document.querySelector("#provider-dialog-body").innerHTML = `
    <div class="form-grid">
      <label class="field"><span>供应商 ID</span><input data-provider-field="id" value="${escapeHTML(provider.id)}" ${isNew ? "" : "disabled"}></label>
      <label class="field"><span>显示名称</span><input data-provider-field="display_name" value="${escapeHTML(provider.display_name)}"></label>
      ${isAPI ? apiProviderFields(provider) : systemProviderFields(provider)}
      <div class="form-section"><h3>新模型默认平台价</h3><p>刷新模型时作为初始值，之后每个模型可以独立修改。</p></div>
      <label class="field"><span>货币</span><input data-default-field="currency" value="${escapeHTML(provider.default_public_price?.currency || "USD")}"></label>
      <label class="field"><span>输入 / 1M Token</span><input data-default-field="input_per_million" value="${fixedDecimal(provider.default_public_price?.input_per_million, 2)}"></label>
      <label class="field"><span>输出 / 1M Token</span><input data-default-field="output_per_million" value="${fixedDecimal(provider.default_public_price?.output_per_million, 2)}"></label>
    </div>`;
  const deleteButton = document.querySelector("#delete-provider");
  deleteButton.hidden = isNew || !isAPI;
  providerDialog.showModal();
}

function apiProviderFields(provider) {
  return `
    <div class="form-section"><h3>基础连接</h3><p>默认约定为 <code>/v1/models</code>、<code>/v1/responses</code> 和 Bearer 鉴权。</p></div>
    <label class="field full"><span>Base URL</span><input data-provider-field="base_url" value="${escapeHTML(provider.base_url || "")}"></label>
    <label class="field"><span>API Key</span><div class="secret-box"><code>${escapeHTML(secretHint("provider", provider.id) || "未配置")}</code><button type="button" class="button copy-dialog-secret">复制</button></div></label>
    <label class="field"><span>替换 API Key（留空保持）</span><input type="password" id="provider-key-update" placeholder="输入新 Key"></label>
    <div class="form-section"><h3>兼容设置</h3><p>仅在供应商不遵循标准路径或鉴权格式时填写。</p></div>
    <label class="field full"><span>Models 完整 URL（可选）</span><input data-provider-field="models_url" value="${escapeHTML(provider.models_url || "")}" placeholder="https://…/models?api-version=…"></label>
    <label class="field full"><span>Responses 完整 URL（可选）</span><input data-provider-field="responses_url" value="${escapeHTML(provider.responses_url || "")}" placeholder="https://…/responses?api-version=…"></label>
    <label class="field"><span>API Key Header</span><input data-provider-field="api_key_header" value="${escapeHTML(provider.api_key_header || "Authorization")}"></label>
    <label class="field"><span>Key 前缀</span><input data-provider-field="api_key_prefix" value="${escapeHTML(provider.api_key_prefix || "Bearer")}" placeholder="Bearer；无前缀填 none"></label>`;
}

function systemProviderFields(provider) {
  return `
    <div class="form-section"><h3>本机程序</h3><p>版本前缀不匹配时供应商会拒绝接单，避免静默协议漂移。</p></div>
    <label class="field full"><span>可执行程序</span><input data-provider-field="executable" value="${escapeHTML(provider.executable || "")}"></label>
    <label class="field"><span>版本前缀</span><input data-provider-field="expected_version" value="${escapeHTML(provider.expected_version || "")}"></label>
    <label class="field"><span>最大并发</span><input type="number" min="1" max="16" data-provider-field="max_concurrency" value="${Number(provider.max_concurrency) || 2}"></label>
    <label class="field"><span>默认推理强度</span><select data-provider-field="default_reasoning_effort"><option value="" ${provider.default_reasoning_effort ? "" : "selected"}>由上游决定</option>${["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"].map(value => `<option value="${value}" ${provider.default_reasoning_effort === value ? "selected" : ""}>${value}</option>`).join("")}</select></label>
    ${provider.type === "codex_exec" ? `<label class="field"><span>速度层级</span><select data-provider-field="service_tier"><option value="" ${provider.service_tier ? "" : "selected"}>标准</option><option value="priority" ${provider.service_tier === "priority" ? "selected" : ""}>Fast（额外额度）</option></select></label>` : ""}
    ${provider.type === "workbuddy_exec" ? `<div class="form-section"><h3>常驻与预热</h3><p>启动及 worker 异常恢复时，用最短真实生成预热每个常驻槽位；会消耗少量积分。</p></div>
      <label class="field"><span>自动预热</span><input type="checkbox" data-provider-field="warmup_enabled" ${provider.warmup_enabled ? "checked" : ""}></label>
      <label class="field"><span>预热模型</span><input data-provider-field="warmup_model" value="${escapeHTML(provider.warmup_model || "hy4-preview")}"></label>
      <label class="field"><span>预热超时（秒）</span><input type="number" min="5" max="300" data-provider-field="warmup_timeout_seconds" value="${Number(provider.warmup_timeout_seconds) || 30}"></label>` : ""}`;
}

async function applyProvider() {
  const draft = state.providerDraft;
  const provider = draft.provider;
  document.querySelectorAll("[data-provider-field]").forEach(input => {
    provider[input.dataset.providerField] = input.type === "checkbox" ? input.checked : input.type === "number" ? Number(input.value) : input.value.trim();
  });
  provider.default_public_price ||= {};
  document.querySelectorAll("[data-default-field]").forEach(input => provider.default_public_price[input.dataset.defaultField] = input.value.trim());
  const defaultInput = normalizedPrice(provider.default_public_price.input_per_million, "默认输入价格");
  const defaultOutput = normalizedPrice(provider.default_public_price.output_per_million, "默认输出价格");
  if (defaultInput === null || defaultOutput === null) return;
  provider.default_public_price.input_per_million = defaultInput;
  provider.default_public_price.output_per_million = defaultOutput;
  provider.default_public_price.revision = `${provider.id}-default-${Date.now()}`;
  provider.default_public_price.source = "configured";
  if (!provider.id || byID(state.config.providers.filter((_, index) => index !== draft.index), provider.id)) {
    return showToast("供应商 ID 为空或重复。", true);
  }
  if (draft.isNew) state.config.providers.push(provider); else state.config.providers[draft.index] = provider;
  const key = document.querySelector("#provider-key-update")?.value.trim();
  if (key) state.secretUpdates.providers[provider.id] = key;
  markDirty();
  if (await saveAll()) providerDialog.close();
}

async function deleteProvider() {
  const { provider, index } = state.providerDraft;
  if (!confirm(`删除 ${provider.display_name || provider.id} 以及它的全部模型？`)) return;
  const removedIDs = new Set(state.config.deployments.filter(item => item.provider_id === provider.id).map(item => item.id));
  state.config.providers.splice(index, 1);
  state.config.deployments = state.config.deployments.filter(item => item.provider_id !== provider.id);
  state.config.clients.forEach(client => client.allowed_deployments = client.allowed_deployments.filter(id => !removedIDs.has(id)));
  markDirty();
  if (await saveAll()) providerDialog.close();
}

async function discoverModels(providerID, button) {
  const oldText = button?.textContent;
  if (button) { button.disabled = true; button.textContent = "正在刷新…"; }
  try {
    if (state.dirty && !(await saveAll())) return;
    const discovery = await api(`/admin/api/providers/${encodeURIComponent(providerID)}/models`, { method: "POST", body: "{}" });
    const provider = byID(state.config.providers, providerID);
    const existing = state.config.deployments.map((item, index) => ({ item, index })).filter(row => row.item.provider_id === providerID);
    const byUpstream = new Map(existing.map(row => [row.item.upstream_model, row]));
    state.modelRows = discovery.models.map(model => {
      const configured = byUpstream.get(model.id);
      if (configured) byUpstream.delete(model.id);
      return { model, configIndex: configured?.index ?? -1, deployment: configured ? clone(configured.item) : null, discovered: true };
    });
    for (const configured of byUpstream.values()) {
      state.modelRows.push({ model: { id: configured.item.upstream_model, display_name: configured.item.upstream_model }, configIndex: configured.index, deployment: clone(configured.item), discovered: false });
    }
    state.modelProviderID = providerID;
    renderModelDialog(provider);
  } catch (error) { showToast(error.message, true); }
  finally { if (button) { button.disabled = false; button.textContent = oldText; } }
}

function renderModelDialog(provider) {
  document.querySelector("#model-dialog-title").textContent = `${provider.display_name || provider.id} · 模型设置`;
  document.querySelector("#model-dialog-caption").textContent = "取消勾选会立即停用已有模型；未被本次目录发现的既有模型仍保留在列表末尾。";
  const defaults = provider.default_public_price || { currency: "USD", input_per_million: "0", output_per_million: "0" };
  const multiplierHeader = provider.type === "workbuddy_exec" ? "<th>积分倍率</th>" : "";
  document.querySelector("#model-dialog-body").innerHTML = `<div class="model-table-wrap"><table class="model-table ${provider.type}"><thead><tr><th>启用</th><th>供应商模型</th>${multiplierHeader}<th>公开模型 ID</th><th>平台输入 / 1M</th><th>平台输出 / 1M</th><th>实际口径</th><th>单位</th><th>实际输入 / 1M</th><th>实际输出 / 1M</th></tr></thead><tbody>${state.modelRows.map((row, index) => modelDialogRow(row, index, defaults, provider)).join("")}</tbody></table></div>`;
  document.querySelectorAll("[data-model-row]").forEach(element => {
    element.querySelector(".actual-kind")?.addEventListener("change", () => syncActualControls(element));
    syncActualControls(element);
  });
  modelDialog.showModal();
}

function modelDialogRow(row, index, defaults, provider) {
  const deployment = row.deployment;
  const actualKind = deployment?.actual_price ? "currency" : deployment?.actual_points ? "points" : "none";
  const actual = deployment?.actual_price || deployment?.actual_points || {};
  const multiplier = provider.type === "workbuddy_exec" ? `<td class="credit-multiplier">${row.model.credit_multiplier ? `× ${fixedDecimal(row.model.credit_multiplier, 2)}` : row.model.id === "auto" ? "动态" : "未公布"}</td>` : "";
  let actualFields = `<td><select class="compact actual-kind"><option value="none" ${actualKind === "none" ? "selected" : ""}>不记录</option><option value="currency" ${actualKind === "currency" ? "selected" : ""}>货币</option><option value="points" ${actualKind === "points" ? "selected" : ""}>积分</option></select></td>
    <td><input class="compact actual-unit" value="${escapeHTML(deployment?.actual_price?.currency || (actualKind === "points" ? "POINTS" : "USD"))}"></td>
    <td><input class="compact actual-input" value="${fixedDecimal(actual.input_per_million, 2)}"></td>
    <td><input class="compact actual-output" value="${fixedDecimal(actual.output_per_million, 2)}"></td>`;
  if (provider.type === "codex_exec") {
    actualFields = `<td><select class="compact actual-kind" disabled><option value="none">不记录</option></select></td><td><input class="compact actual-unit" value="" disabled></td><td><input class="compact actual-input" value="" disabled></td><td><input class="compact actual-output" value="" disabled></td>`;
  } else if (provider.type === "workbuddy_exec") {
    actualFields = `<td><select class="compact actual-kind" disabled><option value="reported_points">积分实报</option></select></td><td><input class="compact actual-unit" value="POINTS" disabled></td><td><input class="compact actual-input" value="实际值" disabled></td><td><input class="compact actual-output" value="实际值" disabled></td>`;
  }
  return `<tr data-model-row="${index}">
    <td><input type="checkbox" class="model-enabled" ${deployment?.enabled ? "checked" : ""}></td>
    <td><strong>${escapeHTML(row.model.display_name || row.model.id)}</strong><small>${escapeHTML(row.model.id)}${row.discovered ? "" : " · 本次未发现"}</small></td>
    ${multiplier}
    <td><input class="compact public-id" value="${escapeHTML(deployment?.id || row.model.id)}"></td>
    <td><input class="compact price-input public-input" value="${fixedDecimal(deployment?.price?.input_per_million || defaults.input_per_million, 2)}"></td>
    <td><input class="compact price-input public-output" value="${fixedDecimal(deployment?.price?.output_per_million || defaults.output_per_million, 2)}"></td>
    ${actualFields}
  </tr>`;
}

function syncActualControls(element) {
  const selector = element.querySelector(".actual-kind");
  const kind = selector?.value;
  const unit = element.querySelector(".actual-unit");
  const inputs = [element.querySelector(".actual-input"), element.querySelector(".actual-output")];
  if (!unit || !inputs[0]) return;
  if (selector.disabled) return;
  unit.disabled = kind !== "currency";
  inputs.forEach(input => input.disabled = kind === "none");
  if (kind === "points") unit.value = "POINTS";
  if (kind === "currency" && (!unit.value || unit.value === "POINTS")) unit.value = "USD";
}

async function applyModels() {
  const provider = byID(state.config.providers, state.modelProviderID);
  const rows = [...document.querySelectorAll("[data-model-row]")];
  const proposed = [];
  for (const element of rows) {
    const row = state.modelRows[Number(element.dataset.modelRow)];
    const enabled = element.querySelector(".model-enabled").checked;
    if (!row.deployment && !enabled) continue;
    const id = element.querySelector(".public-id").value.trim();
    if (!id) return showToast("公开模型 ID 不能为空。", true);
    const publicInput = normalizedPrice(element.querySelector(".public-input").value.trim(), `${id} 平台输入价格`);
    const publicOutput = normalizedPrice(element.querySelector(".public-output").value.trim(), `${id} 平台输出价格`);
    if (publicInput === null || publicOutput === null) return;
    const price = {
      revision: `${id}-public-${Date.now()}`, currency: provider.default_public_price?.currency || "USD",
      input_per_million: publicInput, output_per_million: publicOutput, source: "configured",
    };
    const deployment = row.deployment || {
      id, provider_id: provider.id, upstream_model: row.model.id,
      supported_reasoning_efforts: row.model.supported_reasoning_efforts || [], hard_budget_supported: provider.type === "openai_responses",
    };
    deployment.id = id;
    deployment.enabled = enabled;
    deployment.price = price;
    delete deployment.actual_price;
    delete deployment.actual_points;
    const actualKind = element.querySelector(".actual-kind")?.value;
    if (provider.type === "openai_responses" && (actualKind === "currency" || actualKind === "points")) {
      const actualInput = normalizedPrice(element.querySelector(".actual-input").value, `${id} 实际输入价格`);
      const actualOutput = normalizedPrice(element.querySelector(".actual-output").value, `${id} 实际输出价格`);
      if (actualInput === null || actualOutput === null) return;
      if (actualKind === "currency") {
        deployment.actual_price = { revision: `${id}-actual-${Date.now()}`, currency: element.querySelector(".actual-unit").value.trim().toUpperCase() || "USD", input_per_million: actualInput, output_per_million: actualOutput, source: "configured_estimate" };
      } else {
        deployment.actual_points = { input_per_million: actualInput, output_per_million: actualOutput, source: "configured_estimate" };
      }
    }
    proposed.push({ row, deployment });
  }
  const finalIDs = new Set(state.config.deployments.filter(item => item.provider_id !== provider.id).map(item => item.id));
  for (const item of proposed) {
    if (finalIDs.has(item.deployment.id)) return showToast(`公开模型 ID ${item.deployment.id} 重复。`, true);
    finalIDs.add(item.deployment.id);
  }
  const oldToNew = new Map();
  for (const item of proposed) {
    if (item.row.configIndex >= 0) {
      oldToNew.set(state.config.deployments[item.row.configIndex].id, item.deployment.id);
      state.config.deployments[item.row.configIndex] = item.deployment;
    } else {
      state.config.deployments.push(item.deployment);
    }
  }
  state.config.clients.forEach(client => client.allowed_deployments = client.allowed_deployments.map(id => oldToNew.get(id) || id));
  markDirty();
  if (await saveAll()) modelDialog.close();
}

function renderAccess() {
  content.innerHTML = `
    <div class="section-head first"><div><h2>访问密钥</h2><p>每个密钥独立控制可访问的模型。</p></div><button class="button primary" id="add-client">生成新密钥</button></div>
    <div class="key-list">${state.config.clients.map((client, index) => accessKeyCard(client, index)).join("") || `<div class="empty-state">还没有访问密钥</div>`}</div>`;
  bindAccessEditors();
  document.querySelector("#add-client").addEventListener("click", addClient);
}

function accessKeyCard(client, index) {
  return `<article class="key-card" data-client-index="${index}">
    <div class="key-summary"><div><span class="category">ACCESS KEY</span><h3 title="${escapeHTML(client.id)}">${escapeHTML(client.id)}</h3><code title="${escapeHTML(secretHint("client", client.id) || "未配置")}">${escapeHTML(secretHint("client", client.id) || "未配置")}</code></div><div class="key-actions"><button class="button copy-client">复制</button><button class="button regenerate-client">重新生成</button><button class="button danger remove-client">删除</button></div></div>
    <details><summary>权限设置 <span>${client.allowed_deployments.length} 个模型</span></summary><div class="key-settings">
      <label class="field"><span>密钥名称</span><input class="client-name" value="${escapeHTML(client.id)}"></label>
      <div class="permission-grid">${state.config.deployments.map(model => `<label><input type="checkbox" value="${escapeHTML(model.id)}" ${client.allowed_deployments.includes(model.id) ? "checked" : ""}>${escapeHTML(model.id)}</label>`).join("")}</div>
    </div></details>
  </article>`;
}

function bindAccessEditors() {
  document.querySelectorAll("[data-client-index]").forEach(card => {
    const index = Number(card.dataset.clientIndex);
    const client = state.config.clients[index];
    card.querySelector(".copy-client").addEventListener("click", () => copySecret("client", client.id));
    card.querySelector(".regenerate-client").addEventListener("click", () => regenerateClient(client));
    card.querySelector(".remove-client").addEventListener("click", () => {
      if (!confirm(`删除访问密钥 ${client.id}？`)) return;
      state.config.clients.splice(index, 1); markDirty(); renderAccess();
    });
    card.querySelector(".client-name").addEventListener("change", event => {
      const oldID = client.id;
      client.id = event.target.value.trim();
      if (oldID !== client.id) {
        const originalID = state.secretUpdates.clientRenames[oldID] || oldID;
        delete state.secretUpdates.clientRenames[oldID];
        state.secretUpdates.clientRenames[client.id] = originalID;
      }
      markDirty(); renderAccess();
    });
    card.querySelectorAll(".permission-grid input").forEach(input => input.addEventListener("change", () => {
      client.allowed_deployments = [...card.querySelectorAll(".permission-grid input:checked")].map(item => item.value);
      markDirty();
    }));
  });
}

async function addClient() {
  let suffix = 1;
  while (byID(state.config.clients, `access-${suffix}`)) suffix++;
  const id = `access-${suffix}`;
  try {
    const result = await api("/admin/api/tokens/generate", { method: "POST", body: "{}" });
    state.config.clients.push({ id, allowed_deployments: [] });
    state.secretUpdates.clients[id] = result.token;
    markDirty();
    if (await saveAll()) {
      await copyText(result.token);
      showToast("新密钥已生成并复制；请继续设置模型权限。")
    }
  } catch (error) { showToast(error.message, true); }
}

async function regenerateClient(client) {
  if (!confirm(`重新生成 ${client.id}？旧密钥保存后立即失效。`)) return;
  try {
    const result = await api("/admin/api/tokens/generate", { method: "POST", body: "{}" });
    state.secretUpdates.clients[client.id] = result.token;
    markDirty();
    if (await saveAll()) {
      await copyText(result.token);
      showToast("新密钥已保存并复制，旧密钥已经失效。")
    }
  } catch (error) { showToast(error.message, true); }
}

async function copySecret(kind, id) {
  try {
    if (!state.secrets) state.secrets = await api("/admin/api/secrets");
    const value = kind === "provider" ? state.secrets.provider_api_keys?.[id] : state.secrets.client_tokens?.[id];
    if (!value) return showToast("当前没有已保存的密钥。", true);
    await copyText(value);
    showToast(kind === "provider" ? "API Key 已复制。" : "访问密钥已复制。")
  } catch (error) { showToast(error.message, true); }
}

async function copyText(value) {
  if (navigator.clipboard?.writeText) return navigator.clipboard.writeText(value);
  const textarea = document.createElement("textarea");
  textarea.value = value; textarea.style.position = "fixed"; textarea.style.opacity = "0";
  document.body.appendChild(textarea); textarea.select(); document.execCommand("copy"); textarea.remove();
}

async function renderUsage(groupBy) {
  const isProvider = groupBy === "provider";
  const selectorValue = isProvider ? state.platformProvider : state.usageClient;
  content.innerHTML = `${usageToolbar(groupBy, selectorValue)}<div class="loading"><span></span><p>正在汇总时间窗口内的结算记录…</p></div>`;
  bindUsageControls(groupBy);
  try {
    const filterName = isProvider ? "provider_id" : "client_id";
    const query = new URLSearchParams({ group_by: groupBy, window_value: String(state.usageRange.value), window_unit: state.usageRange.unit });
    if (selectorValue) query.set(filterName, selectorValue);
    const report = await api(`/admin/api/usage?${query}`);
    if (state.activeView !== (isProvider ? "platform_usage" : "user_usage")) return;
    content.innerHTML = `${usageToolbar(groupBy, selectorValue)}${usageReport(groupBy, report, selectorValue)}`;
    bindUsageControls(groupBy);
  } catch (error) {
    content.innerHTML = usageToolbar(groupBy, selectorValue);
    bindUsageControls(groupBy);
    showToast(error.message, true);
  }
}

function usageToolbar(groupBy, selected) {
  const isProvider = groupBy === "provider";
  const options = isProvider ? state.config.providers : state.config.clients;
  return `<div class="usage-toolbar">
    <div><h2>${isProvider ? "供应商消耗" : "访问密钥消耗"}</h2><p>平台价与实际口径使用同一个时间窗口，保证可以直接比较。</p></div>
    <div class="usage-controls">
      <select id="usage-entity"><option value="">${isProvider ? "全部供应商" : "全部访问密钥"}</option>${options.map(item => `<option value="${escapeHTML(item.id)}" ${selected === item.id ? "selected" : ""}>${escapeHTML(isProvider ? item.display_name || item.id : item.id)}</option>`).join("")}</select>
      <div class="quick-range"><button data-range="1-hours" class="${state.usageRange.value === 1 && state.usageRange.unit === "hours" ? "active" : ""}">1 小时</button><button data-range="1-days" class="${state.usageRange.value === 1 && state.usageRange.unit === "days" ? "active" : ""}">1 天</button><button data-range="7-days" class="${state.usageRange.value === 7 && state.usageRange.unit === "days" ? "active" : ""}">7 天</button></div>
      <input id="usage-window-value" type="number" min="1" value="${state.usageRange.value}" aria-label="时间范围数值">
      <select id="usage-window-unit"><option value="hours" ${state.usageRange.unit === "hours" ? "selected" : ""}>小时</option><option value="days" ${state.usageRange.unit === "days" ? "selected" : ""}>天</option></select>
    </div>
  </div>`;
}

function bindUsageControls(groupBy) {
  document.querySelector("#usage-entity")?.addEventListener("change", event => {
    if (groupBy === "provider") state.platformProvider = event.target.value; else state.usageClient = event.target.value;
    renderUsage(groupBy);
  });
  document.querySelectorAll("[data-range]").forEach(button => button.addEventListener("click", () => {
    const [value, unit] = button.dataset.range.split("-");
    state.usageRange = { value: Number(value), unit };
    renderUsage(groupBy);
  }));
  document.querySelector("#usage-window-value")?.addEventListener("change", event => {
    state.usageRange.value = Math.max(1, Number(event.target.value) || 1); renderUsage(groupBy);
  });
  document.querySelector("#usage-window-unit")?.addEventListener("change", event => {
    state.usageRange.unit = event.target.value; renderUsage(groupBy);
  });
}

function usageReport(groupBy, report, selected) {
  const configured = groupBy === "provider" ? state.config.providers : state.config.clients;
  const visible = selected ? configured.filter(item => item.id === selected) : configured;
  const reportByID = new Map((report.groups || []).map(group => [group.id, group]));
  const groups = visible.map(item => reportByID.get(item.id) || { id: item.id, runs: 0, input_tokens: 0, output_tokens: 0, public_totals: [], actual_totals: [], models: [] });
  const rows = groups.flatMap(group => (group.models || []).map(model => ({ ...model, group_id: group.id })));
  return `
    <div class="usage-summary-grid">${groups.map(group => usageSummaryCard(groupBy, group)).join("") || `<div class="empty-state">当前没有可统计对象</div>`}</div>
    <div class="section-head"><div><h2>模型明细</h2><p>只列出所选时间范围内产生已确认结算的模型。</p></div><span class="range-label">${escapeHTML(formatRange(report.since, report.until))}</span></div>
    ${usageModelsTable(groupBy, rows)}`;
}

function usageSummaryCard(groupBy, group) {
  const title = groupBy === "provider" ? providerName(group.id) : group.id;
  return `<article class="usage-card"><header><div><span class="category">${groupBy === "provider" ? "PROVIDER" : "ACCESS KEY"}</span><h3 title="${escapeHTML(title)}">${escapeHTML(title)}</h3></div><strong title="${group.runs} 次请求">${group.runs}</strong></header><div class="usage-kpis"><div><span>平台消耗</span><b>${formatUnitTotals(group.public_totals)}</b></div><div><span>实际消耗</span><b>${formatActual(group.actual_totals, groupBy === "client", groupBy === "provider" ? group.id : "")}</b></div></div><footer>${Number(group.input_tokens).toLocaleString()} 入 · ${Number(group.output_tokens).toLocaleString()} 出</footer></article>`;
}

function usageModelsTable(groupBy, rows) {
  if (!rows.length) return `<div class="empty-state">所选时间范围内没有消耗</div>`;
  return `<div class="table-card"><table><thead><tr>${groupBy === "client" ? "<th>访问密钥</th>" : ""}<th>模型</th><th>供应商</th><th>请求</th><th>输入 Token</th><th>输出 Token</th><th>平台价消耗</th><th>实际供应商消耗</th></tr></thead><tbody>${rows.map(row => `<tr>${groupBy === "client" ? `<td><code>${escapeHTML(row.group_id)}</code></td>` : ""}<td><strong>${escapeHTML(row.deployment_id)}</strong></td><td>${escapeHTML(providerName(row.provider_id))}</td><td>${row.runs}</td><td>${Number(row.input_tokens).toLocaleString()}</td><td>${Number(row.output_tokens).toLocaleString()}</td><td>${formatUnitTotals(row.public_totals)}</td><td>${formatActual(row.actual_totals, false, row.provider_id)}</td></tr>`).join("")}</tbody></table></div>`;
}

function formatUnitTotals(items = []) {
  if (!items.length) return `<span class="muted">—</span>`;
  return items.map(item => amountChip(`${item.unit} ${compactDecimal(item.total, String(item.unit).toUpperCase() === "POINTS" ? 2 : 4)}`, "", "", `${item.unit} ${item.total}`)).join("");
}

function amountChip(text, className = "", badge = "", title = text) {
  return `<span class="amount ${className}" title="${escapeHTML(title)}"><span class="amount-main">${escapeHTML(text)}</span>${badge ? `<small>${escapeHTML(badge)}</small>` : ""}</span>`;
}

function formatActual(actual = [], showProvider = false, fixedProviderID = "") {
  const providerIDs = new Set([fixedProviderID, ...actual.map(item => item.provider_id)].filter(Boolean));
  const values = [];
  for (const providerID of providerIDs) {
    const provider = byID(state.config.providers, providerID);
    const shortProvider = usageProviderName(providerID);
    const prefix = showProvider ? `${shortProvider} · ` : "";
    const providerActual = actual.filter(item => item.provider_id === providerID);
    if (provider?.type === "codex_exec") {
      continue;
    }
    if (provider?.type === "workbuddy_exec") {
      const points = providerActual.filter(item => String(item.unit).toUpperCase() === "POINTS");
      for (const item of points) {
        values.push(amountChip(`${prefix}积分 ${compactDecimal(item.total, 2)}`, item.source === "provider_reported" ? "reported" : "estimated", item.source === "provider_reported" ? "实报" : "估算", `${providerName(providerID)}：积分 ${item.total}（${item.source === "provider_reported" ? "供应商实报" : "配置估算"}）`));
      }
      if (!points.length && providerActual.length) {
        values.push(amountChip(`${prefix}无积分记录`, "unavailable", "", `${providerName(providerID)}：历史记录没有积分字段`));
      }
      continue;
    }
    for (const item of providerActual) {
      const digits = String(item.unit).toUpperCase() === "POINTS" ? 2 : 4;
      values.push(amountChip(`${prefix}${item.unit} ${compactDecimal(item.total, digits)}`, item.source === "provider_reported" ? "reported" : "estimated", item.source === "provider_reported" ? "实报" : "估算", `${providerName(providerID)}：${item.unit} ${item.total}（${item.source === "provider_reported" ? "供应商实报" : "配置估算"}）`));
    }
  }
  return values.length ? values.join(" ") : `<span class="muted">未提供</span>`;
}

function formatRange(since, until) {
  if (!since || !until) return "";
  return `${new Date(since).toLocaleString()} — ${new Date(until).toLocaleString()}`;
}

function switchView(view) { state.activeView = view; render(); }

document.querySelectorAll(".nav-item").forEach(item => item.addEventListener("click", () => switchView(item.dataset.view)));
document.querySelector("#save-all").addEventListener("click", saveAll);
document.querySelector("#apply-models").addEventListener("click", applyModels);
document.querySelector("#apply-provider").addEventListener("click", applyProvider);
document.querySelector("#delete-provider").addEventListener("click", deleteProvider);
document.querySelectorAll("[data-close]").forEach(button => button.addEventListener("click", () => document.querySelector(`#${button.dataset.close}`).close()));
providerDialog.addEventListener("click", event => {
  if (event.target.classList.contains("copy-dialog-secret")) copySecret("provider", state.providerDraft.provider.id);
});
window.addEventListener("beforeunload", event => { if (state.dirty) { event.preventDefault(); event.returnValue = ""; } });
document.querySelector("#toast-close").addEventListener("click", hideToast);
loadState().catch(error => { content.replaceChildren(); showToast(error.message, true); });
