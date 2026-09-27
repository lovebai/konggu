'use strict';
const $ = (s) => document.querySelector(s);
const $$ = (s) => [...document.querySelectorAll(s)];
const state = { user: null, csrf: '', view: 'entry', categories: [], page: 1, total: 0, photo: '', uploading: false, scanner: null, scanReader: null, scanGeneration: 0, requestGeneration: 0, lookupGeneration: 0 };
const titles = { entry: ['出入库登记', '记录每一次物品流转，让库存始终有序。'], records: ['流转记录', '查询、核对每一笔出入库记录。'], stock: ['库存总览', '按物品查看当前可用库存，数量随有效流水实时更新。'], categories: ['物品分类', '统一分类，让每件物品都有归属。'], users: ['账号管理', '为团队分配账号与访问权限。'], trash: ['回收站', '删除的记录保留在此，恢复时会重新校验库存。'], audit: ['操作日志', '关键操作留痕，便于核对与追溯。'] };
const escapeHTML = (v) => String(v ?? '').replace(/[&<>"']/g, (s) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[s]));
const displayName = (u) => u.display_name?.trim() || u.username;
const date = (v) => new Date(v).toLocaleString('zh-CN', { hour12: false });
let toastTimer;
function toast(message, error = false) { $('#toast').textContent = message; $('#toast').classList.toggle('error', error); $('#toast').hidden = false; clearTimeout(toastTimer); toastTimer = setTimeout(() => $('#toast').hidden = true, error ? 6500 : 3500); }
async function api(path, options = {}) {
  const headers = { ...options.headers };
  if (options.body && !(options.body instanceof FormData)) { headers['Content-Type'] = 'application/json'; options.body = JSON.stringify(options.body); }
  if (options.method && options.method !== 'GET') headers['X-CSRF-Token'] = state.csrf;
  const response = await fetch('/api' + path, { ...options, headers, credentials: 'same-origin' });
  if (!response.ok) { let detail; try { detail = await response.json(); } catch { detail = {}; } if (response.status === 401 && path !== '/login') showLogin(); throw new Error(detail.error || '请求失败，请稍后重试'); }
  return options.blob ? response.blob() : response.json();
}
function safe(fn) { return (...args) => { if (args[0]?.type === 'submit') args[0].preventDefault(); return Promise.resolve().then(() => fn(...args)).catch((e) => toast(e.message, true)); }; }
async function busy(button, fn) { button.disabled = true; try { return await fn(); } finally { button.disabled = false; } }
function showLogin() { state.user = null; state.csrf = ''; state.photo = ''; stopScan(); $$('dialog[open]').forEach(d => d.close()); $('#app-view').hidden = true; $('#login-view').hidden = false; $('#boot').hidden = true; }
async function signedIn(data) {
  state.user = data.user; state.csrf = data.csrf; $('#login-view').hidden = true; $('#app-view').hidden = false; $('#boot').hidden = true;
  $('#account-button').textContent = displayName(state.user); $('#avatar').textContent = displayName(state.user)[0]; $('#role').textContent = state.user.role === 'admin' ? '管理员' : '普通用户';
  $$('.admin-only').forEach(e => e.hidden = state.user.role !== 'admin'); $('#upload-help').textContent = `支持 JPG、PNG、GIF，最大 ${data.max_upload_mb} MB`;
  $('#entry-form').reset(); clearPhoto(); unlockProduct(); await loadCategories(); await showView('entry');
}
async function loadCategories() {
  state.categories = await api('/categories');
  for (const selector of ['[name=category_id]', '#filter-category']) { const el = $(selector); const previous = el.value; el.innerHTML = `<option value="">${selector === '#filter-category' ? '全部分类' : '请选择分类'}</option>` + state.categories.map(c => `<option value="${c.id}">${escapeHTML(c.name)}</option>`).join(''); el.value = previous; }
}
async function stats() { const d = await api('/stats'); $('#stat-records').textContent = d.records.toLocaleString(); $('#stat-in').textContent = d.incoming.toLocaleString(); $('#stat-out').textContent = d.outgoing.toLocaleString(); }
async function showView(view) {
  if (!titles[view]) return;
  if (['users', 'categories', 'trash', 'audit'].includes(view) && state.user?.role !== 'admin') return;
  state.view = view; state.requestGeneration++; $$('section.view').forEach(e => e.hidden = true); $(`#view-${view === 'trash' ? 'records' : view}`).hidden = false;
  $$('[data-view]').forEach(e => e.classList.toggle('active', e.dataset.view === view)); $('#page-heading').textContent = titles[view][0]; $('#breadcrumb').textContent = titles[view][0]; $('#page-description').textContent = titles[view][1];
  $('#stats').hidden = !['entry', 'records', 'stock'].includes(view);
  if (view === 'entry') await Promise.all([stats(), recent()]);
  if (view === 'records' || view === 'trash') { state.page = 1; $('#clear-trash').hidden = view !== 'trash'; $('#export-button').hidden = state.user.role !== 'admin'; await Promise.all([loadRecords(), view === 'records' ? stats() : Promise.resolve()]); }
  if (view === 'stock') await Promise.all([loadStock(), stats()]);
  if (view === 'categories') await renderCategories();
  if (view === 'users') await loadUsers();
  if (view === 'audit') await loadAudit();
}
function addTimeRange(p, start, end) { const from = start ? new Date(start) : null; const to = end ? new Date(end) : null; if ((from && isNaN(from)) || (to && isNaN(to))) throw new Error('请选择有效日期时间'); if (from && to && from > to) throw new Error('结束时间不能早于开始时间'); if (from) p.set('start', from.toISOString()); if (to) p.set('end', to.toISOString()); }
function filters(includeTime = true) { const p = new URLSearchParams(); if ($('#record-search').value.trim()) p.set('q', $('#record-search').value.trim()); if ($('#filter-category').value) p.set('category_id', $('#filter-category').value); if ($('#filter-direction').value) p.set('direction', $('#filter-direction').value); if (state.view === 'trash') p.set('trash', '1'); if (includeTime) addTimeRange(p, $('#filter-start').value, $('#filter-end').value); return p; }
function badge(r) { return `<span class="badge ${r.direction === 'out' ? 'out' : ''}">${r.direction === 'in' ? '入库 +' : '出库 −'}${r.quantity} ${escapeHTML(r.product.unit)}</span>`; }
async function recent() { const d = await api('/records'); $('#recent-list').innerHTML = d.items.slice(0, 4).map(r => `<div class="recent-item"><div><strong>${escapeHTML(r.product.name)}</strong><small>${escapeHTML(date(r.occurred_at || r.created_at))}</small></div>${badge(r)}</div>`).join('') || '<div class="empty">还没有登记记录<br><small>从第一件物品开始吧。</small></div>'; }
async function loadRecords() {
  const generation = ++state.requestGeneration; const p = filters(); p.set('page', state.page); const d = await api('/records?' + p); if (generation !== state.requestGeneration) return; state.total = d.total;
  $('#records-body').innerHTML = d.items.map(r => `<tr><td><div class="item-cell">${r.photo ? `<img class="item-thumb" src="/uploads/${encodeURIComponent(r.photo)}" data-photo="${escapeHTML(r.photo)}" alt="${escapeHTML(r.product.name)}" loading="lazy">` : '<span class="no-thumb">▦</span>'}<div>${escapeHTML(r.product.name)}<small>${escapeHTML(r.product.barcode)}</small></div></div></td><td>${escapeHTML(r.category.name)}</td><td>${badge(r)}</td><td>${escapeHTML(displayName(r.user))}</td><td>${escapeHTML(date(r.occurred_at || r.created_at))}<small title="登记时间">登记：${escapeHTML(date(r.created_at))} · #${r.id}</small></td><td class="note-cell">${escapeHTML(r.source || '未填写（历史记录）')}</td><td class="note-cell">${escapeHTML(r.note || '—')}</td><td>${state.user.role === 'admin' ? `<button class="text-button" data-record-id="${r.id}" data-action="${state.view === 'trash' ? 'restore' : 'delete'}">${state.view === 'trash' ? '恢复' : '删除'}</button>` : '—'}</td></tr>`).join('');
  $('#records-empty').hidden = d.items.length > 0; $('#record-total').textContent = `共 ${d.total} 条记录`; $('#page-number').textContent = `${state.page} / ${Math.max(1, Math.ceil(d.total / 20))}`; $('#prev-page').disabled = state.page <= 1; $('#next-page').disabled = state.page * 20 >= d.total;
}
async function loadStock() { const rows = (await api('/products?q=' + encodeURIComponent($('#stock-search').value.trim()))) ?? []; $('#stock-body').innerHTML = rows.map(p => `<tr><td>${escapeHTML(p.name)}</td><td>${escapeHTML(p.barcode)}</td><td>${escapeHTML(p.category_name)}</td><td><b>${p.stock}</b> ${escapeHTML(p.unit)}</td></tr>`).join('') || `<tr><td colspan="4" class="empty">${$('#stock-search').value.trim() ? '未找到匹配的物品，请调整搜索关键词。' : '暂无库存物品，请先登记入库。'}</td></tr>`; }
async function renderCategories() { await loadCategories(); $('#category-list').innerHTML = state.categories.map(c => `<div class="category-card"><span>⊞ &nbsp;${escapeHTML(c.name)}</span><button class="text-button" data-category-id="${c.id}" aria-label="删除${escapeHTML(c.name)}">删除</button></div>`).join('') || '<p class="muted">尚无分类，请添加第一项分类后登记物品。</p>'; }
async function loadUsers() { const users = await api('/admin/users'); $('#users-body').innerHTML = users.map(u => `<tr><td>${escapeHTML(u.username)}</td><td><input class="user-name-input" data-name-for="${u.id}" value="${escapeHTML(u.display_name || '')}" maxlength="50" aria-label="${escapeHTML(u.username)}的姓名" placeholder="默认显示用户名"><button data-save-name="${u.id}">保存姓名</button></td><td>${u.role === 'admin' ? '管理员' : '普通用户'}</td><td><span class="badge ${u.disabled ? 'out' : ''}">${u.disabled ? '已禁用' : '正常'}</span></td><td>${u.id === state.user.id ? '当前账号' : `<button data-user-id="${u.id}" data-disabled="${!u.disabled}">${u.disabled ? '启用' : '禁用'}</button>`}</td></tr>`).join(''); }
async function loadAudit() { const rows = await api('/admin/audit'); const names = { login: '登录', password: '修改密码', 'record.create': '登记物品', 'record.trash': '删除记录', 'record.restore': '恢复记录', 'trash.clear': '清空回收站', 'category.create': '新增分类', 'category.delete': '删除分类', 'user.create': '创建账号', 'user.status': '账号状态变更', 'user.profile': '设置姓名' }; $('#audit-body').innerHTML = rows.map(r => `<tr><td>${escapeHTML(date(r.occurred_at || r.created_at))}</td><td>${r.user_id}</td><td>${escapeHTML(names[r.action] || r.action)}</td><td class="note-cell">${escapeHTML(r.detail)}</td></tr>`).join(''); }
function unlockProduct() { const f = $('#entry-form'); f.elements.name.readOnly = false; f.elements.unit.readOnly = false; f.elements.category_id.disabled = false; $('#product-hint').textContent = '没有条码的物品，可填写自定义唯一编码。'; }
async function lookupProduct() {
  const generation = ++state.lookupGeneration; const f = $('#entry-form'); const barcode = f.elements.barcode.value.trim(); unlockProduct(); if (!barcode) return;
  const d = await api('/products?barcode=' + encodeURIComponent(barcode)); if (generation !== state.lookupGeneration || barcode !== f.elements.barcode.value.trim()) return;
  if (d.product) { f.elements.name.value = d.product.name; f.elements.unit.value = d.product.unit; f.elements.category_id.value = d.product.category_id; f.elements.name.readOnly = true; f.elements.unit.readOnly = true; f.elements.category_id.disabled = true; $('#product-hint').textContent = `已匹配已有物品 · 当前库存 ${d.stock} ${d.product.unit}`; } else { $('#product-hint').textContent = '新物品编码，请填写名称与分类。首次登记请选择入库。'; }
}
function clearPhoto() { state.photo = ''; $('#photo-preview-wrap').hidden = true; $('#photo-preview').removeAttribute('src'); $('#photo-input').value = ''; $('#camera-input').value = ''; $('#upload-area').hidden = false; }
$('#login-form').addEventListener('submit', async e => { e.preventDefault(); $('#login-error').textContent = ''; try { await busy(e.submitter, async () => { const data = await api('/login', { method: 'POST', body: Object.fromEntries(new FormData(e.target)) }); e.target.reset(); await signedIn(data); }); } catch (err) { $('#login-error').textContent = err.message; } });
$('#logout').onclick = safe(async () => { await api('/logout', { method: 'POST' }); showLogin(); });
$$('[data-view]').forEach(b => b.onclick = safe(() => showView(b.dataset.view)));
$('.sidebar .brand').onclick = safe(async e => { e.preventDefault(); await showView('entry'); });
$('#all-records').onclick = safe(() => showView('records'));
$('#today').textContent = new Date().toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric', weekday: 'long' });
let lookupTimer;
$('#entry-form').elements.barcode.addEventListener('change', safe(async () => { clearTimeout(lookupTimer); await lookupProduct(); }));
$('#entry-form').elements.barcode.addEventListener('input', () => { state.lookupGeneration++; unlockProduct(); clearTimeout(lookupTimer); lookupTimer = setTimeout(safe(lookupProduct), 350); });
$('#entry-form').addEventListener('submit', safe(async e => {
  e.preventDefault(); if (state.uploading) throw new Error('图片上传中，请稍候'); const f = e.target;
  await busy($('#save-record'), async () => { const body = Object.fromEntries(new FormData(f)); body.category_id = Number(f.elements.category_id.value); body.quantity = Number(body.quantity); body.photo = state.photo; body.occurred_at = body.occurred_at ? new Date(body.occurred_at).toISOString() : '';
    await api('/records', { method: 'POST', body }); f.reset(); clearPhoto(); unlockProduct(); toast('登记成功，库存已更新'); await Promise.all([stats(), recent()]);
  });
}));
async function uploadPhoto(e) { const file = e.target.files[0]; if (!file) return; if (state.uploading) { e.target.value = ''; throw new Error('图片上传中，请稍候'); } state.uploading = true; $('#save-record').disabled = true; $('#take-photo').disabled = true; $('#choose-photo').disabled = true; $('#upload-help').textContent = '正在上传…'; try { const form = new FormData(); form.append('photo', file); const d = await api('/photos', { method: 'POST', body: form }); state.photo = d.key; $('#photo-preview').src = d.url; $('#photo-preview-wrap').hidden = false; $('#upload-area').hidden = true; toast('图片已上传'); } finally { state.uploading = false; $('#save-record').disabled = false; $('#take-photo').disabled = false; $('#choose-photo').disabled = false; e.target.value = ''; $('#upload-help').textContent = '支持 JPG、PNG、GIF'; } }
$('#photo-input').addEventListener('change', safe(uploadPhoto));
$('#camera-input').addEventListener('change', safe(uploadPhoto));
$('#take-photo').onclick = () => $('#camera-input').click();
$('#choose-photo').onclick = () => $('#photo-input').click();
$('#remove-photo').onclick = clearPhoto;
$('#search-button').onclick = safe(async () => { state.page = 1; await loadRecords(); });
$('#record-search').onkeydown = safe(async e => { if (e.key === 'Enter') { state.page = 1; await loadRecords(); } });
for (const id of ['#filter-category', '#filter-direction', '#filter-start', '#filter-end']) $(id).onchange = safe(async () => { state.page = 1; await loadRecords(); });
$('#prev-page').onclick = safe(async () => { state.page--; await loadRecords(); }); $('#next-page').onclick = safe(async () => { state.page++; await loadRecords(); });
$('#stock-search-button').onclick = safe(loadStock); $('#stock-search').onkeydown = safe(async e => { if (e.key === 'Enter') await loadStock(); });
$('#clear-time-filter').onclick = safe(async () => { $('#filter-start').value = ''; $('#filter-end').value = ''; state.page = 1; await loadRecords(); });
$('#export-button').onclick = () => { $('#export-form').elements.start.value = $('#filter-start').value; $('#export-form').elements.end.value = $('#filter-end').value; $('#export-dialog').showModal(); };
$('#close-export').onclick = () => $('#export-dialog').close();
$('#export-form').onsubmit = safe(async e => { e.preventDefault(); const p = filters(false); addTimeRange(p, e.target.elements.start.value, e.target.elements.end.value); p.set('include_images', e.target.elements.include_images.checked ? '1' : '0'); p.set('tz_offset', new Date().getTimezoneOffset()); await busy(e.submitter, async () => { const blob = await api('/admin/export?' + p, { blob: true }); const url = URL.createObjectURL(blob); const a = document.createElement('a'); a.href = url; a.download = `出入库记录-${new Date().toISOString().slice(0, 10)}.xlsx`; a.click(); setTimeout(() => URL.revokeObjectURL(url), 10000); $('#export-dialog').close(); toast('Excel 导出成功'); }); });
$('#records-body').onclick = safe(async e => { const photo = e.target.closest('[data-photo]'); if (photo) { $('#large-photo').src = '/uploads/' + encodeURIComponent(photo.dataset.photo); $('#photo-dialog').showModal(); return; } const button = e.target.closest('[data-record-id]'); if (!button) return; const restore = button.dataset.action === 'restore'; if (!confirm(restore ? '恢复这条记录？库存会同步更新。' : '将这条记录移入回收站？库存会同步更新。')) return; await busy(button, () => api('/admin/records/' + button.dataset.recordId + (restore ? '/restore' : ''), { method: restore ? 'POST' : 'DELETE' })); toast(restore ? '记录已恢复' : '记录已移入回收站'); await Promise.all([loadRecords(), stats()]); });
$('#clear-trash').onclick = safe(async () => { if (!confirm('永久删除回收站中的所有记录？清空后无法恢复。')) return; await busy($('#clear-trash'), () => api('/admin/trash', { method: 'DELETE' })); toast('回收站已清空'); state.page = 1; await loadRecords(); });
$('#category-form').onsubmit = safe(async e => { e.preventDefault(); await busy(e.submitter, () => api('/admin/categories', { method: 'POST', body: Object.fromEntries(new FormData(e.target)) })); e.target.reset(); await renderCategories(); toast('分类已添加'); });
$('#category-list').onclick = safe(async e => { const b = e.target.closest('[data-category-id]'); if (!b || !confirm('删除这个分类？已被物品使用的分类无法删除。')) return; await api('/admin/categories/' + b.dataset.categoryId, { method: 'DELETE' }); await renderCategories(); });
$('#user-form').onsubmit = safe(async e => { e.preventDefault(); await busy(e.submitter, () => api('/admin/users', { method: 'POST', body: Object.fromEntries(new FormData(e.target)) })); e.target.reset(); await loadUsers(); toast('账号已创建'); });
$('#users-body').onclick = safe(async e => { const save = e.target.closest('[data-save-name]'); if (save) { const name = $('#users-body').querySelector(`[data-name-for="${save.dataset.saveName}"]`).value; await busy(save, () => api('/admin/users/' + save.dataset.saveName, { method: 'PATCH', body: { display_name: name } })); if (Number(save.dataset.saveName) === state.user.id) { state.user.display_name = name.trim(); $('#account-button').textContent = displayName(state.user); $('#avatar').textContent = displayName(state.user)[0]; } toast('姓名已保存'); return; } const b = e.target.closest('[data-user-id]'); if (!b || !confirm('确认变更此账号状态？该账号的现有登录会话将失效。')) return; await api('/admin/users/' + b.dataset.userId, { method: 'PATCH', body: { disabled: b.dataset.disabled === 'true' } }); await loadUsers(); });
$('#account-button').onclick = () => { $('#profile-form').elements.display_name.value = state.user.display_name || ''; $('#profile-dialog').showModal(); };
$('#close-profile').onclick = () => $('#profile-dialog').close();
$('#open-password').onclick = () => { $('#profile-dialog').close(); $('#password-dialog').showModal(); };
$('#profile-form').onsubmit = safe(async e => { e.preventDefault(); state.user = await busy(e.submitter, () => api('/profile', {method:'PATCH',body:Object.fromEntries(new FormData(e.target))})); $('#account-button').textContent = displayName(state.user); $('#avatar').textContent = displayName(state.user)[0]; $('#profile-dialog').close(); toast('姓名已保存'); await showView(state.view); }); $('#close-password').onclick = () => $('#password-dialog').close(); $('#close-photo').onclick = () => $('#photo-dialog').close();
$('#password-form').onsubmit = safe(async e => { e.preventDefault(); await busy(e.submitter, () => api('/password', { method: 'POST', body: Object.fromEntries(new FormData(e.target)) })); e.target.reset(); $('#password-dialog').close(); showLogin(); toast('密码已修改，请重新登录'); });
function stopScan() { state.scanGeneration++; if (state.scanner) { state.scanner.stop(); state.scanner = null; } const stream = $('#scan-video').srcObject; if (stream) stream.getTracks().forEach(t => t.stop()); $('#scan-video').srcObject = null; }
async function scanned(text) { $('#entry-form').elements.barcode.value = text; stopScan(); $('#scan-dialog').close(); toast('条码识别成功'); await lookupProduct(); }
$('#scan-button').onclick = safe(async () => {
  $('#scan-dialog').showModal(); $('#scan-status').textContent = '正在打开摄像头…'; const generation = ++state.scanGeneration;
  if (!window.ZXingBrowser) { $('#scan-status').textContent = '扫码组件加载失败，请刷新页面或手动输入条码。'; return; }
  state.scanReader = new ZXingBrowser.BrowserMultiFormatReader();
  if (!window.isSecureContext || !navigator.mediaDevices?.getUserMedia) { $('#scan-status').textContent = '当前浏览器无法打开摄像头，请使用 HTTPS 访问，或选择条码照片。'; return; }
  try {
    const controls = await state.scanReader.decodeFromConstraints({ video: { facingMode: { ideal: 'environment' }, width: { ideal: 1280 } }, audio: false }, $('#scan-video'), (result) => { if (result && generation === state.scanGeneration) safe(scanned)(result.getText()); });
    if (generation !== state.scanGeneration) { controls.stop(); return; } state.scanner = controls; $('#scan-status').textContent = '请对准条码，识别成功后会自动填入。';
  } catch { if (generation === state.scanGeneration) $('#scan-status').textContent = '无法访问摄像头，请检查权限，或选择条码照片。'; }
});
$('#close-scan').onclick = () => { stopScan(); $('#scan-dialog').close(); }; $('#scan-dialog').addEventListener('close', stopScan); $('#scan-dialog').addEventListener('cancel', stopScan);
$('#scan-file').onchange = safe(async e => { const file = e.target.files[0]; if (!file) return; if (!window.ZXingBrowser) throw new Error('扫码组件未加载'); stopScan(); const url = URL.createObjectURL(file); try { const reader = new ZXingBrowser.BrowserMultiFormatReader(); const result = await reader.decodeFromImageUrl(url); await scanned(result.getText()); } catch { $('#scan-status').textContent = '未识别到条码，请使用清晰、完整的条码照片。'; } finally { URL.revokeObjectURL(url); e.target.value = ''; } });
window.addEventListener('pagehide', stopScan);
(async () => { try { await signedIn(await api('/me')); } catch (e) { showLogin(); if (e.message !== '请先登录') toast(e.message, true); } })();
