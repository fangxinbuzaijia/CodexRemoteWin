'use strict';

const themeKey = 'crw.theme';
function applyTheme(theme, remember) {
  const value = theme === 'light' ? 'light' : 'dark';
  document.documentElement.dataset.theme = value;
  document.documentElement.style.colorScheme = value;
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.content = value === 'light' ? '#f5f7fa' : '#111315';
  if (remember) { try { localStorage.setItem(themeKey, value); } catch {} }
  if (typeof themeButton !== 'undefined') {
    const light = value === 'light';
    themeButton.title = light ? '切换到深色模式' : '切换到浅色模式';
    themeButton.setAttribute('aria-label', themeButton.title);
    themeButton.querySelector('.themeGlyph').textContent = light ? '◐' : '☼';
  }
}

const v11 = { generation: 0, historyController: null, polling: false, uploading: 0, projects: [], objectURLs: new Set(), historyKey: '', newBusy: false };
const style = document.createElement('style');
style.textContent = `
[hidden]{display:none!important}
.fileCard{display:inline-flex;align-items:center;gap:8px;max-width:100%;border:1px solid var(--line);background:var(--raised);color:var(--text);padding:8px;margin:6px 6px 0 0;border-radius:6px;cursor:pointer;overflow-wrap:anywhere;text-align:left;font:inherit}
.fileCard img{width:72px;height:72px;object-fit:contain}.fileCard span{min-width:0;overflow-wrap:anywhere}
.fileChip{flex-shrink:0;min-width:100px;max-width:220px}.fileChip .fileName{white-space:normal;overflow-wrap:anywhere}
.queueItem{align-items:flex-start;flex-wrap:wrap}.queueError{width:100%;font-size:12px;overflow-wrap:anywhere;color:var(--red)}.queueText{overflow-wrap:anywhere}
.remoteToast{position:fixed;bottom:100px;left:50%;transform:translateX(-50%);z-index:110;max-width:min(540px,90vw);background:var(--toast-bg);color:#fff;padding:12px 16px;border:1px solid var(--toast-line);border-radius:6px;overflow-wrap:anywhere;box-shadow:0 4px 16px var(--shadow)}
.remoteDialog{background:var(--surface,#191e22);color:var(--text,#eee);border:1px solid var(--line,#465057);border-radius:8px;width:min(480px,calc(100vw - 32px));padding:20px;max-height:85dvh;overflow:auto}.remoteDialog::backdrop{background:#0009}
.remoteDialog label{display:block;margin:12px 0 6px}.remoteDialog select,.remoteDialog textarea{width:100%;font:inherit;color:inherit;background:var(--raised,#263036);border:1px solid var(--line,#465057);border-radius:4px;padding:10px}.remoteDialog textarea{min-height:100px;resize:vertical}
.remoteDialog .dialogActions{display:flex;justify-content:flex-end;gap:12px;margin-top:16px}.remoteDialog button{padding:10px 14px;border-radius:5px;color:inherit;background:var(--raised,#263036)}
.imageDialog{max-width:95vw;width:auto}.imageDialog img{display:block;max-width:85vw;max-height:75dvh;object-fit:contain}.remoteDialog h2{font-size:18px;margin:0}
`;
document.head.append(style);
const themeButton = document.createElement('button');
themeButton.type = 'button'; themeButton.id = 'themeToggle'; themeButton.className = 'iconBtn themeButton';
const themeGlyph = document.createElement('span'); themeGlyph.className = 'themeGlyph'; themeGlyph.setAttribute('aria-hidden', 'true'); themeButton.append(themeGlyph);
$('newThread').parentNode.insertBefore(themeButton, $('newThread'));
themeButton.onclick = () => applyTheme(document.documentElement.dataset.theme === 'light' ? 'dark' : 'light', true);
applyTheme(document.documentElement.dataset.theme, false);
const newDialog = document.createElement('dialog');
newDialog.className = 'remoteDialog';
newDialog.innerHTML = '<form id="newTaskForm"><h2>新建任务</h2><label for="projectChoice">项目</label><select id="projectChoice"></select><label for="environmentChoice">工作目录</label><select id="environmentChoice"><option value="local">使用项目目录</option><option value="worktree">新建工作树</option></select><label for="firstPrompt">第一条消息</label><textarea id="firstPrompt" maxlength="8000" required></textarea><div id="newTaskError" class="queueError" role="alert"></div><div class="dialogActions"><button type="button" id="cancelNew">取消</button><button class="primary" type="submit" id="createNew">创建并发送</button></div></form>';
document.body.append(newDialog);
const toastNode = document.createElement('div'); toastNode.className = 'remoteToast'; toastNode.hidden = true; toastNode.setAttribute('role', 'status'); document.body.append(toastNode);
let toastTimer;
function toast(message) { toastNode.textContent = message; toastNode.hidden = false; clearTimeout(toastTimer); toastTimer = setTimeout(() => toastNode.hidden = true, 7000); }
function storageRead(key, fallback) { try { return JSON.parse(localStorage.getItem(key)) ?? fallback; } catch { return fallback; } }
function persistQueue() {
  try { localStorage.setItem('crw.queue.v11', JSON.stringify(st.queue)); }
  catch { toast('浏览器存储已满，消息回执仍保存在电脑上'); }
}
st.queue = storageRead('crw.queue.v11', []);
st.queue.forEach(x => { if (x.status === 'sending') x.status = 'unknown'; });
async function jsonAPI(path, options = {}) {
  const r = await api(path, options);
  const d = await r.json().catch(() => ({}));
  if (r.status === 401) { expire(); throw new Error('请重新配对设备'); }
  if (!r.ok) throw new Error(d.message || `请求失败 (${r.status})`);
  return d;
}
function saveDraft() { try { localStorage.setItem('crw.draft.' + st.selected, el.input.value); } catch {} }
function loadDraft() { el.input.value = localStorage.getItem('crw.draft.' + st.selected) || ''; autosize(); }
function busyComposer() { el.send.disabled = v11.uploading > 0 || !st.selected || !!selected()?.archived; }

const oldSyncHeader = syncHeader;
syncHeader = function () { oldSyncHeader(); busyComposer(); };
el.context.hidden = true; el.model.hidden = true;
$('runDot').parentElement.classList.add('runBadge');
// The current app-tools catalog has no interrupt operation. Do not claim that an
// unrelated GUI click stopped a task in the new desktop client.
$('stopAction').disabled = true;
$('stopAction').title = '当前桌面接口未提供停止操作，请在桌面停止';

loadThreads = async function (keepHistory) {
  const d = await jsonAPI('/api/threads?includeArchived=1');
  if (!d.ok) throw new Error(d.message || '读取任务失败');
  st.threads = d.threads || []; st.projects = d.projects || []; v11.projects = st.projects;
  if (v11.pendingThread && st.threads.some(x => x.id === v11.pendingThread)) v11.pendingThread = '';
  if (!st.threads.some(x => x.id === st.selected) && st.selected !== v11.pendingThread) {
    st.selected = st.threads.find(x => !x.archived)?.id || '';
  }
  if (st.selected) localStorage.setItem('crw.thread', st.selected);
  renderProjects(); el.healthText.textContent = '已连接桌面'; setDot(el.healthDot, 'ok');
  if (!keepHistory) {
    loadDraft();
    if (st.selected) await loadHistory(st.selected, '', false);
    else emptyChat('暂无任务，点击右上角新建');
  }
};

const originalAddMessage = addMessage;
addMessage = function (role, text, label, attachments, prepend, target) {
  const article = originalAddMessage(role, text, label, null, prepend, target);
  for (const attachment of attachments || []) {
    const button = document.createElement('button'); button.type = 'button'; button.className = 'fileCard';
    const title = document.createElement('span'); title.textContent = attachment.name + ' · ' + Math.ceil(attachment.size / 1024) + ' KB';
    button.append(title); button.title = attachment.kind === 'image' ? '查看图片' : '下载附件';
    button.onclick = () => openAttachment(attachment).catch(e => toast(e.message));
    article.querySelector('.messageWrap').append(button);
  }
  return article;
};

async function openAttachment(item) {
  const preview = item.kind === 'image';
  const response = await api('/api/attachment?id=' + encodeURIComponent(item.id) + (preview ? '&preview=1' : ''));
  if (!response.ok) throw new Error('附件读取失败，请检查连接或重新配对');
  const blob = await response.blob(); const url = URL.createObjectURL(blob);
  if (preview && /^image\/(png|jpeg|gif|webp)$/.test(blob.type)) {
    const dialog = document.createElement('dialog'); dialog.className = 'remoteDialog imageDialog';
    const image = document.createElement('img'); image.src = url; image.alt = item.name;
    const close = document.createElement('button'); close.textContent = '关闭'; close.onclick = () => dialog.close();
    dialog.append(image, close); document.body.append(dialog);
    dialog.addEventListener('close', () => { URL.revokeObjectURL(url); dialog.remove(); }, { once: true }); dialog.showModal();
  } else {
    const link = document.createElement('a'); link.href = url; link.download = item.name; document.body.append(link); link.click(); link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  }
}

loadHistory = async function (id, cursor = '', prepend = false, quiet = false) {
  const generation = ++v11.generation;
  v11.historyController?.abort(); v11.historyController = new AbortController();
  if (!prepend && !quiet) emptyChat('加载中...');
  let d;
  try { d = await jsonAPI('/api/history?thread=' + encodeURIComponent(id) + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''), { signal: v11.historyController.signal }); }
  catch (e) { if (e.name === 'AbortError') return; throw e; }
  if (generation !== v11.generation || id !== st.selected) return;
  const messages = d.messages || [];
  const key = JSON.stringify(messages);
  if (quiet && key === v11.historyKey) return;
  v11.historyKey = key;
  const oldHeight = el.messages.scrollHeight, oldTop = el.messages.scrollTop;
  const atBottom = oldHeight - oldTop - el.messages.clientHeight < 90;
  if (!prepend) el.messages.textContent = '';
  const fragment = document.createDocumentFragment();
  messages.forEach(x => addMessage(x.role, x.text, x.role === 'user' ? '你' : 'Codex', x.attachments, false, fragment));
  if (prepend) el.messages.prepend(fragment); else el.messages.append(fragment);
  if (!messages.length && !prepend) emptyChat('暂无消息');
  if (d.hasMore && d.cursor) {
    const more = document.createElement('button'); more.type = 'button'; more.className = 'loadMore'; more.textContent = '加载更早消息';
    more.onclick = async () => { more.disabled = true; try { await loadHistory(id, d.cursor, true); more.remove(); } catch (e) { more.disabled = false; toast(e.message); } };
    el.messages.prepend(more);
  }
  el.messages.scrollTop = prepend ? oldTop + el.messages.scrollHeight - oldHeight : (quiet && !atBottom ? oldTop : el.messages.scrollHeight);
};

selectThread = async function (id) {
  saveDraft(); st.selected = id; st.attachments = []; renderAttachments();
  localStorage.setItem('crw.thread', id); v11.historyKey = ''; renderProjects(); closeSide(); loadDraft();
  try { await loadHistory(id); startPoll(); } catch (e) { toast(e.message); }
};

poll = async function () {
  if (!st.selected || !st.token || document.hidden || v11.polling) return;
  v11.polling = true; const id = st.selected;
  try {
    const d = await jsonAPI('/api/status?thread=' + encodeURIComponent(id));
    if (id !== st.selected) return;
    const x = selected(); if (x) { x.active = !!d.active; x.status = d.status; }
    renderActivity(d.steps || [], d.active); syncHeader();
    setDot(el.healthDot, 'ok'); el.healthText.textContent = '已连接桌面';
    // Do not replace older pages while the reader is inspecting history.
    const nearBottom = el.messages.scrollHeight - el.messages.scrollTop - el.messages.clientHeight < 90;
    if (nearBottom) await loadHistory(id, '', false, true);
  } catch (e) { setDot(el.healthDot, 'bad'); el.healthText.textContent = '桌面未连接'; }
  finally { v11.polling = false; }
};
startPoll = function () { clearInterval(st.timer); poll(); st.timer = setInterval(poll, 4000); };
health = async function () { try { await jsonAPI('/api/health'); } catch {} };

renderQueue = function () {
  el.queue.textContent = ''; el.queue.classList.toggle('show', st.queue.length > 0);
  for (const item of st.queue) {
    const row = document.createElement('div'); row.className = 'queueItem';
    const status = document.createElement('span'); status.textContent = ({ sending: '发送中', accepted: '桌面已接收', failed: '发送失败', unknown: '结果待确认' })[item.status] || item.status;
    const text = document.createElement('span'); text.className = 'queueText'; text.textContent = item.text || '附件消息'; row.append(status, text);
    if (item.error) { const error = document.createElement('div'); error.className = 'queueError'; error.textContent = item.error; row.append(error); }
    if (item.status === 'unknown') {
      const check = document.createElement('button'); check.className = 'retry'; check.textContent = '查询回执'; check.onclick = () => checkReceipt(item); row.append(check);
    }
    if (item.status === 'failed') {
      const retry = document.createElement('button'); retry.className = 'retry'; retry.textContent = '重试';
      retry.onclick = () => { item.id = requestID(); dispatch(item); }; row.append(retry);
    }
    if (item.status === 'accepted' || item.status === 'failed') {
      const dismiss = document.createElement('button'); dismiss.className = 'retry'; dismiss.textContent = '×'; dismiss.title = '关闭记录';
      dismiss.onclick = () => { st.queue = st.queue.filter(x => x !== item); persistQueue(); renderQueue(); }; row.append(dismiss);
    }
    el.queue.append(row);
  }
};
function applyReceipt(item, receipt) {
  item.status = receipt.state || 'unknown'; item.error = receipt.error || '';
  item.resultThreadId = receipt.threadId; item.clientThreadId = receipt.clientThreadId;
  persistQueue(); renderQueue();
}
async function checkReceipt(item) {
  try { const d = await jsonAPI('/api/receipt?id=' + encodeURIComponent(item.id)); applyReceipt(item, d.receipt); }
  catch (e) { toast(e.message); }
}
dispatch = async function (item) {
  item.status = 'sending'; item.error = ''; persistQueue(); renderQueue();
  try {
    const response = await api(item.newThread ? '/api/new-thread' : '/api/deliver', {
      method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ text: item.text, threadId: item.threadId, clientRequestId: item.id, attachmentIds: item.attachmentIds || [], projectId: item.projectId || '', environment: item.environment || '' })
    });
    const d = await response.json().catch(() => ({}));
    if (d.receipt) applyReceipt(item, d.receipt);
    else {
      item.status = ['BAD_JSON','BAD_REQUEST_ID','BAD_MESSAGE','BAD_THREAD_ID','BAD_ATTACHMENT','BAD_PROJECT','BAD_ENVIRONMENT','THREAD_UNAVAILABLE','DESKTOP_UNAVAILABLE','UNAUTHORIZED','SESSION_EXPIRED'].includes(d.code) ? 'failed' : 'unknown';
      item.error = d.message || '未取得回执，请先查询发送结果';
      if (response.status === 401) expire();
    }
    if (item.status === 'accepted') {
      if (item.newThread) {
        if (item.resultThreadId) { st.selected = item.resultThreadId; v11.pendingThread = st.selected; localStorage.setItem('crw.thread', st.selected); }
        else toast('工作树准备中，任务就绪后可在列表中选择');
        await loadThreads(false).catch(e => toast('任务已创建，列表刷新失败：' + e.message));
      } else if (st.selected === item.threadId) await loadHistory(st.selected, '', false, true).catch(e => toast('桌面已接收，消息刷新失败：' + e.message));
    }
  } catch (error) { item.status = 'unknown'; item.error = error.message || '连接中断，请查询回执'; }
  finally { persistQueue(); renderQueue(); }
};

submitMessage = function () {
  if (v11.uploading) return toast('请等待附件上传完成');
  if (!st.selected) return toast('请先选择或新建任务');
  const text = el.input.value.trim(); if (!text && !st.attachments.length) return;
  const item = { id: requestID(), threadId: st.selected, text, attachmentIds: st.attachments.map(x => x.id), status: 'sending' };
  st.queue.push(item); el.input.value = ''; saveDraft(); st.attachments = []; renderAttachments(); autosize(); dispatch(item);
};

renderAttachments = function () {
  el.tray.textContent = ''; el.tray.classList.toggle('show', st.attachments.length > 0 || v11.uploading > 0);
  for (const x of st.attachments) {
    const chip = document.createElement('div'); chip.className = 'fileChip';
    const name = document.createElement('span'); name.className = 'fileName'; name.textContent = x.name + ' · 已上传';
    const remove = document.createElement('button'); remove.type = 'button'; remove.className = 'removeFile'; remove.textContent = '×'; remove.title = '移除附件';
    remove.onclick = () => { st.attachments = st.attachments.filter(a => a !== x); renderAttachments(); }; chip.append(name, remove); el.tray.append(chip);
  }
  busyComposer();
};
function uploadFile(file, progress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest(); xhr.open('POST', '/api/upload'); xhr.setRequestHeader('Authorization', 'Bearer ' + st.token); xhr.timeout = 120000;
    xhr.upload.onprogress = e => { if (e.lengthComputable) progress(Math.round(e.loaded / e.total * 100)); };
    xhr.onload = () => { let d = {}; try { d = JSON.parse(xhr.responseText); } catch {} if (xhr.status === 200 && d.ok) resolve(d.attachment); else reject(new Error(d.message || '上传失败')); };
    xhr.onerror = () => reject(new Error('上传连接中断')); xhr.ontimeout = () => reject(new Error('上传超时，请重试'));
    const form = new FormData(); form.append('file', file); xhr.send(form);
  });
}
el.file.onchange = async () => {
  const files = [...el.file.files]; el.file.value = '';
  if (v11.uploading) return toast('请等待当前上传完成');
  v11.uploading++; busyComposer();
  const selectedAtStart = st.selected;
  try {
    for (const file of files) {
      if (st.attachments.length >= 6) { toast('每条消息最多添加 6 个附件'); break; }
      if (!file.size || file.size > 12 * 1024 * 1024) { toast(file.name + '：文件需大于 0 字节且不超过 12 MB'); continue; }
      renderAttachments(); const progress = document.createElement('span'); progress.className = 'fileChip'; el.tray.append(progress);
      try {
        const item = await uploadFile(file, percent => progress.textContent = file.name + ' · ' + percent + '%');
        if (st.selected !== selectedAtStart) { toast('已切换任务，请在当前任务重新选择附件'); break; }
        st.attachments.push(item);
      } catch (e) { toast(file.name + '：' + e.message); }
    }
  } finally { v11.uploading--; renderAttachments(); }
};

newThread = async function () {
  if (v11.uploading) return toast('请等待附件上传完成');
  const choice = $('projectChoice'); choice.textContent = '';
  choice.add(new Option('不关联项目', ''));
  for (const p of v11.projects) choice.add(new Option(p.label, p.projectId));
  choice.value = selected()?.projectKey || '';
  $('firstPrompt').value = el.input.value; $('newTaskError').textContent = ''; updateEnvironment(); newDialog.showModal();
};
function updateEnvironment() {
  const project = v11.projects.find(p => p.projectId === $('projectChoice').value);
  $('environmentChoice').disabled = !project;
  $('environmentChoice').options[1].disabled = !project?.isGitRepository;
  $('environmentChoice').value = project?.isGitRepository ? 'worktree' : 'local';
}
$('projectChoice').onchange = updateEnvironment;
$('cancelNew').onclick = () => { if (!v11.newBusy) newDialog.close(); };
$('newTaskForm').onsubmit = async event => {
  event.preventDefault(); if (v11.newBusy) return;
  v11.newBusy = true; $('createNew').disabled = true;
  const item = { id: requestID(), newThread: true, threadId: '', text: $('firstPrompt').value.trim(), projectId: $('projectChoice').value, environment: $('environmentChoice').value, attachmentIds: st.attachments.map(x => x.id), status: 'sending' };
  st.queue.push(item); newDialog.close(); st.attachments = []; renderAttachments();
  try { await dispatch(item); } finally { v11.newBusy = false; $('createNew').disabled = false; }
};

threadActionFor = async function (x, action, name) {
  if (!x) return;
  try {
    const d = await jsonAPI('/api/thread-action', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ threadId: x.id, action, name }) });
    if (!d.ok) throw new Error(d.message || '操作失败');
    if (d.confirmed === false) toast(d.message);
    if (action === 'archive') st.selected = '';
    await loadThreads(false);
  } catch (e) { toast(e.message); }
};
el.input.oninput = () => { autosize(); saveDraft(); };
window.addEventListener('unhandledrejection', event => { if (event.reason?.name !== 'AbortError') toast(event.reason?.message || '操作失败'); event.preventDefault(); });
document.addEventListener('visibilitychange', () => { if (!document.hidden && st.token) { loadThreads(true).catch(e => toast(e.message)); poll(); } });
boot = async function () {
  if (!st.token) return showPair(true);
  showPair(false); renderQueue();
  try { await loadThreads(false); startPoll(); }
  catch (e) { emptyChat(e.message); el.healthText.textContent = '桌面未连接'; setDot(el.healthDot, 'bad'); }
};
boot();
