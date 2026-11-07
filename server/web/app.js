/* ============================================================================
 * THVV Web — 前端（无构建、无依赖）
 *
 * 所有请求都打到同源的 Go 服务（cookie 会话），无前端 Mock。
 * 契约见 ../API.md 与 ../WEB-UI.md。
 * ========================================================================== */
'use strict';

const API_BASE = '/v1';

/* ---------------------------------------------------------------- 状态 */
const state = {
  user: null,
  meta: null,
  tools: [],
  buckets: { items: [], scenarios: [], default_ladder: [1, 8, 32, 64, 128], number_by_parallel: {} },
  gates: [],
  datasets: [],
  capability: 'perf',    // perf | eval | cache
  evalDatasets: [],      // 选中的评测数据集
  targets: [],
  target: null,          // 当前选中的 Target 对象
  step: 1,               // 向导步骤
  mode: 'single',        // single | matrix
  matrixBuckets: [],     // 全档模式选中的档位
  overrides: {},         // { bucket: {parallels:[], max_parallel:null, number:null} }
  run: null,
  unsub: null,
  historyCursor: null,
};

/* ---------------------------------------------------------------- 工具 */
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
const nowISO = () => new Date().toISOString().replace(/\.\d+Z$/, 'Z');
const uuid = () =>
  (crypto.randomUUID ? crypto.randomUUID()
    : 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => {
        const r = Math.random() * 16 | 0;
        return (c === 'x' ? r : (r & 0x3 | 0x8)).toString(16);
      }));

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function fmtNum(v, digits = 2) {
  if (v === null || v === undefined || Number.isNaN(v)) return '—';
  const n = Number(v);
  if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (Math.abs(n) >= 1e4) return (n / 1e3).toFixed(1) + 'k';
  return n.toFixed(digits);
}
function fmtPct(v, digits = 2) { return v === null || v === undefined ? '—' : (Number(v) * 100).toFixed(digits) + '%'; }
function fmtDur(sec) {
  if (sec === null || sec === undefined) return '—';
  if (sec < 60) return sec.toFixed(1) + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm' + Math.round(sec % 60) + 's';
  return Math.floor(sec / 3600) + 'h' + Math.round((sec % 3600) / 60) + 'm';
}
function fmtBytes(b) {
  if (!b) return '—';
  const u = ['B', 'KB', 'MB', 'GB'];
  let i = 0, n = b;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(n < 10 && i > 0 ? 1 : 0) + ' ' + u[i];
}
function toast(msg, kind = '') {
  const el = document.createElement('div');
  el.className = 'toast ' + kind;
  el.textContent = msg;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), kind === 'err' ? 7000 : 3600);
}

/* ---------------------------------------------------------------- API */
class ApiError extends Error {
  constructor(problem, status) {
    super(problem.detail || problem.title || `HTTP ${status}`);
    this.problem = problem;
    this.status = status;
  }
}

async function api(method, path, body, opts = {}) {
  const headers = { 'Accept': 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (opts.idempotencyKey) headers['Idempotency-Key'] = opts.idempotencyKey;

  const res = await fetch(API_BASE + path, {
    method,
    headers,
    credentials: 'same-origin',      // Web 走 HttpOnly cookie 会话
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (res.status === 204) return null;
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) {
    if (res.status === 401 && !location.hash.includes('login')) {
      state.user = null;
      location.hash = '#/login';
    }
    throw new ApiError(data || { title: res.statusText, detail: text }, res.status);
  }
  return data;
}

/* SSE：EventSource 自动携带 cookie 会话 */
const SSE_EVENTS = [
  'run.snapshot', 'run.started', 'step.started', 'step.progress',
  'step.finished', 'run.progress', 'run.log', 'run.gate', 'run.finished', 'error',
];

function subscribeRun(runId, onEvent, onError) {
  const es = new EventSource(`${API_BASE}/runs/${runId}/events?since=0`, { withCredentials: true });
  let closed = false;
  SSE_EVENTS.forEach(type => {
    es.addEventListener(type, ev => {
      let data = {};
      try { data = JSON.parse(ev.data || '{}'); } catch (_) { }
      onEvent(type, data);
    });
  });
  // 主动 close：否则浏览器会按 SSE 规范自动重连，与下面的重连逻辑叠加成"不停重试"
  es.onerror = () => {
    if (closed) return;
    closed = true;
    es.close();
    onError && onError();
  };
  return () => { closed = true; es.close(); };
}

/* ---------------------------------------------------------------- 档位/计划计算 */
function bucketInfo(label) {
  return state.buckets.items.find(b => b.label === label);
}
function bucketTokens(label) {
  const b = bucketInfo(label);
  return b ? b.prompt_tokens.max : 999999;
}

/** 与服务端 steps_total 同一算法（纯函数，用于提交前预估与校验） */
function computeSteps(plan) {
  // 缓存测试：单步骤
  if (plan.workload) {
    return { total: 1, perStep: [{ bucket: plan.workload }], skipped: [] };
  }
  // 效果评测：一个数据集一个步骤
  if (Array.isArray(plan.datasets)) {
    const perStep = plan.datasets.map(d => ({ bucket: d, parallel: plan.eval_batch_size || 0, number: plan.limit || 0 }));
    return { total: perStep.length, perStep, skipped: [] };
  }
  const cat = state.buckets;
  const ladder = (plan.parallel_ladder && plan.parallel_ladder.length) ? plan.parallel_ladder : cat.default_ladder;
  const perStep = [];
  const skipped = [];
  for (const label of plan.buckets) {
    const info = bucketInfo(label);
    if (!info) { skipped.push({ bucket: label, reason: 'unknown_bucket' }); continue; }
    let maxPar = info.max_parallel;
    const ov = (plan.bucket_overrides || {})[label] || {};
    if (ov.max_parallel) maxPar = Math.min(maxPar, ov.max_parallel);
    if (plan.tpm_budget > 0) maxPar = Math.min(maxPar, Math.floor(plan.tpm_budget / info.prompt_tokens.max));
    let ps = ladder.filter(p => p > 0 && p <= maxPar);
    if (Array.isArray(ov.parallels)) ps = ps.concat(ov.parallels.filter(p => p > 0 && p <= maxPar));
    ps = [...new Set(ps)].sort((a, b) => a - b);
    if (!ps.length) { skipped.push({ bucket: label, reason: 'budget_too_low', max_parallel: maxPar }); continue; }
    ps.forEach(p => {
      // 请求数优先级必须与服务端 domain.ComputeSteps 一致
      const n = plan.number > 0 ? plan.number
        : ov.number > 0 ? ov.number
        : (cat.number_by_parallel[String(p)] ?? 100);
      perStep.push({ bucket: label, parallel: p, number: n });
    });
  }
  return { total: perStep.length, perStep, skipped };
}

/* ---------------------------------------------------------------- 路由 */
const ROUTES = ['login', 'new', 'run', 'history', 'catalog'];

function parseHash() {
  const raw = (location.hash || '#/new').replace(/^#\/?/, '');
  const [name, ...rest] = raw.split('/');
  return { name: ROUTES.includes(name) ? name : 'new', params: rest };
}

async function router() {
  const { name, params } = parseHash();

  if (name === 'login' || !state.user) {
    showView('login'); return;
  }
  // 离开运行页时关闭 SSE，避免后台长连接
  if (name !== 'run' && state.unsub) { state.unsub(); state.unsub = null; }

  showView(name);
  $$('#nav a').forEach(a => a.classList.toggle('active', a.dataset.route === name));

  if (name === 'new') initWizard();
  if (name === 'run') await openRun(params[0]);
  if (name === 'history') await loadHistory(true);
  if (name === 'catalog') renderCatalog();
}

function showView(name) {
  $$('.view').forEach(v => v.hidden = true);
  $('#view-' + name).hidden = false;
  const hideChrome = name === 'login';
  $('#nav').style.visibility = hideChrome ? 'hidden' : 'visible';
  $('#btn-logout').hidden = hideChrome;
  $('#server-badge').hidden = hideChrome;
}
window.addEventListener('hashchange', router);

/* ---------------------------------------------------------------- 启动 */
/** 拉取参考数据（工具/档位/阈值/端点）。登录后与启动时各调一次。 */
async function loadReferenceData() {
  const [meta, tools, buckets, gates, datasets, targets] = await Promise.all([
    api('GET', '/meta').catch(() => null),
    api('GET', '/tools').catch(() => ({ items: [] })),
    api('GET', '/catalog/buckets').catch(() => null),
    api('GET', '/catalog/gates').catch(() => ({ items: [] })),
    api('GET', '/catalog/datasets').catch(() => ({ items: [] })),
    api('GET', '/targets').catch(() => ({ items: [] })),
  ]);
  state.meta = meta;
  state.tools = tools.items || [];
  if (buckets) state.buckets = buckets;
  state.gates = gates.items || [];
  state.datasets = datasets.items || [];
  state.targets = targets.items || [];
  if (meta) setServerBadge(true, meta);
}

async function boot() {
  try {
    state.user = await api('GET', '/auth/session');
  } catch (e) {
    state.user = null;
  }

  // 已登录才拉业务数据（未登录时这些请求会 401）
  if (state.user) {
    try {
      await loadReferenceData();
    } catch (e) {
      setServerBadge(false);
    }
  }
  router();
}

function setServerBadge(ok, meta) {
  const el = $('#server-badge');
  el.hidden = false;
  if (ok) {
    el.className = 'badge ok';
    el.textContent = `服务正常 · v${meta?.server?.version || '1.0.0'}`;
  } else {
    el.className = 'badge err';
    el.textContent = '服务不可用';
  }
}

/* ---------------------------------------------------------------- 登录 */
$('#login-form')?.addEventListener('submit', async e => {
  e.preventDefault();
  const btn = e.target.querySelector('button');
  btn.disabled = true; btn.textContent = '登录中…';
  try {
    await api('POST', '/auth/login', {
      username: $('#login-user').value.trim(),
      password: $('#login-pass').value,
    });
    state.user = await api('GET', '/auth/session');
    await loadReferenceData();
    state.step = 1;
    location.hash = '#/new';
    initWizard();
  } catch (err) {
    $('#login-hint').textContent = err.message || '登录失败';
    $('#login-hint').style.color = 'var(--err)';
  } finally {
    btn.disabled = false; btn.textContent = '登录';
  }
});

$('#btn-logout')?.addEventListener('click', async () => {
  try { await api('POST', '/auth/logout'); } catch (_) { }
  state.user = null;
  location.hash = '#/login';
});

/* ================================================================
 * 向导
 * ============================================================== */
function initWizard() {
  if (!state.buckets.items.length && state.user) {
    loadReferenceData().then(() => { fillBucketSelects(); fillGateSelect(); renderOverrideTable(); updateEstimate(); });
  }
  fillExistingTargets();
  fillBucketSelects();
  fillGateSelect();
  renderOverrideTable();
  renderEvalDatasets();
  syncModeUI();
  syncCapabilityUI();
  goStep(state.step);
}

function goStep(n) {
  state.step = n;
  [1, 2, 3].forEach(i => {
    $('#panel-step-' + i).hidden = i !== n;
    const li = $(`.steps-bar li[data-step="${i}"]`);
    li.classList.toggle('active', i === n);
    li.classList.toggle('done', i < n);
  });
  if (n === 3) renderConfirm();
  window.scrollTo({ top: 0, behavior: 'smooth' });
}

function fillExistingTargets() {
  const sel = $('#target-existing');
  if (!sel) return;
  sel.innerHTML = '<option value="">— 新建端点 —</option>' +
    state.targets.map(t => `<option value="${esc(t.target_id)}">${esc(t.name)} · ${esc(t.model)}</option>`).join('');
}

$('#target-existing')?.addEventListener('change', e => {
  const id = e.target.value;
  if (!id) { state.target = null; clearTargetForm(); return; }
  const t = state.targets.find(x => x.target_id === id);
  if (!t) return;
  state.target = t;
  $('#f-vendor').value = t.vendor || '';
  $('#f-name').value = t.name || '';
  $('#f-api-url').value = t.api_url || '';
  $('#f-protocol').value = t.protocol || 'openai';
  $('#f-model').value = t.model || '';
  $('#f-api-key').value = '';
  $('#f-api-key').placeholder = '（已保存，留空则不修改）';
  $('#f-tokenizer').value = t.tokenizer?.value || '';
  $('#f-max-tokens').value = t.defaults?.max_tokens ?? 4096;
  const temp = t.defaults?.temperature;
  $('#f-temperature').value = temp === 'auto' || temp === undefined ? 'auto' : String(temp);
  $('#tok-hint').textContent = t.tokenizer
    ? `当前：${t.tokenizer.value}（${t.tokenizer.source}${t.tokenizer.cached ? '，已缓存' : ''}）`
    : '按模型名自动匹配';
  toast('已载入端点：' + t.name);
});

function clearTargetForm() {
  ['#f-vendor', '#f-name', '#f-api-url', '#f-model', '#f-api-key', '#f-tokenizer'].forEach(s => { $(s).value = ''; });
  $('#f-protocol').value = 'openai';
  $('#f-temperature').value = 'auto';
  $('#f-max-tokens').value = 4096;
  $('#f-api-key').placeholder = 'sk-...';
  $('#probe-result').hidden = true;
}

/* 协议 → URL 后缀提示 */
$('#f-protocol')?.addEventListener('change', e => {
  $('#url-hint').textContent = e.target.value === 'anthropic'
    ? 'Anthropic Messages 协议，完整路径形如 https://host/v1/messages'
    : 'OpenAI 兼容协议，完整路径形如 https://host/v1/chat/completions';
});

/* ---------- 预检 ---------- */
$('#btn-probe')?.addEventListener('click', async e => {
  const btn = e.target;
  const payload = readTargetForm();
  if (!validateTargetForm(payload)) return;
  btn.disabled = true; btn.innerHTML = '<span class="spin"></span>预检中…';
  try {
    // 没有 api_key 且已选已有端点 → 直接对该端点预检
    if (!payload.api_key && state.target) {
      $('#probe-result').hidden = false;
      renderProbe(null, 'running');
      const report = await api('POST', `/targets/${state.target.target_id}/probe`, { timeout_sec: 30 });
      renderProbe(report);
    } else {
      const t = await api('POST', '/targets', payload, { idempotencyKey: uuid() });
      state.target = t;
      state.targets.unshift(t);
      fillExistingTargets();
      $('#target-existing').value = t.target_id;
      $('#f-api-key').value = '';
      $('#f-api-key').placeholder = '（已保存，留空则不修改）';
      $('#key-hint').textContent = '已保存：' + t.api_key_hint;
      if (t.tokenizer) $('#tok-hint').textContent = `推断结果：${t.tokenizer.value}（${t.tokenizer.source}）`;

      $('#probe-result').hidden = false;
      renderProbe(null, 'running');
      const report = await api('POST', `/targets/${t.target_id}/probe`, { timeout_sec: 30 });
      renderProbe(report);
    }
  } catch (err) {
    renderProbe(null, 'error', err);
    toast(err.message, 'err');
  } finally {
    btn.disabled = false; btn.textContent = '保存并预检';
  }
});

function renderProbe(report, phase, err) {
  const box = $('#probe-result');
  box.hidden = false;
  if (phase === 'running') { box.innerHTML = '<div class="row"><span class="spin"></span>正在预检…</div>'; return; }
  if (phase === 'error') {
    box.innerHTML = `<div class="row"><span class="ico" style="color:var(--err)">✕</span>预检失败：${esc(err?.message || '')}</div>`;
    return;
  }
  const rows = (report.checks || []).map(c => {
    const sev = c.ok ? 'ok' : (c.severity === 'warn' ? 'warn' : 'err');
    const ico = c.ok ? '✓' : (sev === 'warn' ? '!' : '✕');
    const color = sev === 'ok' ? 'var(--ok)' : sev === 'warn' ? 'var(--warn)' : 'var(--err)';
    return `<div class="row"><span class="ico" style="color:${color}">${ico}</span>
      <b style="min-width:96px">${esc(c.name)}</b><span class="muted">${esc(c.detail || '')}</span></div>`;
  }).join('');
  box.innerHTML = `
    <div class="row"><span class="ico" style="color:${report.ok ? 'var(--ok)' : 'var(--warn)'}">${report.ok ? '✓' : '!'}</span>
      <b>${report.ok ? '预检通过' : '预检有警告/report.ok=false'}</b>
      <span class="muted">耗时 ${report.elapsed_ms}ms${report.engine ? ' · ' + esc(report.engine.name) + ' ' + esc(report.engine.evalscope || '') : ''}</span>
    </div>${rows}`;
}

function readTargetForm() {
  const tempRaw = $('#f-temperature').value;
  return {
    name: $('#f-name').value.trim() || autoName(),
    vendor: $('#f-vendor').value.trim() || undefined,
    api_url: $('#f-api-url').value.trim(),
    protocol: $('#f-protocol').value,
    model: $('#f-model').value.trim(),
    api_key: $('#f-api-key').value.trim() || undefined,
    tokenizer: $('#f-tokenizer').value.trim() || null,
    defaults: {
      temperature: tempRaw === 'auto' ? 'auto' : Number(tempRaw),
      max_tokens: Number($('#f-max-tokens').value) || 4096,
    },
  };
}

function autoName() {
  const m = $('#f-model').value.trim();
  const v = $('#f-vendor').value.trim();
  if (!m) return '';
  return (v ? v + '-' : '') + m.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
}

function validateTargetForm(p) {
  const errs = [];
  if (!p.vendor) errs.push('测试名称必填');
  if (!/^https?:\/\//.test(p.api_url)) errs.push('API 地址必须以 http(s):// 开头');
  if (!p.model) errs.push('模型名必填');
  if (!state.target && !p.api_key) errs.push('新建端点时必须填写 API Key');
  if (p.protocol === 'openai' && !/\/chat\/completions$/.test(p.api_url)) errs.push('OpenAI 协议地址应以 /chat/completions 结尾');
  if (p.protocol === 'anthropic' && !/\/messages$/.test(p.api_url)) errs.push('Anthropic 协议地址应以 /messages 结尾');
  if (errs.length) { toast(errs[0], 'err'); return false; }
  return true;
}

/* ---------- 步骤 1 → 2 ---------- */
$('#btn-to-step2')?.addEventListener('click', async () => {
  if (!state.target) {
    toast('请先「保存并预检」以登记端点', 'err');
    return;
  }
  goStep(2);
});

/* ---------- 步骤 2：档位与模式 ---------- */
function fillBucketSelects() {
  const builtIn = state.buckets.items.filter(b => b.built_in);
  $('#s-bucket').innerHTML = builtIn
    .map(b => `<option value="${b.label}">${b.label} · ${b.prompt_tokens.min}~${b.prompt_tokens.max} tokens</option>`)
    .join('');
  if (builtIn[0]) state.matrixBuckets = builtIn.map(b => b.label);
  renderBucketChips();
  updateSingleHints();
}

function renderBucketChips() {
  const box = $('#m-buckets');
  box.innerHTML = state.buckets.items.map(b => {
    const on = state.matrixBuckets.includes(b.label);
    const cls = !b.built_in ? 'chip off' : (on ? 'chip on' : 'chip');
    const title = b.built_in ? '' : '数据集未构建，需先离线生成';
    return `<span class="${cls}" data-bucket="${b.label}" title="${title}">${b.label}</span>`;
  }).join('');
}

$('#m-buckets')?.addEventListener('click', e => {
  const chip = e.target.closest('.chip');
  if (!chip || chip.classList.contains('off')) return;
  const label = chip.dataset.bucket;
  const i = state.matrixBuckets.indexOf(label);
  if (i >= 0) state.matrixBuckets.splice(i, 1); else state.matrixBuckets.push(label);
  chip.classList.toggle('on');
  renderOverrideTable();
  updateEstimate();
});

function syncCapabilityUI() {
  $$('#cap-seg .seg-item').forEach(b => b.classList.toggle('active', b.dataset.cap === state.capability));
  $('#pane-perf').hidden = state.capability !== 'perf';
  $('#pane-eval').hidden = state.capability !== 'eval';
  $('#pane-cache').hidden = state.capability !== 'cache';
  $('#r-formats-field').hidden = state.capability !== 'perf';
  $('#g-tpm').closest('.field').hidden = state.capability !== 'perf';
  updateEstimate();
  updateEvalEstimate();
  updateCacheEstimate();
}

$$('#cap-seg .seg-item').forEach(btn => btn.addEventListener('click', () => {
  state.capability = btn.dataset.cap;
  fillGateSelect();
  syncCapabilityUI();
}));

function renderEvalDatasets() {
  const box = $('#e-datasets');
  if (!box) return;
  if (!state.datasets.length) {
    box.innerHTML = '<span class="muted">服务端未启用评测数据集</span>';
    return;
  }
  box.innerHTML = state.datasets.map(d => {
    const on = state.evalDatasets.includes(d.name);
    const tags = [d.needs_judge ? 'Judge' : '', d.needs_docker ? 'Docker' : ''].filter(Boolean).join(' / ');
    const title = [d.display_name, d.kind, tags].filter(Boolean).join(' · ');
    return `<span class="chip${on ? ' on' : ''}" data-ds="${esc(d.name)}" title="${esc(title)}">${esc(d.name)}${tags ? `<small> (${esc(tags)})</small>` : ''}</span>`;
  }).join('');
}

$('#e-datasets')?.addEventListener('click', e => {
  const chip = e.target.closest('.chip');
  if (!chip) return;
  const name = chip.dataset.ds;
  const i = state.evalDatasets.indexOf(name);
  if (i >= 0) state.evalDatasets.splice(i, 1); else state.evalDatasets.push(name);
  chip.classList.toggle('on');
  updateEvalEstimate();
});

function evalDatasetInfo(name) { return state.datasets.find(d => d.name === name); }

function updateEvalEstimate() {
  const box = $('#e-estimate');
  if (!box) return;
  if (state.capability !== 'eval') { box.hidden = true; return; }
  box.hidden = false;
  if (!state.evalDatasets.length) { box.innerHTML = '请至少选择一个数据集'; return; }
  const needJudge = state.evalDatasets.some(n => evalDatasetInfo(n)?.needs_judge);
  const needDocker = state.evalDatasets.filter(n => evalDatasetInfo(n)?.needs_docker);
  box.innerHTML = `本次共 <b>${state.evalDatasets.length}</b> 个数据集：${state.evalDatasets.map(esc).join('、')}。
    ${needJudge ? '<br>⚠ 含需要 LLM Judge 的数据集，必须在下方填 Judge API Key。' : ''}
    ${needDocker.length ? `<br>⚠ ${needDocker.map(esc).join('、')} 需要宿主安装 Docker + swebench/ms-enclave。` : ''}`;
}

function syncModeUI() {
  $$('#mode-seg .seg-item').forEach(b => b.classList.toggle('active', b.dataset.mode === state.mode));
  $('#pane-single').hidden = state.mode !== 'single';
  $('#pane-matrix').hidden = state.mode !== 'matrix';
  updateEstimate();
}

$$('#mode-seg .seg-item').forEach(btn => btn.addEventListener('click', () => {
  state.mode = btn.dataset.mode;
  syncModeUI();
}));

function updateSingleHints() {
  const b = bucketInfo($('#s-bucket').value);
  if (!b) return;
  $('#s-bucket-hint').textContent = `并发上限 ${b.max_parallel}；该档数据集 ${b.built_in ? '已就绪' : '未构建'}`;
  const p = Number($('#s-parallel').value) || 1;
  $('#s-parallel-hint').textContent = p > b.max_parallel
    ? `⚠ 超过该档上限 ${b.max_parallel}，服务端会裁剪`
    : `约 ${p * bucketTokens(b.label)} tokens/min 瞬时压力`;
}
$('#s-bucket')?.addEventListener('change', updateSingleHints);
$('#s-parallel')?.addEventListener('change', updateSingleHints);

/* ---------- 全档覆盖表 ---------- */
function renderOverrideTable() {
  const tbody = $('#override-table tbody');
  if (!tbody) return;
  tbody.innerHTML = state.matrixBuckets.map(label => {
    const info = bucketInfo(label);
    if (!info) return '';
    const ov = state.overrides[label] || {};
    return `<tr data-bucket="${esc(label)}">
      <td><b>${esc(label)}</b></td>
      <td class="num">${info.max_parallel}</td>
      <td><input data-k="parallels" placeholder="如 8,16" value="${esc((ov.parallels || []).join(','))}"></td>
      <td><input data-k="number" type="number" min="1" placeholder="默认" value="${ov.number ?? ''}"></td>
    </tr>`;
  }).join('');
}

$('#override-table')?.addEventListener('change', e => {
  const row = e.target.closest('tr');
  if (!row) return;
  const label = row.dataset.bucket;
  const ov = state.overrides[label] || (state.overrides[label] = {});
  const k = e.target.dataset.k;
  if (k === 'parallels') {
    ov.parallels = e.target.value.split(',').map(s => Number(s.trim())).filter(n => n > 0);
  } else if (k === 'number') {
    ov.number = e.target.value ? Number(e.target.value) : null;
  }
  updateEstimate();
});

/* ---------- 计划组装 ---------- */
function evalPlan() {
  const num = (sel) => Number($(sel).value) || 0;
  const plan = {
    mode: 'single',
    datasets: [...state.evalDatasets],
    limit: num('#e-limit'),
    repeats: num('#e-repeats'),
    eval_batch_size: num('#e-batch'),
    thinking: $('#e-thinking').value === 'true',
    provider_name: $('#e-provider').value.trim() || undefined,
  };
  const mt = num('#e-max-tokens');
  if (mt) plan.request = { max_tokens: mt };
  const jm = $('#e-judge-model').value.trim();
  const ju = $('#e-judge-url').value.trim();
  const jk = $('#e-judge-key').value.trim();
  if (jm || ju || jk) {
    plan.judge = {};
    if (jm) plan.judge.model = jm;
    if (ju) plan.judge.base_url = ju;
    if (jk) plan.judge.api_key = jk;
  }
  return plan;
}

function cachePlan() {
  const num = (sel) => Number($(sel).value) || 0;
  const seed = num('#c-seed');
  return {
    mode: 'single',
    workload: 'multi-round-chat',
    duration_sec: num('#c-duration'),
    qps: Number($('#c-qps').value) || 1,
    kv_cache_volume_gb: Number($('#c-volume').value) || 0,
    tokens_per_gb_kvcache: num('#c-tpg'),
    shared_prompt_length: num('#c-shared'),
    chat_history_length: num('#c-history'),
    user_input_length: num('#c-input'),
    output_length: num('#c-output'),
    no_warmup: $('#c-warmup').value === 'false',
    ...(seed ? { seed } : {}),
  };
}

function updateCacheEstimate() {
  const box = $('#c-estimate');
  if (!box) return;
  if (state.capability !== 'cache') { box.hidden = true; return; }
  box.hidden = false;
  const plan = cachePlan();
  const perSession = plan.shared_prompt_length + plan.chat_history_length;
  const users = perSession > 0 ? Math.floor(plan.kv_cache_volume_gb * plan.tokens_per_gb_kvcache / perSession) : 0;
  const estTokens = Math.round(plan.qps * plan.duration_sec * (perSession + plan.output_length));
  box.innerHTML = `并发 session 数约 <b>${Math.max(1, users)}</b>（由 KV 容量推导）；` +
    `预计消耗约 <b>${estTokens.toLocaleString()}</b> token（QPS × 时长 × 单请求 token）。` +
    (plan.no_warmup ? '<br>⚠ 预热已关闭：为冷启动口径，验收不判改善率。' : '');
}

function buildPlan() {
  if (state.capability === 'eval') return evalPlan();
  if (state.capability === 'cache') return cachePlan();
  const base = {
    dataset_dir: 'auto',
    cooldown_sec: Number($('#m-cooldown').value) || 0,
    success_rate_min: Number($('#m-srmin').value) || 0,
    tpm_budget: Number($('#m-tpm').value) || 0,
    stop_on_failure: $('#m-stop').value === 'true',
    request: {
      temperature: state.target?.defaults?.temperature ?? 'auto',
      max_tokens: state.target?.defaults?.max_tokens ?? 4096,
      max_turns: 1,
    },
  };

  if (state.mode === 'single') {
    const bucket = $('#s-bucket').value;
    const parallel = Number($('#s-parallel').value) || 1;
    return {
      mode: 'single',
      buckets: [bucket],
      parallel_ladder: [parallel],
      number: Number($('#s-number').value) || 200,
      ...base,
    };
  }

  const ladder = $('#m-ladder').value.split(',')
    .map(s => Number(s.trim())).filter(n => n > 0);
  const overrides = {};
  for (const [k, v] of Object.entries(state.overrides)) {
    if (!state.matrixBuckets.includes(k)) continue;
    const clean = {};
    if (Array.isArray(v.parallels) && v.parallels.length) clean.parallels = v.parallels;
    if (v.number) clean.number = v.number;
    if (Object.keys(clean).length) overrides[k] = clean;
  }
  return {
    mode: 'matrix',
    buckets: [...state.matrixBuckets],
    parallel_ladder: ladder.length ? ladder : state.buckets.default_ladder,
    ...(Object.keys(overrides).length ? { bucket_overrides: overrides } : {}),
    ...base,
  };
}

function updateEstimate() {
  const box = $('#estimate');
  if (!box) return;
  if (state.capability === 'eval' || state.mode === 'single') { box.hidden = true; return; }
  box.hidden = false;
  const plan = buildPlan();
  if (!plan.buckets.length) { box.innerHTML = '请至少选择一个档位'; return; }
  const { total, perStep, skipped } = computeSteps(plan);
  const requests = perStep.reduce((s, x) => s + x.number, 0);
  const byBucket = {};
  perStep.forEach(s => { byBucket[s.bucket] = (byBucket[s.bucket] || []).concat(s.parallel); });
  const jumps = Object.keys(byBucket).length > 1 ? (Object.keys(byBucket).length - 1) * plan.cooldown_sec : 0;
  box.innerHTML = `
    本次共 <b>${total}</b> 个执行步骤，约 <b>${requests}</b> 条请求${jumps ? `，档间冷却合计 <b>${fmtDur(jumps)}</b>` : ''}。
    实际耗时取决于被测端吞吐，全档位常达数小时（含冷却是串行执行）。
    ${skipped.length ? `<br>⚠ 跳过：${skipped.map(s => esc(s.bucket) + '(' + esc(s.reason) + ')').join('、')}` : ''}
    ${skipped.some(s => s.reason === 'budget_too_low') ? '<br>提示：TPM 预算过小，部分档位连最低并发都超预算。' : ''}`;
}

['#m-ladder', '#m-cooldown', '#m-srmin', '#m-tpm', '#m-stop'].forEach(sel => {
  $(sel)?.addEventListener('input', updateEstimate);
  $(sel)?.addEventListener('change', updateEstimate);
});

function fillGateSelect() {
  const sel = $('#g-profile');
  const list = state.gates.filter(g => (g.capability || 'perf') === (state.capability || 'perf'));
  sel.innerHTML = '<option value="">不判定</option>' +
    list.map(g => `<option value="${esc(g.profile)}">${esc(g.profile)} — ${esc(g.description || '')}</option>`).join('');
  if (list[0]) sel.value = list[0].profile;
  else if (state.gates[0]) sel.value = state.gates[0].profile;
}

/* ---------- 步骤 2 → 3 ---------- */
$('#btn-back-1')?.addEventListener('click', () => goStep(1));
$('#btn-to-step3')?.addEventListener('click', () => {
  if (state.capability === 'eval') {
    if (!state.evalDatasets.length) { toast('请至少选择一个数据集', 'err'); return; }
    const needJudge = state.evalDatasets.some(n => evalDatasetInfo(n)?.needs_judge);
    if (needJudge && !$('#e-judge-key').value.trim()) { toast('所选数据集需要 LLM Judge，请填写 Judge API Key', 'err'); return; }
  } else if (state.capability === 'cache') {
    const dur = Number($('#c-duration').value) || 0;
    if (dur < 10 || dur > 3600) { toast('测试时长需在 10~3600 秒', 'err'); return; }
  } else if (state.mode === 'matrix' && !state.matrixBuckets.length) {
    toast('请至少选择一个档位', 'err'); return;
  }
  goStep(3);
});
$('#btn-back-2')?.addEventListener('click', () => goStep(2));

/* ---------- 确认页 ---------- */
function buildSubmitPayload() {
  const plan = buildPlan();
  const formats = [];
  if ($('#r-html').checked) formats.push('html');
  if ($('#r-xlsx').checked) formats.push('xlsx');
  const payload = {
    tool: state.capability === 'cache' ? 'lmcache' : 'thvv',
    capability: state.capability,
    target_id: state.target?.target_id,
    plan,
    report: { formats, client_name: $('#r-client').value.trim() || undefined, title: $('#r-title').value.trim() || undefined },
  };
  if (state.capability === 'perf') {
    const profile = $('#g-profile').value;
    if (profile) {
      payload.gate = { profile, mode: $('#g-mode').value, params: { user_tpm_quota: Number($('#g-tpm').value) || 0 } };
    }
  } else {
    // eval/cache：默认启用各自 SLA 模板；提交「不判定」则不传 gate
    const profile = $('#g-profile').value;
    if (profile) payload.gate = { profile, mode: 'report_only' };
  }
  return payload;
}

function renderConfirm() {
  const t = state.target || {};
  $('#cf-target').innerHTML = [
    ['名称', t.name], ['测试名称', t.vendor || '—'], ['模型', t.model],
    ['协议', t.protocol], ['地址', t.api_url],
    ['Key', t.api_key_hint || '—'],
    ['Tokenizer', t.tokenizer?.value ? `${t.tokenizer.value}（${t.tokenizer.source}）` : '自动'],
  ].map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v ?? '—')}</dd>`).join('');

  const payload = buildSubmitPayload();
  const { total } = computeSteps(payload.plan);
  let rows;
  if (state.capability === 'cache') {
    const plan = payload.plan;
    const perSession = plan.shared_prompt_length + plan.chat_history_length;
    const users = perSession > 0 ? Math.max(1, Math.floor(plan.kv_cache_volume_gb * plan.tokens_per_gb_kvcache / perSession)) : '—';
    rows = [
      ['能力', '缓存测试（multi-round-chat）'],
      ['时长 / QPS', `${plan.duration_sec}s / ${plan.qps}`],
      ['并发 session', `${users}（容量推导）`],
      ['Prompt / 历史 / 输入 / 输出', `${plan.shared_prompt_length} / ${plan.chat_history_length} / ${plan.user_input_length} / ${plan.output_length} token`],
      ['预热', plan.no_warmup ? '关闭（冷启动口径）' : '开启'],
      ['验收', payload.gate ? `${payload.gate.profile}（稳态 TTFT + 改善率）` : '不判定'],
      ['执行步骤', total],
      ['产物', 'CSV / JSON / 配置 / run.log'],
    ];
    $('#cf-plan').innerHTML = rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('');
    $('#cf-json').textContent = JSON.stringify(payload, null, 2);
    return;
  }
  if (state.capability === 'eval') {
    const plan = payload.plan;
    rows = [
      ['能力', '效果评测'],
      ['数据集', (plan.datasets || []).join(' / ')],
      ['每集条数', plan.limit || '全量'],
      ['repeats', plan.repeats || '数据集默认'],
      ['并发', plan.eval_batch_size || '数据集默认'],
      ['思考模式', plan.thinking ? '开启' : '关闭'],
      ['max_tokens', plan.request?.max_tokens || '自动'],
      ['Judge', plan.judge?.model ? `${plan.judge.model}${plan.judge.api_key ? '（已填 Key）' : ''}` : '不需要'],
      ['验收', payload.gate ? `${payload.gate.profile}（分数下限）` : '不判定'],
      ['执行步骤', total],
      ['产物', 'HTML / JSON / CSV / tar.gz'],
    ];
    $('#cf-plan').innerHTML = rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('');
    $('#cf-json').textContent = JSON.stringify(payload, null, 2);
    return;
  }
  rows = [['模式', payload.plan.mode === 'single' ? '单档测试' : '全档位矩阵']];
  if (payload.plan.mode === 'single') {
    rows.push(['档位', payload.plan.buckets[0]]);
    rows.push(['并发', payload.plan.parallel_ladder[0]]);
    rows.push(['请求数', payload.plan.number]);
  } else {
    rows.push(['档位', payload.plan.buckets.join(' / ')]);
    rows.push(['并发梯度', payload.plan.parallel_ladder.join(', ')]);
    rows.push(['冷却', payload.plan.cooldown_sec + 's']);
    rows.push(['TPM 预算', payload.plan.tpm_budget || '不限']);
  }
  rows.push(['执行步骤', total]);
  rows.push(['验收门槛', payload.gate ? `${payload.gate.profile}（${payload.gate.mode}）` : '不判定']);
  rows.push(['产物', (payload.report.formats.join(' + ') || '无')]);
  $('#cf-plan').innerHTML = rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('');
  $('#cf-json').textContent = JSON.stringify(payload, null, 2);
}

/* ---------- 提交 ---------- */
$('#btn-submit')?.addEventListener('click', async e => {
  const btn = e.target;
  const payload = buildSubmitPayload();
  btn.disabled = true; btn.innerHTML = '<span class="spin"></span>提交中…';
  try {
    const run = await api('POST', '/runs', payload, { idempotencyKey: uuid() });
    toast('已提交，任务 ' + run.run_id);
    location.hash = '#/run/' + run.run_id;
  } catch (err) {
    toast(err.message, 'err');
    if (err.problem?.details) console.warn(err.problem);
  } finally {
    btn.disabled = false; btn.textContent = '开始测试';
  }
});

/* ================================================================
 * 运行监控
 * ============================================================== */
const TERMINAL = ['succeeded', 'partial', 'failed', 'canceled'];

async function openRun(runId) {
  if (!runId) { location.hash = '#/new'; return; }
  if (state.unsub) { state.unsub(); state.unsub = null; }
  state.run = null;
  $('#run-result').hidden = true;
  $('#steps-table tbody').innerHTML = '';
  $('#run-log').textContent = '';
  $('#run-log').hidden = true;
  $('#btn-toggle-log').textContent = '显示日志';
  $('#run-kpis').innerHTML = '';
  setRunTitle(null);

  try {
    const run = await api('GET', `/runs/${runId}?include=steps`);
    state.run = run;
    renderRun(run);
  } catch (err) {
    toast(err.message, 'err');
    location.hash = '#/history';
    return;
  }

  if (!TERMINAL.includes(state.run.status)) {
    state.unsub = subscribeRun(runId, handleEvent, async () => {
      // 断线后先确认 run 是否已结束：终态就直接拉结果，不再重连
      try {
        const r = await api('GET', `/runs/${runId}?include=steps`);
        if (TERMINAL.includes(r.status)) {
          state.run = r;
          renderRun(r);
          await loadRunResult(runId);
          return;
        }
      } catch (_) { /* 网络仍不可用时继续重试 */ }
      toast('事件流断开，正在尝试重连…', 'err');
      setTimeout(() => { if ((location.hash || '').includes(runId)) openRun(runId); }, 5000);
    });
  } else {
    await loadRunResult(runId);
  }
}

function setRunTitle(run) {
  if (!run) { $('#run-title').textContent = '运行中…'; $('#run-meta').textContent = ''; return; }
  const cap = run.capability === 'perf' ? '性能压测' : run.capability === 'eval' ? '效果评测' : run.capability === 'cache' ? '缓存测试' : '功能测试';
  $('#run-title').textContent = `${run.target?.model || ''} · ${cap}`;
  const plan = run.plan || {};
  const scope = run.capability === 'eval'
    ? `数据集 ${(plan.datasets || []).length} 个：${(plan.datasets || []).join(', ')}`
    : run.capability === 'cache'
      ? `${plan.workload || 'multi-round-chat'} · ${plan.duration_sec ?? '—'}s / ${plan.qps ?? '—'} QPS`
      : (plan.mode === 'single'
      ? `单档 ${plan.buckets?.[0] || ''} / 并发 ${plan.parallel_ladder?.[0] ?? ''} / ${plan.number ?? ''} 条`
      : `全档 ${plan.buckets?.length || 0} 个档位 / 梯度 ${(plan.parallel_ladder || []).join(',')}`);
  $('#run-meta').textContent = `${run.run_id} · ${scope} · 创建于 ${new Date(run.created_at).toLocaleString()}`;
}

function renderRun(run) {
  setRunTitle(run);
  const pill = $('#run-status');
  pill.textContent = statusText(run.status);
  pill.className = 'status-pill ' + run.status;
  $('#btn-cancel').disabled = TERMINAL.includes(run.status);

  const p = run.progress || {};
  const total = p.steps_total || run.plan?.steps_total || 0;
  const done = p.steps_done || 0;
  const pct = total ? Math.min(100, (done / total) * 100) : (TERMINAL.includes(run.status) ? 100 : 2);
  $('#run-progress-bar').style.width = pct + '%';
  $('#run-progress-text').textContent = `${done} / ${total} 步${p.current ? ` · 正在跑 ${p.current.bucket} / 并发 ${p.current.parallel}` : ''}`;
  $('#run-progress-eta').textContent = p.eta_sec ? `预计剩余 ${fmtDur(p.eta_sec)}` : (p.elapsed_sec ? `已用 ${fmtDur(p.elapsed_sec)}` : '');

  if (run.steps) renderSteps(run.steps, run.capability);
}

function renderStepsHead(cap) {
  const el = $('#steps-head');
  if (!el) return;
  el.innerHTML = cap === 'eval'
    ? '<tr><th>#</th><th>数据集</th><th>并发</th><th>条数</th><th>状态</th><th class="num">得分</th><th class="num">耗时</th></tr>'
    : cap === 'cache'
      ? '<tr><th>#</th><th>Workload</th><th>并发</th><th>请求数</th><th>状态</th><th class="num">成功率</th><th class="num">耗时</th></tr>'
      : '<tr><th>#</th><th>档位</th><th>并发</th><th>请求数</th><th>状态</th>'
        + '<th class="num">成功率</th><th class="num">TTFT P50</th><th class="num">Output tps</th><th class="num">耗时</th></tr>';
}

function statusText(s) {
  return ({
    queued: '排队中', preparing: '准备中', running: '运行中', reporting: '生成报告中',
    succeeded: '已完成', partial: '部分完成', failed: '失败', canceled: '已取消',
  })[s] || s;
}

function renderSteps(steps, capability) {
  const cap = capability || state.run?.capability || 'perf';
  renderStepsHead(cap);
  const tbody = $('#steps-table tbody');
  if (cap === 'eval') {
    tbody.innerHTML = steps.map((s, i) => `
      <tr data-step="${i}">
        <td class="muted">${i + 1}</td>
        <td><b>${esc(s.bucket || '—')}</b></td>
        <td class="num">${s.parallel || '默认'}</td>
        <td class="num">${s.number || '全量'}</td>
        <td class="st">${stepBadge(s.status)}</td>
        <td class="num sr">${s.score != null ? fmtNum(s.score) : '—'}</td>
        <td class="num dur">${s.duration_sec != null ? fmtDur(s.duration_sec) : '—'}</td>
      </tr>`).join('');
    return;
  }
  if (cap === 'cache') {
    tbody.innerHTML = steps.map((s, i) => `
      <tr data-step="${i}">
        <td class="muted">${i + 1}</td>
        <td><b>${esc(s.bucket || '—')}</b></td>
        <td class="num">${s.parallel ? s.parallel + ' session' : '—'}</td>
        <td class="num">${s.number || '—'} 请求</td>
        <td class="st">${stepBadge(s.status)}</td>
        <td class="num">${s.success_rate != null ? fmtPct(s.success_rate) : '—'}</td>
        <td class="num dur">${s.duration_sec != null ? fmtDur(s.duration_sec) : '—'}</td>
      </tr>`).join('');
    return;
  }
  tbody.innerHTML = steps.map((s, i) => `
    <tr data-step="${i}">
      <td class="muted">${i + 1}</td>
      <td><b>${esc(s.bucket || '—')}</b></td>
      <td class="num">${s.parallel ?? '—'}</td>
      <td class="num">${s.number ?? '—'}</td>
      <td class="st">${stepBadge(s.status)}</td>
      <td class="num sr">${s.success_rate != null ? fmtPct(s.success_rate) : '—'}</td>
      <td class="num ttft">${s.ttft_p50_sec != null ? fmtNum(s.ttft_p50_sec) + 's' : '—'}</td>
      <td class="num tps">${s.output_tps != null ? fmtNum(s.output_tps) : '—'}</td>
      <td class="num dur">${s.duration_sec != null ? fmtDur(s.duration_sec) : '—'}</td>
    </tr>`).join('');
}

function stepBadge(status) {
  const map = { pending: ['', '待执行'], running: ['info', '运行中'], succeeded: ['ok', '成功'], failed: ['err', '失败'], skipped: ['warn', '跳过'], canceled: ['warn', '已取消'] };
  const [cls, text] = map[status] || ['', status];
  return `<span class="badge ${cls}">${text}</span>`;
}

function handleEvent(type, d) {
  const run = state.run;
  if (!run) return;

  switch (type) {
    case 'run.snapshot':
      Object.assign(run, d);
      renderRun(run);
      break;

    case 'run.started':
      run.status = 'running';
      renderRun(run);
      appendLog(`[${d.at || nowISO()}] 引擎启动 ${d.engine?.pid ? 'pid=' + d.engine.pid : ''}`);
      break;

    case 'step.started': {
      run.progress = run.progress || {};
      run.progress.current = { bucket: d.bucket, parallel: d.parallel, number: d.number };
      run.steps = run.steps || [];
      if (!run.steps[d.step_index]) run.steps[d.step_index] = {};
      Object.assign(run.steps[d.step_index], { bucket: d.bucket, parallel: d.parallel, number: d.number, status: 'running' });
      renderRun(run);
      appendLog(`→ step ${d.step_index + 1}: ${d.bucket} 并发=${d.parallel} 请求=${d.number}`);
      break;
    }

    case 'step.progress': {
      const tr = $(`#steps-table tbody tr[data-step="${d.step_index}"]`);
      if (tr) {
        const total = Number(d.total) || 0;
        const done = Math.max(0, Math.min(Number(d.completed) || 0, total));
        $('.st', tr).innerHTML = total > 0
          ? `<span class="badge info">运行中 ${done}/${total}</span>`
          : '<span class="badge info">运行中</span>';
      }
      break;
    }

    case 'step.finished': {
      const steps = run.steps || (run.steps = []);
      const prev = steps[d.step_index] || (steps[d.step_index] = {});
      Object.assign(prev, {
        status: d.status, success_rate: d.success_rate,
        output_tps: d.output_tps, duration_sec: d.duration_sec,
        ttft_p50_sec: d.ttft_p50_sec ?? prev.ttft_p50_sec,
        score: d.score ?? prev.score,
      });
      renderRun(run);
      appendLog(`← step ${d.step_index + 1} ${d.status} 成功率=${fmtPct(d.success_rate)} 耗时=${fmtDur(d.duration_sec)}`);
      break;
    }

    case 'run.progress':
      run.progress = { ...(run.progress || {}), ...d };
      if (d.current === null) run.progress.current = null;   // 步骤结束后清掉"正在跑"
      renderRun(run);
      break;

    case 'run.log':
      appendLog(d.line);
      break;

    case 'run.gate':
      run.gate_result = d;
      break;

    case 'run.finished':
      run.status = d.status;
      run.finished_at = d.finished_at;
      run.artifacts = d.artifacts || run.artifacts || [];
      renderRun(run);
      appendLog(`■ 任务结束：${statusText(d.status)}`);
      if (state.unsub) { state.unsub(); state.unsub = null; }
      api('GET', `/runs/${run.run_id}?include=steps`)
        .then(fresh => { if (state.run?.run_id === run.run_id) { state.run = fresh; renderRun(fresh); } })
        .catch(() => { })
        .finally(() => loadRunResult(run.run_id));
      break;

    case 'error':
      toast(`引擎错误：${d.detail || d.code}`, 'err');
      appendLog(`✕ ${d.code}: ${d.detail || ''}`);
      break;
  }
}

function appendLog(line) {
  const box = $('#run-log');
  if (!box || !line) return;
  box.textContent += line + '\n';
  if (!box.hidden) box.scrollTop = box.scrollHeight;
}

$('#btn-toggle-log')?.addEventListener('click', e => {
  const box = $('#run-log');
  box.hidden = !box.hidden;
  e.target.textContent = box.hidden ? '显示日志' : '隐藏日志';
  if (!box.hidden) box.scrollTop = box.scrollHeight;
});

$('#btn-cancel')?.addEventListener('click', async () => {
  if (!state.run) return;
  if (!confirm('确定取消该任务？已完成的步骤结果会保留，仍可生成报告。')) return;
  try {
    await api('POST', `/runs/${state.run.run_id}/cancel`);
    toast('已发送取消请求');
  } catch (err) { toast(err.message, 'err'); }
});

$('#btn-back-new')?.addEventListener('click', () => { state.step = 1; location.hash = '#/new'; });

// 从导航进入「新建测试」时回到第一步
$('#nav')?.addEventListener('click', e => {
  if (e.target.closest('a')?.dataset.route === 'new') state.step = 1;
});

/* ---------- 结果 ---------- */
async function loadRunResult(runId) {
  try {
    const [results, artifacts] = await Promise.all([
      api('GET', `/runs/${runId}/results`).catch(() => null),
      api('GET', `/runs/${runId}/artifacts`).catch(() => ({ items: [] })),
    ]);
    if (!results) return;
    renderResults(results, artifacts.items || []);
  } catch (err) { /* 结果尚未就绪 */ }
}

function renderResults(results, artifacts) {
  $('#run-result').hidden = false;

  if (results.capability === 'eval') { renderEvalResults(results, artifacts); return; }
  if (results.capability === 'cache') { renderCacheResults(results, artifacts); return; }
  $('#run-result-title').textContent = '验收结论';

  const s = results.summary || {};
  const kpis = [
    ['成功率', fmtPct(s.success_rate), `${s.steps_succeeded ?? '—'}/${s.steps_total ?? '—'} 步成功`],
    ['TTFT P50', s.ttft_p50_sec != null ? fmtNum(s.ttft_p50_sec) + 's' : '—', '首 token 延迟'],
    ['TTFT P90', s.ttft_p90_sec != null ? fmtNum(s.ttft_p90_sec) + 's' : '—', '长尾'],
    ['TPOT', s.tpot_ms != null ? fmtNum(s.tpot_ms) + 'ms' : '—', '每输出 token'],
    ['峰值 TPM', fmtNum(s.tpm_peak, 0), 'tokens/min'],
    ['峰值 Output', s.peak_output_tps != null ? fmtNum(s.peak_output_tps) + ' t/s' : '—', '单档最高'],
  ];
  $('#run-kpis').innerHTML = kpis.map(([k, v, sub]) =>
    `<div class="kpi"><div class="k">${esc(k)}</div><div class="v">${esc(v)}<small> ${esc(sub)}</small></div></div>`).join('');

  const g = results.gate_result;
  const badge = $('#gate-badge');
  if (!g) {
    badge.className = 'badge'; badge.textContent = '未启用验收判定';
    $('#gate-body').innerHTML = '<p class="muted">本次未设置验收门槛模板，仅输出指标。</p>';
  } else {
    badge.className = 'badge ' + (g.passed ? 'ok' : 'err');
    badge.textContent = g.passed ? '✓ 验收通过' : '✕ 未达标';
    const viol = (g.violations || []).map(v => `
      <tr><td>${esc(v.tier || v.bucket || '')}</td>
        <td>${esc(v.rule)}</td>
        <td class="num">${fmtNum(v.actual)}</td>
        <td class="num">${fmtNum(v.threshold)}</td>
        <td class="muted">${esc(v.message || '')}</td></tr>`).join('');
    const tiers = (g.tier_summary || []).map(t => `
      <tr><td><b>${esc(t.tier)}</b></td><td class="num">${t.steps}</td>
        <td class="num">${fmtNum(t.p50_max_sec)}s</td><td class="num">${fmtNum(t.p90_max_sec)}s</td>
        <td>${t.passed ? '<span class="badge ok">通过</span>' : '<span class="badge err">不通过</span>'}</td></tr>`).join('');
    $('#gate-body').innerHTML = `
      <p class="muted">模板 ${esc(g.profile)} · 违规 ${(g.violations || []).length} 项</p>
      ${tiers ? `<table class="tbl"><thead><tr><th>档位</th><th class="num">步数</th>
        <th class="num">P50 最大值</th><th class="num">P90 最大值</th><th>结论</th></tr></thead><tbody>${tiers}</tbody></table>` : ''}
      ${viol ? `<h4 style="margin-top:16px">违规明细</h4><table class="tbl"><thead><tr>
        <th>档位</th><th>规则</th><th class="num">实测</th><th class="num">阈值</th><th>说明</th>
      </tr></thead><tbody>${viol}</tbody></table>` : ''}`;
  }

  $('#artifact-list').innerHTML = artifacts.length ? artifacts.map(a => `
    <li>
      <span class="kind">${esc(a.kind)}</span>
      <span class="name">${esc(a.filename)}</span>
      <span class="size">${fmtBytes(a.size_bytes)}</span>
      <a class="btn sm ghost" href="${esc(a.download_url)}" download>下载</a>
    </li>`).join('') : '<li class="muted">暂无产物</li>';
}

function renderCacheResults(results, artifacts) {
  $('#run-result-title').textContent = '缓存测试结论';
  const s = results.summary || {};
  const c = results.cache || {};
  const g = results.gate_result;
  let cls, text;
  if (g) {
    cls = g.passed ? 'ok' : 'err';
    text = g.passed ? '✓ 验收通过' : '✕ 未达标';
  } else {
    const v = s.verdict;
    cls = v === 'pass' ? 'ok' : v === 'partial' ? 'warn' : 'err';
    text = v === 'pass' ? '✓ 通过' : v === 'partial' ? '! 部分通过' : '✕ 未通过';
  }
  const badge = $('#gate-badge');
  badge.className = 'badge ' + cls;
  badge.textContent = text;

  const imp = s.ttft_improvement;
  const kpis = [
    ['稳态 TTFT P50', s.ttft_steady_p50_ms != null ? fmtNum(s.ttft_steady_p50_ms) + 'ms' : '—', '前缀命中下的首 token'],
    ['首轮 TTFT', s.ttft_first_round_ms != null ? fmtNum(s.ttft_first_round_ms) + 'ms' : '—', '冷/半冷缓存'],
    ['TTFT 改善率', imp != null ? fmtPct(imp) : '—', '1 − 稳态P50/首轮均值'],
    ['成功率', s.success_rate != null ? fmtPct(s.success_rate) : '—', `${s.successful_requests ?? '—'}/${s.total_requests ?? '—'} 请求`],
  ];
  $('#run-kpis').innerHTML = kpis.map(([k, v, sub]) =>
    `<div class="kpi"><div class="k">${esc(k)}</div><div class="v">${esc(v)}<small> ${esc(sub)}</small></div></div>`).join('');

  const roundRows = (c.rounds || []).map(r => `
    <tr><td class="num">${r.round}</td><td class="num">${r.requests}</td>
      <td class="num">${fmtNum(r.ttft_mean_ms)}ms</td>
      <td class="num">${fmtNum(r.input_tokens_mean)}</td></tr>`).join('');
  const violRows = (g?.violations || []).map(v => `
    <tr><td>${esc(v.rule)}</td><td class="num">${fmtNum(v.actual)}</td>
      <td class="num">${fmtNum(v.threshold)}</td><td class="muted">${esc(v.message || '')}</td></tr>`).join('');
  $('#gate-body').innerHTML = `
    <p class="muted">workload ${esc(c.workload || 'multi-round-chat')} · ${esc(c.model || '')} ·
      ${c.num_sessions ? c.num_sessions + ' 个并发 session · ' : ''}${s.total_requests ?? '—'} 个请求。
      ${g ? `验收模板 ${esc(g.profile)} · 违规 ${(g.violations || []).length} 项` : '未启用验收'}</p>
    ${roundRows ? `<table class="tbl"><thead><tr><th class="num">轮次</th><th class="num">请求数</th>
      <th class="num">TTFT 均值</th><th class="num">输入 token 均值</th></tr></thead><tbody>${roundRows}</tbody></table>` : ''}
    ${violRows ? `<h4 style="margin-top:16px">违规明细</h4><table class="tbl"><thead><tr>
      <th>规则</th><th class="num">实测</th><th class="num">阈值</th><th>说明</th>
    </tr></thead><tbody>${violRows}</tbody></table>` : ''}`;

  $('#artifact-list').innerHTML = artifacts.length ? artifacts.map(a => `
    <li>
      <span class="kind">${esc(a.kind)}</span>
      <span class="name">${esc(a.filename)}</span>
      <span class="size">${fmtBytes(a.size_bytes)}</span>
      <a class="btn sm ghost" href="${esc(a.download_url)}" download>下载</a>
    </li>`).join('') : '<li class="muted">暂无产物</li>';
}

function renderEvalResults(results, artifacts) {
  $('#run-result-title').textContent = '评测结论';
  const s = results.summary || {};
  const ds = (results.eval && results.eval.datasets) || [];
  const g = results.gate_result;
  let cls, text;
  if (g) {
    cls = g.passed ? 'ok' : 'err';
    text = g.passed ? '✓ 验收通过' : '✕ 未达标';
  } else {
    const verdictMap = { pass: ['ok', '✓ 全部通过'], partial: ['warn', '! 部分通过'], fail: ['err', '✕ 未通过'] };
    [cls, text] = verdictMap[s.verdict] || ['', '—'];
  }
  const badge = $('#gate-badge');
  badge.className = 'badge ' + cls;
  badge.textContent = text;

  const kpis = [
    ['数据集', `${s.datasets_succeeded ?? '—'}/${s.datasets_total ?? '—'}`, '成功 / 总数'],
    ['平均分', s.score_avg != null ? fmtNum(s.score_avg, 4) : '—', '成功数据集均值'],
    ['模型', results.eval?.model || '—', results.eval?.provider || ''],
  ];
  $('#run-kpis').innerHTML = kpis.map(([k, v, sub]) =>
    `<div class="kpi"><div class="k">${esc(k)}</div><div class="v">${esc(v)}<small> ${esc(sub)}</small></div></div>`).join('');

  const tierByDs = {};
  (g?.tier_summary || []).forEach(t => tierByDs[t.tier] = t);
  const rows = ds.map(d => {
    const tier = tierByDs[d.dataset];
    const sc = d.success ? fmtNum(d.score, 4) : '—';
    const min = tier ? fmtNum(tier.p50_max_sec, 4) : '不判定';
    const passCell = tier
      ? (tier.passed ? '<span class="badge ok">达标</span>' : '<span class="badge err">未达标</span>')
      : '<span class="muted">—</span>';
    return `
    <tr>
      <td><b>${esc(d.dataset)}</b></td>
      <td>${d.success ? '<span class="badge ok">成功</span>' : `<span class="badge err">失败</span>`}</td>
      <td class="num"><b>${sc}</b></td>
      <td class="num">${min}</td>
      <td>${passCell}</td>
      <td>${esc(d.metric || '—')}</td>
      <td class="num">${d.num_samples ?? '—'}</td>
      <td class="num">${d.repeats || '默认'}</td>
      <td class="num">${d.skipped ?? 0}</td>
      <td class="num">${d.elapsed_sec != null ? fmtDur(d.elapsed_sec) : '—'}</td>
      <td class="muted">${esc(d.error || '')}</td>
    </tr>`;
  }).join('');
  const violRows = (g?.violations || []).map(v => `
    <tr><td>${esc(v.bucket || '')}</td><td>${esc(v.rule)}</td>
      <td class="num">${fmtNum(v.actual, 4)}</td><td class="num">${fmtNum(v.threshold, 4)}</td>
      <td class="muted">${esc(v.message || '')}</td></tr>`).join('');
  $('#gate-body').innerHTML = ds.length
    ? `<p class="muted">共 ${ds.length} 个数据集；分数来自 eval_summary.json（Python 侧口径）。
       ${g ? `验收模板 ${esc(g.profile)} · 违规 ${(g.violations || []).length} 项` : '未启用分数验收'}</p>
       <table class="tbl"><thead><tr><th>数据集</th><th>状态</th><th class="num">得分</th><th class="num">下限</th><th>结论</th>
       <th>指标</th><th class="num">样本数</th><th class="num">repeats</th><th class="num">跳过</th><th class="num">耗时</th><th>错误</th>
       </tr></thead><tbody>${rows}</tbody></table>
       ${violRows ? `<h4 style="margin-top:16px">违规明细</h4><table class="tbl"><thead><tr>
         <th>数据集</th><th>规则</th><th class="num">实测</th><th class="num">阈值</th><th>说明</th>
       </tr></thead><tbody>${violRows}</tbody></table>` : ''}`
    : '<p class="muted">没有数据集结果。</p>';

  $('#artifact-list').innerHTML = artifacts.length ? artifacts.map(a => `
    <li>
      <span class="kind">${esc(a.kind)}</span>
      <span class="name">${esc(a.filename)}</span>
      <span class="size">${fmtBytes(a.size_bytes)}</span>
      <a class="btn sm ghost" href="${esc(a.download_url)}" download>下载</a>
    </li>`).join('') : '<li class="muted">暂无产物</li>';
}

/* ================================================================
 * 历史
 * ============================================================== */
async function loadHistory(reset) {
  if (reset) { state.historyCursor = null; $('#history-table tbody').innerHTML = ''; }
  const params = new URLSearchParams({ limit: '20' });
  if (state.historyCursor) params.set('cursor', state.historyCursor);
  const st = $('#h-status').value;
  if (st) params.set('status', st);
  try {
    const res = await api('GET', '/runs?' + params);
    state.historyCursor = res.next_cursor;
    $('#btn-more').hidden = !res.has_more;
    const rows = (res.items || []).map(r => {
      const g = r.gate_result;
      let verdict = '—';
      if (r.capability === 'cache' && g) {
        verdict = g.passed ? '<span class="badge ok">通过</span>' : '<span class="badge err">未达标</span>';
      } else if (r.capability === 'eval' && g) {
        verdict = g.passed ? '<span class="badge ok">通过</span>' : '<span class="badge err">未达标</span>';
      } else if (r.capability === 'eval') {
        const v = r.summary?.verdict;
        verdict = v === 'pass' ? '<span class="badge ok">通过</span>'
          : v === 'partial' ? '<span class="badge warn">部分通过</span>'
            : v === 'fail' ? '<span class="badge err">未通过</span>' : '—';
      } else if (g) {
        verdict = g.passed ? '<span class="badge ok">通过</span>' : '<span class="badge err">未达标</span>';
      }
      return `<tr>
        <td class="muted">${new Date(r.created_at).toLocaleString()}</td>
        <td><b>${esc(r.target?.model || '—')}</b><br><span class="hint">${esc(r.target?.vendor || '')}</span></td>
        <td>${r.capability === 'perf' ? '性能压测' : r.capability === 'eval' ? '效果评测' : r.capability === 'cache' ? '缓存测试' : '功能测试'}</td>
        <td><span class="status-pill ${r.status}">${statusText(r.status)}</span></td>
        <td class="num">${r.progress?.steps_done ?? 0}/${r.progress?.steps_total ?? r.plan?.steps_total ?? 0}</td>
        <td class="num">${r.capability === 'eval'
          ? (r.summary?.score_avg != null ? '均分 ' + fmtNum(r.summary.score_avg, 3) : '—')
          : r.capability === 'cache'
            ? (r.summary?.ttft_improvement != null ? '改善 ' + fmtPct(r.summary.ttft_improvement) : '—')
            : (r.summary?.success_rate != null ? fmtPct(r.summary.success_rate) : '—')}</td>
        <td>${verdict}</td>
        <td><a href="#/run/${esc(r.run_id)}">查看</a></td>
      </tr>`;
    }).join('');
    $('#history-table tbody').insertAdjacentHTML('beforeend', rows || '<tr><td colspan="8" class="muted">暂无记录</td></tr>');
  } catch (err) { toast(err.message, 'err'); }
}

$('#btn-refresh')?.addEventListener('click', () => loadHistory(true));
$('#h-status')?.addEventListener('change', () => loadHistory(true));
$('#btn-more')?.addEventListener('click', () => loadHistory(false));

/* ================================================================
 * 能力清单
 * ============================================================== */
function renderCatalog() {
  $('#catalog-tools').innerHTML = state.tools.map(t => `
    <div class="tool-card">
      <div class="head"><b>${esc(t.name)}</b>
        <span class="badge info">${esc(t.kind)}</span>
        <span class="hint">v${esc(t.version || '')}</span></div>
      ${t.capabilities.map(c => `<div class="cap">
        <span class="badge ${c.status === 'ga' ? 'ok' : c.status === 'preview' ? 'info' : 'warn'}">${esc(c.status)}</span>
        <b>${esc(c.name)}</b><span class="muted">${esc(c.description || '')}</span></div>`).join('')}
    </div>`).join('') || '<p class="muted">未获取到工具信息</p>';

  $('#catalog-buckets tbody').innerHTML = state.buckets.items.map(b => `
    <tr><td><b>${esc(b.label)}</b></td>
      <td class="num">${b.prompt_tokens.min} ~ ${b.prompt_tokens.max}</td>
      <td class="num">${b.max_parallel}</td>
      <td>${b.built_in ? '<span class="badge ok">就绪</span>' : '<span class="badge warn">需构建</span>'}</td></tr>`).join('');

  const gate = state.gates[0];
  $('#catalog-gates tbody').innerHTML = gate ? gate.tiers.map(t => `
    <tr><td><b>${esc(t.tier)}</b></td>
      <td class="num">${fmtNum(t.ttft_p50_max_sec)}s</td>
      <td class="num">${fmtNum(t.ttft_p90_max_sec)}s</td></tr>`).join('')
    : '<tr><td colspan="3" class="muted">无阈值模板</td></tr>';
}

/* ---------------------------------------------------------------- 启动 */
boot();
