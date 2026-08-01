(() => {
  'use strict';

  const MANAGEMENT_API = '/v0/management/grok2api-egress/api';
  const VIEW_META = {
    dashboard: ['概览', 'Grok2API 运行状态与资源概览'],
    accounts: ['账号', '筛选、批量管理并导出选中的验证账号'],
    egress: ['出口守护', '出口节点、质量检测与隔离策略'],
    models: ['模型', '模型路由、可用性与账号支持'],
    keys: ['Client Key', '访问密钥、速率限制与模型范围'],
    audits: ['请求审计', '请求性能、Token、重试与出口链路'],
    settings: ['设置', '查看并保存 Grok2API 完整运行配置']
  };
  const state = {
    view: 'dashboard', loading: false, refreshTimer: 0, debounceTimer: 0,
    dashboardPeriod: '24h', accountsProvider: 'grok_build', accountsPage: 1, accountsPageSize: 50,
    accountsSearch: '', accountsStatus: '', accounts: [], accountTotal: 0, accountSummary: null, accountSelected: new Set(),
    nodes: [], guard: null, nodeSearch: '', nodeSelected: new Set(),
    models: [], modelTotal: 0, modelSearch: '', modelProvider: '', modelStatus: '', modelSelected: new Set(),
    keys: [], keyTotal: 0, keySearch: '', keyStatus: '', keySelected: new Set(),
    audits: [], auditSummary: null, auditPeriod: '24h', auditSearch: '', auditStatus: '', auditCursor: '', auditNextCursor: '', auditHasMore: false, auditHistory: [],
    settings: null, formHandler: null, confirmHandler: null
  };

  const $ = (id) => document.getElementById(id);
  const root = $('view-root');
  const esc = (value) => String(value ?? '').replace(/[&<>'"]/g, (char) => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[char]));
  const number = (value, maximumFractionDigits = 0) => new Intl.NumberFormat('zh-CN', { maximumFractionDigits }).format(Number(value || 0));
  const money = (ticks) => '$' + (Number(ticks || 0) / 100000000).toFixed(4);
  const time = (value) => value ? new Intl.DateTimeFormat('zh-CN', {month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit'}).format(new Date(value)) : '-';
  const relative = (value) => {
    if (!value) return '-';
    const ts = typeof value === 'number' ? value * 1000 : new Date(value).getTime();
    const seconds = Math.max(0, Math.floor((Date.now() - ts) / 1000));
    if (seconds < 60) return seconds + ' 秒前';
    if (seconds < 3600) return Math.floor(seconds / 60) + ' 分钟前';
    if (seconds < 86400) return Math.floor(seconds / 3600) + ' 小时前';
    return Math.floor(seconds / 86400) + ' 天前';
  };
  const field = (id, label, value, options = {}) => {
    const type = options.type || 'text';
    const full = options.full ? ' full' : '';
    const attrs = [options.required ? 'required' : '', options.min !== undefined ? 'min="' + esc(options.min) + '"' : '', options.max !== undefined ? 'max="' + esc(options.max) + '"' : '', options.step ? 'step="' + esc(options.step) + '"' : ''].filter(Boolean).join(' ');
    const control = options.select
      ? '<select id="' + id + '">' + options.select.map((item) => '<option value="' + esc(item[0]) + '" ' + (String(value) === String(item[0]) ? 'selected' : '') + '>' + esc(item[1]) + '</option>').join('') + '</select>'
      : options.textarea
        ? '<textarea id="' + id + '" ' + attrs + '>' + esc(value) + '</textarea>'
        : '<input id="' + id + '" type="' + type + '" value="' + esc(value) + '" ' + attrs + ' autocomplete="off">';
    return '<div class="field' + full + '"><label for="' + id + '">' + esc(label) + '</label>' + control + '<p class="help">' + esc(options.help || '') + '</p></div>';
  };
  const checkField = (id, label, checked, help = '') => '<div class="field full check-row"><div><label for="' + id + '">' + esc(label) + '</label><p class="help">' + esc(help) + '</p></div><input id="' + id + '" type="checkbox" ' + (checked ? 'checked' : '') + '></div>';
  const metric = (label, value, detail = '', tone = '') => '<div class="metric"><div class="metric-label">' + esc(label) + '</div><div class="metric-value ' + esc(tone) + '">' + esc(value) + '</div><div class="metric-detail" title="' + esc(detail) + '">' + esc(detail) + '</div></div>';
  const badge = (text, tone = 'muted') => '<span class="badge ' + tone + '">' + esc(text) + '</span>';
  const pageFooter = (page, pageSize, total, key) => {
    const pages = Math.max(1, Math.ceil(total / pageSize));
    return '<div class="pagination"><span>第 ' + page + ' / ' + pages + ' 页 · 共 ' + number(total) + ' 项</span><div class="row-actions"><button class="button" data-action="' + key + '-prev" ' + (page <= 1 ? 'disabled' : '') + '>上一页</button><button class="button" data-action="' + key + '-next" ' + (page >= pages ? 'disabled' : '') + '>下一页</button></div></div>';
  };

  function managementKey() {
    const decodeStoredValue = (storageKey) => {
      let raw = localStorage.getItem(storageKey) || '';
      if (!raw) return null;
      if (raw.startsWith('enc::v1::')) {
        const encoded = atob(raw.slice(9));
        const value = Uint8Array.from(encoded, (char) => char.charCodeAt(0));
        const key = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent);
        for (let index = 0; index < value.length; index += 1) value[index] ^= key[index % key.length];
        raw = new TextDecoder().decode(value);
      }
      return JSON.parse(raw);
    };
    try {
      const legacy = decodeStoredValue('authToken');
      if (typeof legacy === 'string' && legacy) return legacy;
      const current = decodeStoredValue('cli-proxy-auth');
      const key = current?.state?.managementKey;
      return typeof key === 'string' ? key : '';
    } catch (_) { return ''; }
  }

  async function rawRequest(path, options = {}) {
    const key = managementKey();
    if (!key) throw new Error('CPA 管理密钥未持久化，请重新登录管理面板并勾选“记住密码”');
    return fetch(MANAGEMENT_API, {
      method: 'POST', cache: 'no-store',
      headers: {'Content-Type':'application/json','Accept':options.accept || 'application/json','Authorization':'Bearer ' + key,'X-Grok2API-Egress-UI':'1'},
      body: JSON.stringify({method: options.method || 'GET', path, body: options.body})
    });
  }

  async function api(path, options = {}) {
    const response = await rawRequest(path, options);
    const raw = await response.text();
    let payload = {};
    try { payload = raw ? JSON.parse(raw) : {}; } catch (_) { throw new Error('服务返回了无效数据'); }
    if (!response.ok || payload.error) throw new Error(payload?.error?.message || ('请求失败 (HTTP ' + response.status + ')'));
    return payload.data ?? payload;
  }

  async function download(path, body) {
    const response = await rawRequest(path, {method:'POST', body, accept:'application/json'});
    if (!response.ok) {
      let message = '导出失败 (HTTP ' + response.status + ')';
      try { message = (await response.json())?.error?.message || message; } catch (_) {}
      throw new Error(message);
    }
    const blob = await response.blob();
    const disposition = response.headers.get('Content-Disposition') || '';
    const match = disposition.match(/filename="?([^";]+)"?/i);
    const name = match?.[1] || ('grok2api-selected-accounts-' + new Date().toISOString().replace(/[:.]/g, '') + '.json');
    const href = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = href; link.download = name; document.body.appendChild(link); link.click(); link.remove();
    window.setTimeout(() => URL.revokeObjectURL(href), 1000);
  }

  function toast(message, kind = '') {
    const node = document.createElement('div');
    node.className = 'toast ' + kind;
    node.textContent = message;
    $('toast-region').appendChild(node);
    window.setTimeout(() => node.remove(), 4200);
  }

  function busy(button, value) {
    if (!button) return;
    button.disabled = value;
    button.classList.toggle('busy', value);
  }

  function setService(ok, text) {
    $('service-dot').className = 'status-dot ' + (ok ? 'good' : 'bad');
    $('service-text').textContent = text;
  }

  function openForm(title, description, html, handler, submitLabel = '保存') {
    $('form-dialog-title').textContent = title;
    $('form-dialog-description').textContent = description || '';
    $('form-dialog-body').innerHTML = '<div class="form-grid">' + html + '</div>';
    $('form-dialog-submit').textContent = submitLabel;
    state.formHandler = handler;
    $('form-dialog').showModal();
    $('form-dialog-body').querySelector('input,select,textarea')?.focus();
  }

  function confirmAction(title, description, html, handler, label = '确认') {
    $('confirm-dialog-title').textContent = title;
    $('confirm-dialog-description').textContent = description || '';
    $('confirm-dialog-body').innerHTML = html || '';
    $('confirm-dialog-submit').textContent = label;
    state.confirmHandler = handler;
    $('confirm-dialog').showModal();
  }

  function showDetail(title, description, value) {
    $('detail-dialog-title').textContent = title;
    $('detail-dialog-description').textContent = description || '';
    $('detail-dialog-body').innerHTML = '<pre class="json-view">' + esc(JSON.stringify(value, null, 2)) + '</pre>';
    $('detail-dialog').showModal();
  }

  function currentValue(id) { return $(id)?.value ?? ''; }
  function checked(id) { return Boolean($(id)?.checked); }

  async function runMutation(button, task, options = {}) {
    busy(button, true);
    try {
      const result = await task();
      if (options.close) $(options.close)?.close();
      if (options.message) toast(options.message);
      if (options.reload !== false) await loadCurrent(true);
      return result;
    } catch (error) {
      toast(error.message || '操作失败', 'error');
      throw error;
    } finally { busy(button, false); }
  }

  function navigate(view) {
    if (!VIEW_META[view]) return;
    state.view = view;
    location.hash = view;
    document.querySelectorAll('.nav-item').forEach((item) => item.setAttribute('aria-current', item.dataset.view === view ? 'page' : 'false'));
    $('page-title').textContent = VIEW_META[view][0];
    $('page-subtitle').textContent = VIEW_META[view][1];
    document.body.classList.remove('sidebar-open');
    loadCurrent();
  }

  async function loadCurrent(silent = false) {
    if (state.loading) return;
    state.loading = true;
    busy($('refresh-button'), true);
    if (!silent) root.innerHTML = '<div class="loading-panel">正在加载</div>';
    try {
      const loaders = {dashboard:loadDashboard, accounts:loadAccounts, egress:loadEgress, models:loadModels, keys:loadKeys, audits:loadAudits, settings:loadSettings};
      await loaders[state.view]();
      setService(true, '已连接');
    } catch (error) {
      setService(false, '连接失败');
      root.innerHTML = '<div class="section error-state">' + esc(error.message || '加载失败') + '</div>';
      if (!silent) toast(error.message || '加载失败', 'error');
    } finally {
      state.loading = false;
      busy($('refresh-button'), false);
    }
  }

  async function loadDashboard() {
    const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
    const dashboard = await api('/dashboard?period=' + encodeURIComponent(state.dashboardPeriod) + '&timezone=' + encodeURIComponent(timezone));
    const usage = dashboard.usage || {};
    const resources = dashboard.resources || {};
    root.innerHTML = '<div class="stack">' +
      '<div class="toolbar"><div class="segmented">' + ['24h','7d','30d','90d'].map((period) => '<button class="button ' + (period === state.dashboardPeriod ? 'active' : '') + '" data-action="dashboard-period" data-value="' + period + '">' + period + '</button>').join('') + '</div></div>' +
      '<section class="metrics" aria-label="运行概览">' +
        metric('请求数', number(usage.requests), '成功 ' + number(usage.successfulRequests) + ' · 失败 ' + number(usage.failedRequests), usage.failedRequests ? 'bad' : '') +
        metric('成功率', number(usage.successRate, 1) + '%', '当前统计周期', usage.successRate >= 99 ? 'good' : '') +
        metric('输出 Token', number(usage.outputTokens), '总 Token ' + number(usage.tokens)) +
        metric('平均首字', usage.averageFirstTokenMs ? number(usage.averageFirstTokenMs) + ' ms' : '-', usage.outputTokensPerSecond ? number(usage.outputTokensPerSecond, 1) + ' Token/s' : '暂无吞吐样本') +
      '</section>' +
      '<div class="split"><section class="section"><div class="section-head"><div><h2>资源</h2><p>账号、模型与 Client Key 当前状态</p></div></div><div class="stat-grid">' +
        [['可用账号',resources.activeAccounts,'总计 '+number(resources.totalAccounts)],['Build 账号',resources.buildAccounts,'Web '+number(resources.webAccounts)],['Console 账号',resources.consoleAccounts,'独立账号池'],['启用模型',resources.enabledModels,'总计 '+number(resources.totalModels)],['活跃 Key',resources.activeClientKeys,'总计 '+number(resources.totalClientKeys)],['计费估算',money(usage.billedCostUsdTicks),'当前周期']].map((item) => '<div class="stat"><span class="metric-label">'+esc(item[0])+'</span><div class="metric-value">'+esc(number(item[1]))+'</div><span class="subtext">'+esc(item[2])+'</span></div>').join('') +
      '</div></section><section class="section"><div class="section-head"><div><h2>Provider 分布</h2><p>按请求统计</p></div></div><div class="event-list">' +
        (dashboard.providers || []).map((item) => '<div class="event"><div><strong>'+esc(item.provider)+'</strong><small>成功 '+number(item.successfulRequests)+' · Token '+number(item.tokens)+'</small></div><span class="numeric">'+number(item.requests)+'</span></div>').join('') +
      '</div></section></div>' +
      '<section class="section"><div class="section-head"><div><h2>热门模型</h2><p>请求量与 Token 消耗</p></div></div><div class="table-wrap"><table><thead><tr><th>模型</th><th class="numeric">请求</th><th class="numeric">输入</th><th class="numeric">输出</th><th class="numeric">费用</th></tr></thead><tbody>' +
        (dashboard.topModels || []).map((item) => '<tr><td><span class="name-cell">'+esc(item.model)+'</span></td><td class="numeric">'+number(item.requests)+'</td><td class="numeric">'+number(item.inputTokens)+'</td><td class="numeric">'+number(item.outputTokens)+'</td><td class="numeric">'+money(item.billedCostUsdTicks)+'</td></tr>').join('') +
      '</tbody></table></div></section></div>';
  }

  async function loadAccounts() {
    const query = new URLSearchParams({page:String(state.accountsPage),pageSize:String(state.accountsPageSize),provider:state.accountsProvider});
    if (state.accountsSearch) query.set('search', state.accountsSearch);
    if (state.accountsStatus) query.set('status', state.accountsStatus);
    const [page, summary, nodes] = await Promise.all([api('/accounts?' + query), api('/accounts/summary'), api('/nodes?page=1&pageSize=2000')]);
    state.accounts = page.items || []; state.accountTotal = page.total || 0; state.accountSummary = summary;
    state.nodes = nodes.items || state.nodes;
    state.accountSelected = new Set([...state.accountSelected].filter((id) => state.accounts.some((item) => item.id === id)));
    renderAccounts();
  }

  function accountStatus(account) {
    if (!account.enabled) return badge('已停用', 'muted');
    if (account.authStatus === 'reauthRequired') return badge('需重认证', 'bad');
    if (account.cooldownUntil && new Date(account.cooldownUntil) > new Date()) return badge('冷却中', 'warn');
    if (account.quota?.status === 'waitingReset') return badge('等待重置', 'warn');
    return badge('可调度', 'good');
  }

  function renderAccounts() {
    const summary = state.accountSummary || {};
    const all = state.accounts.length > 0 && state.accounts.every((item) => state.accountSelected.has(item.id));
    root.innerHTML = '<div class="stack">' +
      '<section class="metrics">' + metric('账号总数', number(summary.total), '当前全部 Provider') + metric('可调度', number(summary.available), '恢复中 ' + number(summary.recovering), 'good') + metric('需要关注', number(summary.attention), '风险 ' + number(summary.risk), summary.attention ? 'bad' : '') + metric('需重认证', number(summary.issues?.reauthRequired), '已停用 ' + number(summary.issues?.disabled), summary.issues?.reauthRequired ? 'bad' : '') + '</section>' +
      '<section class="section"><div class="section-head"><div><h2>账号列表</h2><p>导出只包含当前 Provider 中明确选中的账号</p></div><div class="toolbar">' +
        '<div class="segmented">' + [['grok_build','Build'],['grok_web','Web'],['grok_console','Console']].map((item) => '<button class="button '+(state.accountsProvider===item[0]?'active':'')+'" data-action="account-provider" data-value="'+item[0]+'">'+item[1]+'</button>').join('') + '</div>' +
        '<input id="account-search" class="search" type="search" placeholder="搜索名称、邮箱或 ID" value="'+esc(state.accountsSearch)+'" aria-label="搜索账号">' +
        '<select id="account-status" class="filter-select"><option value="">全部状态</option><option value="enabled" '+(state.accountsStatus==='enabled'?'selected':'')+'>已启用</option><option value="disabled" '+(state.accountsStatus==='disabled'?'selected':'')+'>已停用</option><option value="reauthRequired" '+(state.accountsStatus==='reauthRequired'?'selected':'')+'>需重认证</option><option value="cooldown" '+(state.accountsStatus==='cooldown'?'selected':'')+'>冷却中</option></select>' +
      '</div></div>' +
      '<div class="batch-bar" '+(state.accountSelected.size?'':'hidden')+'><span>已选择 '+state.accountSelected.size+' 个账号</span><div class="batch-actions"><button class="button" data-action="account-enable">启用</button><button class="button" data-action="account-disable">停用</button><button class="button" data-action="account-bind">绑定出口</button><button class="button" data-action="account-quota">刷新额度</button><button class="button" data-action="account-token">刷新凭据</button><button class="button primary" data-action="account-export">导出选中</button><button class="button danger" data-action="account-delete">删除</button></div></div>' +
      '<div class="table-wrap"><table><colgroup><col style="width:2.75rem"><col style="width:17rem"><col style="width:7rem"><col style="width:10rem"><col style="width:9rem"><col style="width:8rem"><col style="width:10rem"><col style="width:12rem"></colgroup><thead><tr><th class="check-cell"><input id="account-select-all" type="checkbox" '+(all?'checked':'')+' aria-label="选择当前页账号"></th><th>账号</th><th>状态</th><th>额度</th><th>出口</th><th class="numeric">并发 / 优先级</th><th>最近使用</th><th style="text-align:right">操作</th></tr></thead><tbody>' +
      (state.accounts.length ? state.accounts.map((account) => {
        const quota = account.quota || {};
        const node = state.nodes.find((item) => item.id === account.egressNodeId);
        return '<tr data-id="'+esc(account.id)+'"><td class="check-cell"><input type="checkbox" data-action="account-select" '+(state.accountSelected.has(account.id)?'checked':'')+' aria-label="选择 '+esc(account.name)+'"></td><td><span class="name-cell" title="'+esc(account.name)+'">'+esc(account.name)+'</span><span class="subtext">'+esc(account.email || ('ID '+account.id))+' · '+esc(account.authType)+'</span></td><td>'+accountStatus(account)+'<span class="subtext">'+esc(account.lastError || '')+'</span></td><td><span>'+ (quota.limitKnown ? number(quota.remaining,1)+' / '+number(quota.limit,1) : quota.type || '-') +'</span><span class="subtext">'+esc(quota.status || '-')+'</span></td><td><span class="name-cell">'+esc(node?.name || (account.egressNodeId ? '节点 '+account.egressNodeId : '未绑定'))+'</span><span class="subtext">'+esc(account.egressAssignmentMode || 'auto')+'</span></td><td class="numeric">'+number(account.maxConcurrent)+' / '+number(account.priority)+'</td><td>'+relative(account.lastUsedAt)+'<span class="subtext">'+time(account.lastUsedAt)+'</span></td><td><div class="row-actions"><button class="button" data-action="account-detail">详情</button><button class="button ghost" data-action="account-edit">编辑</button></div></td></tr>';
      }).join('') : '<tr><td colspan="8" class="empty">没有匹配的账号</td></tr>') +
      '</tbody></table></div>' + pageFooter(state.accountsPage,state.accountsPageSize,state.accountTotal,'account') + '</section></div>';
    $('account-search').addEventListener('input', debounce((event) => { state.accountsSearch = event.target.value.trim(); state.accountsPage = 1; loadCurrent(true); }, 280));
    $('account-status').addEventListener('change', (event) => { state.accountsStatus = event.target.value; state.accountsPage = 1; loadCurrent(true); });
    $('account-select-all').addEventListener('change', (event) => { state.accounts.forEach((item) => event.target.checked ? state.accountSelected.add(item.id) : state.accountSelected.delete(item.id)); renderAccounts(); });
  }

  function selectedAccounts() { return [...state.accountSelected]; }

  async function updateAccountBatch(enabled, button) {
    const ids = selectedAccounts();
    await runMutation(button, () => api('/accounts/batch', {method:'PATCH',body:{ids,provider:state.accountsProvider,enabled}}), {message:enabled?'已启用选中账号':'已停用选中账号'});
    state.accountSelected.clear();
  }

  function exportAccounts() {
    const ids = selectedAccounts();
    confirmAction('导出选中账号', '将导出 ' + ids.length + ' 个 ' + state.accountsProvider + ' 账号的完整验证凭据。', '<div class="notice">文件包含敏感凭据，请勿上传到公开仓库或发送给他人。</div><div class="field full check-row" style="margin-top:1rem"><label for="export-confirm">我确认只在受控环境中使用</label><input id="export-confirm" type="checkbox"></div>', async (button) => {
      if (!checked('export-confirm')) { toast('请先确认敏感凭据使用范围', 'error'); return; }
      busy(button, true);
      try { await download('/accounts/export', {provider:state.accountsProvider,ids}); $('confirm-dialog').close(); }
      catch (error) { toast(error.message, 'error'); }
      finally { busy(button, false); }
    }, '导出 ' + ids.length + ' 个账号');
  }

  function editAccount(account) {
    openForm('编辑账号', account.name, field('account-name','名称',account.name,{required:true}) + field('account-priority','优先级',account.priority,{type:'number',min:0}) + field('account-concurrency','最大并发',account.maxConcurrent,{type:'number',min:0}) + field('account-minimum','最低剩余额度',account.minimumRemaining,{type:'number',min:0,step:'0.01'}) + checkField('account-enabled','启用账号',account.enabled), async (button) => {
      await runMutation(button, () => api('/accounts/'+encodeURIComponent(account.id), {method:'PATCH',body:{name:currentValue('account-name').trim(),enabled:checked('account-enabled'),priority:Number(currentValue('account-priority')),maxConcurrent:Number(currentValue('account-concurrency')),minimumRemaining:Number(currentValue('account-minimum'))}}), {close:'form-dialog',message:'账号已更新'});
    });
  }

  function bindAccounts() {
    const options = state.nodes.filter((node) => node.scope === state.accountsProvider || (state.accountsProvider === 'grok_build' && node.scope === 'grok_build'));
    openForm('绑定出口节点', '将选中的账号绑定到指定出口；也可以切回自动分配。', field('bind-node','出口节点','',{full:true,select:[['','自动分配 / 解除手动绑定'],...options.map((node)=>[node.id,node.name+' · '+(node.exitIp||'未探测')])]}) , async (button) => {
      const ids = selectedAccounts(); const nodeID = currentValue('bind-node');
      if (nodeID) await runMutation(button, () => api('/nodes/'+encodeURIComponent(nodeID)+'/accounts',{method:'POST',body:{provider:state.accountsProvider,ids,mode:'manual'}}), {close:'form-dialog',message:'账号已绑定出口'});
      else await runMutation(button, () => api('/nodes/accounts',{method:'DELETE',body:{provider:state.accountsProvider,ids}}), {close:'form-dialog',message:'账号已恢复自动分配'});
      state.accountSelected.clear();
    });
  }

  async function loadEgress() {
    const [guard, list] = await Promise.all([api('/quality-guard'), api('/nodes?page=1&pageSize=2000')]);
    state.guard = guard; state.nodes = list.items || [];
    state.nodeSelected = new Set([...state.nodeSelected].filter((id) => state.nodes.some((item) => item.id === id)));
    renderEgress();
  }

  function visibleNodes() {
    const query = state.nodeSearch.toLowerCase();
    return !query ? state.nodes : state.nodes.filter((node) => [node.id,node.name,node.exitIp].some((value)=>String(value||'').toLowerCase().includes(query)));
  }

  function guardNode(id) { return state.guard?.nodes?.[id] || {}; }

  function nodeStatus(node, guard) {
    if (guard.disabled_by_guard) return badge('已隔离','bad');
    if (!node.enabled) return badge('已停用','muted');
    if (guard.error_strikes) return badge('检测失败','warn');
    if (guard.last_classification === 'hard' || guard.last_classification === 'soft') return badge('可疑','warn');
    if (guard.last_classification === 'healthy') return badge('健康','good');
    return badge('待检测','muted');
  }

  function renderEgress() {
    const nodes = visibleNodes(); const cfg = state.guard?.config || {}; const selected = state.nodeSelected.size;
    const enabled = state.nodes.filter((item)=>item.enabled).length;
    const quarantined = state.nodes.filter((item)=>guardNode(item.id).disabled_by_guard).length;
    const all = nodes.length && nodes.every((item)=>state.nodeSelected.has(item.id));
    const stats = state.guard?.statistics || {active:{total:0,healthy:0,soft:0,hard:0,errors:0,output_tokens:0},passive:{total:0,healthy:0,soft:0,hard:0,errors:0},actions:{quarantined:0,restored:0,suppressed:0}};
    root.innerHTML = '<div class="stack"><section class="metrics">'+metric('守护进程',state.guard?.available?'运行中':'不可用',state.guard?.updatedAt?'更新于 '+relative(state.guard.updatedAt):'没有状态',state.guard?.available?'good':'bad')+metric('运行模式',cfg.mode||'-',cfg.model||'-')+metric('可调度节点',enabled+' / '+state.nodes.length,'启用 / 全部')+metric('已隔离',quarantined,quarantined?'等待真实模型复测':'当前无隔离',quarantined?'bad':'good')+'</section>'+
      '<section class="section"><div class="section-head"><div><h2>出口节点</h2><p>代理 URL 仅写入，不在页面回显</p></div><div class="toolbar"><input id="node-search" class="search" type="search" placeholder="搜索节点、IP 或 ID" value="'+esc(state.nodeSearch)+'"><button class="button primary" data-action="node-add">添加节点</button></div></div>'+
      '<div class="batch-bar" '+(selected?'':'hidden')+'><span>已选择 '+selected+' 个节点</span><div class="batch-actions"><button class="button" data-action="node-enable">启用</button><button class="button" data-action="node-disable">停用</button><button class="button" data-action="node-test-batch">连通性</button><button class="button danger" data-action="node-delete-batch">删除</button></div></div>'+
      '<div class="table-wrap"><table><thead><tr><th class="check-cell"><input id="node-select-all" type="checkbox" '+(all?'checked':'')+'></th><th>节点</th><th>状态</th><th>出口 IP</th><th class="numeric">Token/s</th><th class="numeric">首字</th><th>账号</th><th style="text-align:right">操作</th></tr></thead><tbody>'+
      (nodes.length ? nodes.map((node)=>{const guard=guardNode(node.id);return '<tr data-id="'+esc(node.id)+'"><td class="check-cell"><input type="checkbox" data-action="node-select" '+(state.nodeSelected.has(node.id)?'checked':'')+'></td><td><span class="name-cell">'+esc(node.name)+'</span><span class="subtext">ID '+esc(node.id)+' · '+(node.proxyPool?'代理池':'固定代理')+'</span></td><td>'+nodeStatus(node,guard)+'<span class="subtext">'+esc(guard.last_reason||node.probeStatus||'-')+'</span></td><td>'+esc(node.exitIp||'-')+'<span class="subtext">'+esc(node.probeStatus||'unknown')+' · '+number(node.probeLatencyMs)+' ms</span></td><td class="numeric '+(guard.last_classification==='hard'?'tone-bad':guard.last_classification==='soft'?'tone-warn':'')+'">'+(guard.last_observed_at?Number(guard.last_output_tps||0).toFixed(1):'-')+'</td><td class="numeric">'+(guard.last_first_token_ms?number(guard.last_first_token_ms)+' ms':'-')+'</td><td>'+number(node.assignedAccountCount)+' / '+(node.accountCapacity||'∞')+'</td><td><div class="row-actions"><button class="button" data-action="node-quality">质量</button><button class="button" data-action="node-test">连通性</button><button class="button ghost" data-action="node-edit">编辑</button><button class="button ghost" data-action="node-toggle">'+(node.enabled?'停用':'启用')+'</button></div></td></tr>'}).join(''):'<tr><td colspan="8" class="empty">没有匹配的节点</td></tr>')+'</tbody></table></div></section>'+
      '<div class="split"><section class="section"><div class="section-head"><div><h2>检测统计</h2><p>主动与被动质量检测</p></div></div><div class="stat-grid">'+[['自动检测',stats.active.total+stats.passive.total,'主动 + 被动'],['主动探测',stats.active.total,'健康 '+number(stats.active.healthy)+' · 错误 '+number(stats.active.errors)],['被动审计',stats.passive.total,'健康 '+number(stats.passive.healthy)],['探测 Token',stats.active.output_tokens,'包含推理 Token'],['异常命中',stats.active.soft+stats.active.hard+stats.passive.soft+stats.passive.hard,'软 / 硬'],['隔离操作',stats.actions.quarantined,'恢复 '+number(stats.actions.restored)]].map((x)=>'<div class="stat"><span class="metric-label">'+x[0]+'</span><div class="metric-value">'+number(x[1])+'</div><span class="subtext">'+esc(x[2])+'</span></div>').join('')+'</div></section><section class="section"><div class="section-head"><div><h2>守护策略</h2><p>Sidecar 自动重载</p></div><button class="button" data-action="policy-edit">编辑</button></div><div class="stat-grid">'+[['模式',cfg.mode],['主动间隔',(cfg.active_interval_seconds||0)+' 秒'],['软 / 硬阈值',(cfg.soft_tps||0)+' / '+(cfg.hard_tps||0)],['连续异常',(cfg.consecutive_soft||0)+' / '+(cfg.consecutive_errors||0)],['隔离复测',(cfg.quarantine_seconds||0)+' 秒'],['最低健康',cfg.min_healthy_nodes]].map((x)=>'<div class="stat"><span class="metric-label">'+x[0]+'</span><div class="metric-value">'+esc(x[1]??'-')+'</div></div>').join('')+'</div></section></div></div>';
    $('node-search').addEventListener('input', (event)=>{state.nodeSearch=event.target.value;renderEgress();});
    $('node-select-all').addEventListener('change',(event)=>{nodes.forEach((node)=>event.target.checked?state.nodeSelected.add(node.id):state.nodeSelected.delete(node.id));renderEgress();});
  }

  function editNode(node = null) {
    openForm(node?'编辑节点':'添加节点', '创建时代理 URL 必填；编辑留空表示保留现有代理。', checkField('node-enabled','启用节点',node?node.enabled:true)+field('node-name','名称',node?.name||'',{required:true})+field('node-capacity','账号容量',node?.accountCapacity||0,{type:'number',min:0,help:'0 表示不限制'})+field('node-proxy','代理 URL','',{type:'password',full:true,required:!node,help:'支持 HTTP、HTTPS 与 SOCKS5'})+checkField('node-pool','代理池模式',Boolean(node?.proxyPool),'动态轮换代理开启；固定代理关闭'), async (button)=>{
      const proxy=currentValue('node-proxy').trim(); const body={name:currentValue('node-name').trim(),scope:'grok_build',enabled:checked('node-enabled'),proxyPool:checked('node-pool'),accountCapacity:Number(currentValue('node-capacity')),userAgent:'',cloudflareCookies:''}; if(proxy)body.proxyURL=proxy;
      if(!body.name||(!node&&!proxy)){toast('请填写名称和代理 URL','error');return;}
      await runMutation(button,()=>api(node?'/nodes/'+encodeURIComponent(node.id):'/nodes',{method:node?'PUT':'POST',body}),{close:'form-dialog',message:node?'节点已更新':'节点已添加'});
    });
  }

  function editPolicy() {
    const cfg=state.guard?.config||{};
    openForm('守护策略','严格模式只允许真实模型质量检测健康后恢复。',field('policy-mode','模式',cfg.mode,{select:[['hybrid','混合'],['passive','被动'],['active','主动']]})+field('policy-active','主动检测间隔（秒）',cfg.active_interval_seconds,{type:'number',min:60})+field('policy-passive','审计轮询（秒）',cfg.passive_poll_seconds,{type:'number',min:1})+field('policy-quarantine','隔离复测（秒）',cfg.quarantine_seconds,{type:'number',min:30})+field('policy-soft','软阈值 Token/s',cfg.soft_tps,{type:'number',min:1})+field('policy-hard','硬阈值 Token/s',cfg.hard_tps,{type:'number',min:2})+field('policy-soft-count','连续软异常',cfg.consecutive_soft,{type:'number',min:1})+field('policy-error-count','连续错误',cfg.consecutive_errors,{type:'number',min:1})+field('policy-min-healthy','最低健康节点',cfg.min_healthy_nodes,{type:'number',min:1,full:true}),async(button)=>{
      const body={mode:currentValue('policy-mode'),activeIntervalSeconds:Number(currentValue('policy-active')),passivePollSeconds:Number(currentValue('policy-passive')),quarantineSeconds:Number(currentValue('policy-quarantine')),softTPS:Number(currentValue('policy-soft')),hardTPS:Number(currentValue('policy-hard')),consecutiveSoft:Number(currentValue('policy-soft-count')),consecutiveErrors:Number(currentValue('policy-error-count')),minHealthyNodes:Number(currentValue('policy-min-healthy'))};
      if(body.softTPS>=body.hardTPS){toast('软阈值必须低于硬阈值','error');return;}
      await runMutation(button,()=>api('/quality-guard/config',{method:'PUT',body}),{close:'form-dialog',message:'守护策略已保存'});
    });
  }

  async function loadModels() {
    const query=new URLSearchParams({page:'1',pageSize:'200'}); if(state.modelSearch)query.set('search',state.modelSearch);if(state.modelProvider)query.set('provider',state.modelProvider);if(state.modelStatus)query.set('status',state.modelStatus);
    const page=await api('/models?'+query);state.models=page.items||[];state.modelTotal=page.total||0;state.modelSelected=new Set([...state.modelSelected].filter((id)=>state.models.some((item)=>item.id===id)));renderModels();
  }

  function renderModels(){
    const all=state.models.length&&state.models.every((item)=>state.modelSelected.has(item.id));
    root.innerHTML='<section class="section"><div class="section-head"><div><h2>模型路由</h2><p>同步上游模型并控制公开模型 ID</p></div><div class="toolbar"><input id="model-search" class="search" type="search" placeholder="搜索模型" value="'+esc(state.modelSearch)+'"><select id="model-provider" class="filter-select"><option value="">全部 Provider</option>'+[['grok_build','Build'],['grok_web','Web'],['grok_console','Console']].map((x)=>'<option value="'+x[0]+'" '+(state.modelProvider===x[0]?'selected':'')+'>'+x[1]+'</option>').join('')+'</select><select id="model-status" class="filter-select"><option value="">全部状态</option><option value="enabled" '+(state.modelStatus==='enabled'?'selected':'')+'>已启用</option><option value="disabled" '+(state.modelStatus==='disabled'?'selected':'')+'>已停用</option></select><button class="button" data-action="model-sync">同步</button><button class="button primary" data-action="model-add">添加模型</button></div></div><div class="batch-bar" '+(state.modelSelected.size?'':'hidden')+'><span>已选择 '+state.modelSelected.size+' 个模型</span><div class="batch-actions"><button class="button" data-action="model-enable">启用</button><button class="button" data-action="model-disable">停用</button><button class="button danger" data-action="model-delete">删除</button></div></div><div class="table-wrap"><table><thead><tr><th class="check-cell"><input id="model-select-all" type="checkbox" '+(all?'checked':'')+'></th><th>公开模型</th><th>上游模型</th><th>状态</th><th>Provider</th><th class="numeric">支持账号</th><th>最近同步</th><th style="text-align:right">操作</th></tr></thead><tbody>'+(state.models.length?state.models.map((model)=>'<tr data-id="'+esc(model.id)+'"><td class="check-cell"><input type="checkbox" data-action="model-select" '+(state.modelSelected.has(model.id)?'checked':'')+'></td><td><span class="name-cell">'+esc(model.publicId)+'</span><span class="subtext">'+esc(model.capability)+' · '+esc(model.origin)+'</span></td><td><span class="name-cell">'+esc(model.upstreamModel)+'</span></td><td>'+badge(model.enabled?(model.available?'可用':'不可用'):'已停用',model.enabled?(model.available?'good':'warn'):'muted')+'</td><td>'+esc(model.provider)+'</td><td class="numeric">'+number(model.supportedAccounts)+' / '+number(model.totalAccounts)+'</td><td>'+relative(model.lastSyncedAt)+'</td><td><div class="row-actions"><button class="button ghost" data-action="model-edit">编辑</button><button class="button danger" data-action="model-delete-one">删除</button></div></td></tr>').join(''):'<tr><td colspan="8" class="empty">没有匹配的模型</td></tr>')+'</tbody></table></div></section>';
    $('model-search').addEventListener('input',debounce((e)=>{state.modelSearch=e.target.value.trim();loadCurrent(true);},280));$('model-provider').addEventListener('change',(e)=>{state.modelProvider=e.target.value;loadCurrent(true);});$('model-status').addEventListener('change',(e)=>{state.modelStatus=e.target.value;loadCurrent(true);});$('model-select-all').addEventListener('change',(e)=>{state.models.forEach((m)=>e.target.checked?state.modelSelected.add(m.id):state.modelSelected.delete(m.id));renderModels();});
  }

  function editModel(model=null){
    const immutable=Boolean(model);
    openForm(model?'编辑模型':'添加模型',model?'编辑公开模型 ID、状态与账号绑定。':'创建自定义模型路由。',field('model-public','公开模型 ID',model?.publicId||'',{required:true})+field('model-upstream','上游模型',model?.upstreamModel||'',{required:true,full:immutable})+field('model-provider-edit','Provider',model?.provider||'grok_build',{select:[['grok_build','Build'],['grok_web','Web'],['grok_console','Console']]})+field('model-capability','能力',model?.capability||'responses',{select:[['responses','Responses'],['chat','Chat'],['image','Image'],['video','Video']]})+checkField('model-enabled-edit','启用模型',model?model.enabled:true),async(button)=>{
      const createBody={publicId:currentValue('model-public').trim(),provider:currentValue('model-provider-edit'),upstreamModel:currentValue('model-upstream').trim(),capability:currentValue('model-capability'),enabled:checked('model-enabled-edit'),accountIds:model?.accountIds||[]};
      const body=model?{publicId:createBody.publicId,enabled:createBody.enabled,accountIds:createBody.accountIds}:createBody;
      await runMutation(button,()=>api(model?'/models/'+encodeURIComponent(model.id):'/models',{method:model?'PATCH':'POST',body}),{close:'form-dialog',message:model?'模型已更新':'模型已创建'});
    });
    if(immutable){$('model-upstream').disabled=true;$('model-provider-edit').disabled=true;$('model-capability').disabled=true;}
  }

  async function loadKeys(){const query=new URLSearchParams({page:'1',pageSize:'200'});if(state.keySearch)query.set('search',state.keySearch);if(state.keyStatus)query.set('status',state.keyStatus);const page=await api('/client-keys?'+query);state.keys=page.items||[];state.keyTotal=page.total||0;state.keySelected=new Set([...state.keySelected].filter((id)=>state.keys.some((item)=>item.id===id)));renderKeys();}

  function renderKeys(){const all=state.keys.length&&state.keys.every((item)=>state.keySelected.has(item.id));root.innerHTML='<section class="section"><div class="section-head"><div><h2>Client Key</h2><p>密钥正文仅在创建或显式查看时显示</p></div><div class="toolbar"><input id="key-search" class="search" type="search" placeholder="搜索名称或前缀" value="'+esc(state.keySearch)+'"><select id="key-status" class="filter-select"><option value="">全部状态</option><option value="enabled" '+(state.keyStatus==='enabled'?'selected':'')+'>已启用</option><option value="disabled" '+(state.keyStatus==='disabled'?'selected':'')+'>已停用</option></select><button class="button primary" data-action="key-add">创建 Key</button></div></div><div class="batch-bar" '+(state.keySelected.size?'':'hidden')+'><span>已选择 '+state.keySelected.size+' 个 Key</span><div class="batch-actions"><button class="button" data-action="key-enable">启用</button><button class="button" data-action="key-disable">停用</button><button class="button danger" data-action="key-delete">删除</button></div></div><div class="table-wrap"><table><thead><tr><th class="check-cell"><input id="key-select-all" type="checkbox" '+(all?'checked':'')+'></th><th>名称</th><th>状态</th><th>前缀</th><th class="numeric">RPM</th><th class="numeric">并发</th><th>账号范围</th><th>最近使用</th><th style="text-align:right">操作</th></tr></thead><tbody>'+(state.keys.length?state.keys.map((key)=>'<tr data-id="'+esc(key.id)+'"><td class="check-cell"><input type="checkbox" data-action="key-select" '+(state.keySelected.has(key.id)?'checked':'')+'></td><td><span class="name-cell">'+esc(key.name)+'</span></td><td>'+badge(key.enabled?'已启用':'已停用',key.enabled?'good':'muted')+'</td><td><span class="name-cell">'+esc(key.prefix)+'...</span></td><td class="numeric">'+(key.rpmLimit||'∞')+'</td><td class="numeric">'+(key.maxConcurrent||'∞')+'</td><td>'+esc((key.providerScope||['all']).join(', '))+'<span class="subtext">'+esc((key.tierScope||['all']).join(', '))+'</span></td><td>'+relative(key.lastUsedAt)+'</td><td><div class="row-actions"><button class="button" data-action="key-secret">查看</button><button class="button ghost" data-action="key-edit">编辑</button><button class="button danger" data-action="key-delete-one">删除</button></div></td></tr>').join(''):'<tr><td colspan="9" class="empty">暂无 Client Key</td></tr>')+'</tbody></table></div></section>';$('key-search').addEventListener('input',debounce((e)=>{state.keySearch=e.target.value.trim();loadCurrent(true);},280));$('key-status').addEventListener('change',(e)=>{state.keyStatus=e.target.value;loadCurrent(true);});$('key-select-all').addEventListener('change',(e)=>{state.keys.forEach((k)=>e.target.checked?state.keySelected.add(k.id):state.keySelected.delete(k.id));renderKeys();});}

  function editKey(key=null){openForm(key?'编辑 Client Key':'创建 Client Key','RPM 或并发填写 0 表示不限制。',field('key-name-edit','名称',key?.name||'',{required:true})+field('key-rpm-edit','RPM 限制',key?.rpmLimit||0,{type:'number',min:0})+field('key-concurrency-edit','最大并发',key?.maxConcurrent||0,{type:'number',min:0})+field('key-billing-edit','计费上限（USD ticks）',key?.billingLimitUsdTicks||0,{type:'number',min:0})+field('key-expires-edit','过期时间（RFC3339，可空）',key?.expiresAt||'',{full:true})+field('key-provider-edit','Provider 范围',(key?.providerScope||['all']).join(','),{full:true,help:'all 或 grok_build,grok_web,grok_console'})+field('key-tier-edit','层级范围',(key?.tierScope||['all']).join(','),{full:true,help:'all 或 free,super'})+checkField('key-alias-edit','允许模型别名',key?.allowModelAliases??true)+checkField('key-enabled-edit','启用 Key',key?key.enabled:true),async(button)=>{const body={name:currentValue('key-name-edit').trim(),enabled:checked('key-enabled-edit'),expiresAt:currentValue('key-expires-edit').trim(),rpmLimit:Number(currentValue('key-rpm-edit')),maxConcurrent:Number(currentValue('key-concurrency-edit')),billingLimitUsdTicks:Number(currentValue('key-billing-edit')),allowModelAliases:checked('key-alias-edit'),allowedModelIds:key?.allowedModelIds||[],providerScope:currentValue('key-provider-edit').split(',').map(x=>x.trim()).filter(Boolean),tierScope:currentValue('key-tier-edit').split(',').map(x=>x.trim()).filter(Boolean)};const result=await runMutation(button,()=>api(key?'/client-keys/'+encodeURIComponent(key.id):'/client-keys',{method:key?'PATCH':'POST',body}),{close:'form-dialog',message:key?'Key 已更新':'Key 已创建'});if(!key&&result?.secret)showDetail('新 Client Key','只会在此完整显示一次，请立即保存。',{secret:result.secret});});}

  async function loadAudits(){const query=new URLSearchParams({pagination:'cursor',pageSize:'50',period:state.auditPeriod});if(state.auditCursor)query.set('cursor',state.auditCursor);if(state.auditSearch)query.set('search',state.auditSearch);if(state.auditStatus)query.set('status',state.auditStatus);const summaryQuery=new URLSearchParams({period:state.auditPeriod});if(state.auditSearch)summaryQuery.set('search',state.auditSearch);if(state.auditStatus)summaryQuery.set('status',state.auditStatus);const [page,summary]=await Promise.all([api('/request-audits?'+query),api('/request-audits/summary?'+summaryQuery)]);state.audits=page.items||[];state.auditNextCursor=page.nextCursor||'';state.auditHasMore=Boolean(page.hasMore);state.auditSummary=summary;renderAudits();}

  function renderAudits(){const usage=state.auditSummary?.usage||{};root.innerHTML='<div class="stack"><section class="metrics">'+metric('请求数',number(usage.requests),'成功 '+number(usage.successfulRequests)+' · 失败 '+number(usage.failedRequests),usage.failedRequests?'bad':'')+metric('成功率',number(usage.successRate,1)+'%','当前筛选条件')+metric('平均耗时',number(usage.averageDurationMs)+' ms','总 Token '+number(usage.totalTokens))+metric('估算费用',money(usage.estimatedCostInUsdTicks),'当前周期')+'</section><section class="section"><div class="section-head"><div><h2>请求记录</h2><p>点击记录查看每次上游尝试与错误链</p></div><div class="toolbar"><div class="segmented">'+['24h','7d','30d','90d'].map((p)=>'<button class="button '+(state.auditPeriod===p?'active':'')+'" data-action="audit-period" data-value="'+p+'">'+p+'</button>').join('')+'</div><input id="audit-search" class="search" type="search" placeholder="搜索请求、模型或账号" value="'+esc(state.auditSearch)+'"><select id="audit-status" class="filter-select"><option value="">全部状态</option><option value="success" '+(state.auditStatus==='success'?'selected':'')+'>成功</option><option value="error" '+(state.auditStatus==='error'?'selected':'')+'>失败</option></select></div></div><div class="table-wrap"><table><thead><tr><th>时间</th><th>模型</th><th>账号 / 出口</th><th>状态</th><th class="numeric">首字</th><th class="numeric">Token/s</th><th class="numeric">Token</th><th class="numeric">尝试</th><th style="text-align:right">操作</th></tr></thead><tbody>'+(state.audits.length?state.audits.map((audit)=>'<tr data-id="'+esc(audit.id)+'"><td>'+time(audit.createdAt)+'</td><td><span class="name-cell">'+esc(audit.modelPublicId||audit.modelUpstreamModel||'-')+'</span><span class="subtext">'+esc(audit.provider)+' · '+esc(audit.operation)+'</span></td><td><span class="name-cell">'+esc(audit.accountName||'-')+'</span><span class="subtext">'+esc(audit.egressNodeName||audit.egressMode||'-')+'</span></td><td>'+badge(String(audit.statusCode),audit.statusCode>=200&&audit.statusCode<400?'good':'bad')+'<span class="subtext">'+esc(audit.errorCode||'')+'</span></td><td class="numeric">'+(audit.firstTokenMs?number(audit.firstTokenMs)+' ms':'-')+'</td><td class="numeric '+(audit.outputTokensPerSecond>=500?'tone-bad':'')+'">'+(audit.outputTokensPerSecond?number(audit.outputTokensPerSecond,1):'-')+'</td><td class="numeric">'+number(audit.totalTokens)+'</td><td class="numeric">'+number(audit.attemptCount)+'</td><td><div class="row-actions"><button class="button" data-action="audit-detail">详情</button></div></td></tr>').join(''):'<tr><td colspan="9" class="empty">没有匹配的审计记录</td></tr>')+'</tbody></table></div><div class="pagination"><span>当前加载 '+state.audits.length+' 条</span><div class="row-actions"><button class="button" data-action="audit-prev" '+(state.auditHistory.length?'':'disabled')+'>上一页</button><button class="button" data-action="audit-next" '+(state.auditHasMore?'':'disabled')+'>下一页</button></div></div></section></div>';$('audit-search').addEventListener('input',debounce((e)=>{state.auditSearch=e.target.value.trim();state.auditCursor='';state.auditHistory=[];loadCurrent(true);},280));$('audit-status').addEventListener('change',(e)=>{state.auditStatus=e.target.value;state.auditCursor='';state.auditHistory=[];loadCurrent(true);});}

  async function loadSettings(){state.settings=await api('/settings');root.innerHTML='<div class="stack">'+(state.settings.restartRequired?.length?'<div class="notice">这些设置需要重启后生效：'+esc(state.settings.restartRequired.join('、'))+'</div>':'')+'<section class="section"><div class="section-head"><div><h2>完整运行配置</h2><p>保存时携带 revision，防止覆盖其他管理员的并发修改</p></div><button class="button primary" data-action="settings-save">保存设置</button></div><div class="dialog-body"><div class="field"><label for="settings-json">配置 JSON</label><textarea id="settings-json" style="min-height:calc(100vh - 15rem)">'+esc(JSON.stringify(state.settings.config,null,2))+'</textarea><p class="help">敏感字段由 Grok2API 响应层脱敏，未配置的密钥不会在此回显。</p></div></div></section></div>';}

  function debounce(fn, delay){return (...args)=>{clearTimeout(state.debounceTimer);state.debounceTimer=window.setTimeout(()=>fn(...args),delay);};}
  function rowItem(event, list){const row=event.target.closest('tr[data-id]');return row?list.find((item)=>item.id===row.dataset.id):null;}

  root.addEventListener('click', async (event) => {
    const button=event.target.closest('[data-action]');if(!button)return;const action=button.dataset.action;
    try {
      if(action==='dashboard-period'){state.dashboardPeriod=button.dataset.value;await loadCurrent(true);return;}
      if(action==='account-provider'){state.accountsProvider=button.dataset.value;state.accountsPage=1;state.accountSelected.clear();await loadCurrent(true);return;}
      if(action==='account-prev'&&state.accountsPage>1){state.accountsPage--;await loadCurrent(true);return;}
      if(action==='account-next'){state.accountsPage++;await loadCurrent(true);return;}
      const account=rowItem(event,state.accounts);
      if(action==='account-select'&&account){button.checked?state.accountSelected.add(account.id):state.accountSelected.delete(account.id);renderAccounts();return;}
      if(action==='account-detail'&&account){showDetail('账号详情',account.name,account);return;}
      if(action==='account-edit'&&account){editAccount(account);return;}
      if(action==='account-enable'){await updateAccountBatch(true,button);return;}
      if(action==='account-disable'){await updateAccountBatch(false,button);return;}
      if(action==='account-export'){exportAccounts();return;}
      if(action==='account-bind'){bindAccounts();return;}
      if(action==='account-quota'){await runMutation(button,()=>api('/accounts/batch/refresh-quotas',{method:'POST',body:{ids:selectedAccounts(),provider:state.accountsProvider}}),{message:'额度刷新完成'});return;}
      if(action==='account-token'){await runMutation(button,()=>api('/accounts/batch/refresh-tokens',{method:'POST',body:{ids:selectedAccounts(),provider:state.accountsProvider}}),{message:'凭据刷新完成'});return;}
      if(action==='account-delete'){const ids=selectedAccounts();confirmAction('删除选中账号','将删除 '+ids.length+' 个账号，此操作不可撤销。','<div class="notice">只删除当前 Provider 中明确选中的账号，不会静默扩大范围。</div>',async(confirmButton)=>{await runMutation(confirmButton,()=>api('/accounts',{method:'DELETE',body:{ids,provider:state.accountsProvider,linkedDeleteTargets:[]}}),{close:'confirm-dialog',message:'账号已删除'});state.accountSelected.clear();},'删除账号');return;}

      const node=rowItem(event,state.nodes);
      if(action==='node-select'&&node){button.checked?state.nodeSelected.add(node.id):state.nodeSelected.delete(node.id);renderEgress();return;}
      if(action==='node-add'){editNode();return;}if(action==='node-edit'&&node){editNode(node);return;}if(action==='policy-edit'){editPolicy();return;}
      if(action==='node-quality'&&node){await runMutation(button,()=>api('/quality-guard/nodes/'+encodeURIComponent(node.id)+'/test',{method:'POST'}),{message:'质量检测完成'});return;}
      if(action==='node-test'&&node){await runMutation(button,()=>api('/nodes/'+encodeURIComponent(node.id)+'/test',{method:'POST'}),{message:'连通性检测完成'});return;}
      if(action==='node-toggle'&&node){await runMutation(button,()=>api('/nodes/batch',{method:'PATCH',body:{ids:[node.id],enabled:!node.enabled}}),{message:node.enabled?'节点已停用':'节点已启用'});return;}
      if(action==='node-enable'||action==='node-disable'){await runMutation(button,()=>api('/nodes/batch',{method:'PATCH',body:{ids:[...state.nodeSelected],enabled:action==='node-enable'}}),{message:'节点状态已更新'});state.nodeSelected.clear();return;}
      if(action==='node-test-batch'){await runMutation(button,()=>api('/nodes/test',{method:'POST',body:{ids:[...state.nodeSelected]}}),{message:'批量检测完成'});return;}
      if(action==='node-delete-batch'){const ids=[...state.nodeSelected];confirmAction('删除出口节点','将删除 '+ids.length+' 个节点。','<div class="notice">已绑定账号会失去该出口分配。</div>',async(confirmButton)=>{await runMutation(confirmButton,()=>api('/nodes',{method:'DELETE',body:{ids}}),{close:'confirm-dialog',message:'节点已删除'});state.nodeSelected.clear();},'删除节点');return;}

      const model=rowItem(event,state.models);
      if(action==='model-select'&&model){button.checked?state.modelSelected.add(model.id):state.modelSelected.delete(model.id);renderModels();return;}if(action==='model-add'){editModel();return;}if(action==='model-edit'&&model){editModel(model);return;}
      if(action==='model-sync'){await runMutation(button,()=>api('/models/sync',{method:'POST'}),{message:'模型同步完成'});return;}
      if(action==='model-enable'||action==='model-disable'){await runMutation(button,()=>api('/models/batch',{method:'PATCH',body:{ids:[...state.modelSelected],enabled:action==='model-enable'}}),{message:'模型状态已更新'});state.modelSelected.clear();return;}
      if(action==='model-delete'||action==='model-delete-one'){const ids=action==='model-delete-one'&&model?[model.id]:[...state.modelSelected];confirmAction('删除模型','将删除 '+ids.length+' 个模型路由。','',async(confirmButton)=>{await runMutation(confirmButton,()=>api('/models',{method:'DELETE',body:{ids}}),{close:'confirm-dialog',message:'模型已删除'});state.modelSelected.clear();},'删除模型');return;}

      const key=rowItem(event,state.keys);
      if(action==='key-select'&&key){button.checked?state.keySelected.add(key.id):state.keySelected.delete(key.id);renderKeys();return;}if(action==='key-add'){editKey();return;}if(action==='key-edit'&&key){editKey(key);return;}
      if(action==='key-secret'&&key){const result=await runMutation(button,()=>api('/client-keys/'+encodeURIComponent(key.id)+'/secret'),{reload:false});showDetail('Client Key',key.name,{secret:result.secret});return;}
      if(action==='key-enable'||action==='key-disable'){await runMutation(button,()=>api('/client-keys/batch',{method:'PATCH',body:{ids:[...state.keySelected],enabled:action==='key-enable'}}),{message:'Key 状态已更新'});state.keySelected.clear();return;}
      if(action==='key-delete'||action==='key-delete-one'){const ids=action==='key-delete-one'&&key?[key.id]:[...state.keySelected];confirmAction('删除 Client Key','将删除 '+ids.length+' 个 Client Key。','',async(confirmButton)=>{await runMutation(confirmButton,()=>api('/client-keys',{method:'DELETE',body:{ids}}),{close:'confirm-dialog',message:'Key 已删除'});state.keySelected.clear();},'删除 Key');return;}

      if(action==='audit-period'){state.auditPeriod=button.dataset.value;state.auditCursor='';state.auditHistory=[];await loadCurrent(true);return;}
      if(action==='audit-next'&&state.auditHasMore){state.auditHistory.push(state.auditCursor);state.auditCursor=state.auditNextCursor;await loadCurrent(true);return;}
      if(action==='audit-prev'&&state.auditHistory.length){state.auditCursor=state.auditHistory.pop()||'';await loadCurrent(true);return;}
      const audit=rowItem(event,state.audits);if(action==='audit-detail'&&audit){const detail=await runMutation(button,()=>api('/request-audits/'+encodeURIComponent(audit.id)),{reload:false});showDetail('请求审计详情',audit.requestId,detail);return;}
      if(action==='settings-save'){let config;try{config=JSON.parse(currentValue('settings-json'));}catch(_){toast('配置 JSON 格式无效','error');return;}confirmAction('保存完整配置','将使用 revision '+state.settings.revision+' 提交。','<div class="notice">Grok2API 会拒绝覆盖更新后的 revision；需要重启的字段会在保存后列出。</div>',async(confirmButton)=>{await runMutation(confirmButton,()=>api('/settings',{method:'PUT',body:{revision:state.settings.revision,config}}),{close:'confirm-dialog',message:'设置已保存'});},'保存设置');}
    } catch (_) {}
  });

  document.querySelectorAll('.nav-item').forEach((item)=>item.addEventListener('click',()=>navigate(item.dataset.view)));
  $('refresh-button').addEventListener('click',()=>loadCurrent());
  $('theme-button').addEventListener('click',()=>{const dark=document.documentElement.dataset.theme==='dark';document.documentElement.dataset.theme=dark?'light':'dark';localStorage.setItem('grok2api-workbench-theme',dark?'light':'dark');});
  $('menu-button').addEventListener('click',()=>document.body.classList.add('sidebar-open'));
  $('sidebar-scrim').addEventListener('click',()=>document.body.classList.remove('sidebar-open'));
  $('form-dialog-submit').addEventListener('click',async(event)=>{event.preventDefault();if(state.formHandler)await state.formHandler(event.currentTarget);});
  $('confirm-dialog-submit').addEventListener('click',async(event)=>{event.preventDefault();if(state.confirmHandler)await state.confirmHandler(event.currentTarget);});
  document.addEventListener('keydown',(event)=>{if(event.key==='Escape')document.body.classList.remove('sidebar-open');});
  const savedTheme=localStorage.getItem('grok2api-workbench-theme');document.documentElement.dataset.theme=savedTheme||(matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light');
  const initial=location.hash.slice(1);navigate(VIEW_META[initial]?initial:'dashboard');
  state.refreshTimer=window.setInterval(()=>{if(document.visibilityState==='visible'&&!document.querySelector('dialog[open]'))loadCurrent(true);},30000);
})();
