'use strict';

/* GitHub++ 控制台前端逻辑
 * 零依赖实现：不引入任何框架或外部 CDN，所有交互由原生 DOM 完成。
 * 这样在 NAS 内网环境下也能正常工作。 */

const $ = (id) => document.getElementById(id);
const $$ = (sel) => Array.prototype.slice.call(document.querySelectorAll(sel));

/* ---------- 状态 ---------- */
const state = {
  token: localStorage.getItem('ghpp_token') || '',
  status: null,
  config: null,
  mirrors: [],
  logSource: null,
  logAutoScroll: true,
  timers: [],
  // 接入方式区域当前选中的主机地址（用于生成 HTTP 代理 / Git / hosts / 前缀下载示例）。
  // Docker 项固定用 127.0.0.1 不跟随此值。空串表示尚未初始化，renderStatus 会填默认值。
  accessHost: '',
  // 本机所有局域网 IPv4，由 /api/status 返回，用于填充下拉选项。
  localIPs: [],
  // 用户保存的外网接入地址，由 /api/status 返回。
  externalHost: '',
};

/* ---------- 请求封装 ---------- */
async function api(path, options) {
  const opt = Object.assign({method: 'GET'}, options || {});
  opt.headers = Object.assign({
    'Content-Type': 'application/json',
  }, opt.headers || {});

  if (state.token) {
    opt.headers['Authorization'] = 'Bearer ' + state.token;
  }
  if (opt.body && typeof opt.body !== 'string') {
    opt.body = JSON.stringify(opt.body);
  }

  let resp;
  try {
    resp = await fetch(path, opt);
  } catch (e) {
    throw new Error('无法连接服务，请确认程序正在运行');
  }

  if (resp.status === 401) {
    doLogout(true);
    throw new Error('登录已过期，请重新登录');
  }

  let data = null;
  const text = await resp.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (e) {
      throw new Error('服务返回了无法解析的内容');
    }
  }

  if (!resp.ok) {
    throw new Error((data && data.error) || ('请求失败：HTTP ' + resp.status));
  }
  return data && data.data !== undefined ? data.data : data;
}

/* ---------- 提示条 ---------- */
function toast(message, kind) {
  const el = document.createElement('div');
  el.className = 'toast' + (kind ? ' ' + kind : '');
  el.textContent = message;
  $('toast-wrap').appendChild(el);
  setTimeout(() => {
    el.style.opacity = '0';
    el.style.transition = 'opacity .2s';
    setTimeout(() => el.remove(), 220);
  }, 3000);
}

/* ---------- 格式化 ---------- */
function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

function fmtSpeed(kbps) {
  const v = Number(kbps) || 0;
  if (v <= 0) return '—';
  if (v < 1024) return Math.round(v) + ' KB/s';
  return (v / 1024).toFixed(1) + ' MB/s';
}

function fmtLatency(ms) {
  const v = Number(ms) || 0;
  if (v <= 0) return '—';
  return Math.round(v) + ' ms';
}

function fmtDuration(seconds) {
  const s = Math.round(Number(seconds) || 0);
  if (s < 60) return s + ' 秒';
  if (s < 3600) return Math.floor(s / 60) + ' 分 ' + (s % 60) + ' 秒';
  return Math.floor(s / 3600) + ' 时 ' + Math.floor((s % 3600) / 60) + ' 分';
}

function fmtTime(iso) {
  if (!iso) return '暂无';
  const d = new Date(iso);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '暂无';
  const pad = (n) => String(n).padStart(2, '0');
  return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
    pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
}

function parseDuration(str) {
  if (!str) return 0;
  const m = String(str).match(/(?:(\d+)h)?(?:(\d+)m)?([\d.]+)s/);
  if (!m) return 0;
  return (Number(m[1]) || 0) * 3600 + (Number(m[2]) || 0) * 60 + (Number(m[3]) || 0);
}

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

/* ---------- 登录 ---------- */
function showLogin() {
  $('login-view').hidden = false;
  $('app-view').hidden = true;
  const pass = $('login-pass');
  if (pass) pass.focus();
}

function showApp() {
  $('login-view').hidden = true;
  $('app-view').hidden = false;
}

async function doLogin(ev) {
  ev.preventDefault();
  const btn = $('login-submit');
  const errBox = $('login-error');
  btn.disabled = true;
  btn.textContent = '登录中…';
  errBox.hidden = true;

  try {
    const data = await api('/api/login', {
      method: 'POST',
      body: {
        username: $('login-user').value.trim(),
        password: $('login-pass').value,
      },
    });
    state.token = data.token;
    localStorage.setItem('ghpp_token', data.token);
    showApp();
    await boot();
  } catch (e) {
    errBox.textContent = e.message;
    errBox.hidden = false;
  } finally {
    btn.disabled = false;
    btn.textContent = '登录';
  }
}

function doLogout(silent) {
  if (state.token) {
    api('/api/logout', {method: 'POST'}).catch(() => {});
  }
  state.token = '';
  localStorage.removeItem('ghpp_token');
  stopAllTimers();
  if (state.logSource) {
    state.logSource.close();
    state.logSource = null;
  }
  showLogin();
  if (!silent) toast('已退出登录');
}

/* ---------- 总览 ---------- */
function renderStatus() {
  const st = state.status;
  if (!st) return;
  const s = st.status;
  const m = s.metrics;

  const dot = $('run-dot');
  dot.className = 'dot ' + (s.running ? 'on' : 'off');
  $('run-label').textContent = s.running ? '运行中' : '已停止';

  // 总览页启停开关按钮：运行中显示「停止服务」（红底白字），停止时显示「启动服务」（蓝底白字）。
  const toggleBtn = $('btn-service-toggle');
  if (toggleBtn) {
    toggleBtn.textContent = s.running ? '停止服务' : '启动服务';
    toggleBtn.classList.toggle('btn-primary', !s.running);
    toggleBtn.classList.toggle('btn-danger-solid', s.running);
  }

  const hitRate = m.total_requests > 0
    ? Math.round(m.accelerated / m.total_requests * 100)
    : 0;

  const metrics = [
    {label: '累计请求', value: m.total_requests.toLocaleString(), sub: '其中加速 ' + m.accelerated.toLocaleString() + ' 次'},
    {label: '加速命中率', value: hitRate + '%', sub: m.failed > 0 ? '失败 ' + m.failed + ' 次' : '无失败请求'},
    {label: '流量转发', value: fmtBytes(m.bytes_out), sub: '累计节省约 ' + fmtDuration(m.saved_ms / 1000)},
    {label: '可用加速源', value: s.mirrors.healthy + ' / ' + s.mirrors.enabled, sub: '共 ' + s.mirrors.total + ' 个已配置'},
    {label: '运行时长', value: s.uptime || '—', sub: '代理 ' + (s.proxy_addr || '未监听')},
    {label: '换源次数', value: String(m.failovers), sub: '自动故障转移'},
  ];

  $('metrics').innerHTML = metrics.map((x) => (
    '<div class="metric">' +
    '<div class="label">' + esc(x.label) + '</div>' +
    '<div class="value">' + esc(x.value) + '</div>' +
    '<div class="sub">' + esc(x.sub) + '</div>' +
    '</div>'
  )).join('');

  $$('#mode-picker .mode-opt').forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.mode === s.config_mode);
  });

  const modeNames = {auto: '自动', proxy: '镜像中转', hosts: 'DNS 优选', direct: '直连'};
  $('mode-reason').textContent = '当前生效：' + (modeNames[s.effective_mode] || s.effective_mode);

  const v = st.verdict;
  if (v && v.reason) {
    $('verdict').textContent = v.reason;
  } else {
    $('verdict').textContent = '尚未完成首次评估，点击下方按钮立即测速。';
  }

  renderCategoryChart(m);

  // 同步接入方式切换栏所需的网络信息：本机 LAN IP 列表 + 已保存外网地址。
  // 首次加载时 accessHost 为空，用当前访问 hostname 作默认值（保持旧行为）。
  state.localIPs = Array.isArray(s.local_ips) ? s.local_ips : [];
  state.externalHost = s.external_host || '';
  if (!state.accessHost) state.accessHost = location.hostname || '127.0.0.1';
  renderAccessHostSelect();

  renderConnectList(s);
}

const CHART_COLORS = ['#4f46e5', '#16a34a', '#d97706', '#7c3aed'];
const CHART_LABELS = ['网页与 API', '文件下载', '仓库克隆', 'Docker 拉取'];

function renderCategoryChart(m) {
  const data = [m.web_count, m.raw_count, m.clone_count, m.docker_count || 0];
  const total = data.reduce((a, b) => a + b, 0);

  $('legend-cat').innerHTML = CHART_LABELS.map((l, i) => (
    '<span><i style="background:' + CHART_COLORS[i] + '"></i>' + l + ' ' +
    (total > 0 ? Math.round(data[i] / total * 100) : 0) + '%</span>'
  )).join('');

  // 缓存数据，供窗口尺寸变化时重绘。
  state.chartData = data;
  drawCategoryChart();
}

function drawCategoryChart() {
  const canvas = $('chart-cat');
  if (!canvas || !state.chartData) return;
  const data = state.chartData;
  const total = data.reduce((a, b) => a + b, 0);

  // 从当前主题读取文字/占位色，保证深色主题下可读。
  const css = getComputedStyle(document.documentElement);
  const textColor = css.getPropertyValue('--text').trim() || '#1a1d2e';
  const faintColor = css.getPropertyValue('--text-faint').trim() || '#98a0b3';
  const trackColor = css.getPropertyValue('--border-soft').trim() || '#eef1f6';

  // 用内联 Canvas 绘制环形图，避免引入任何外部依赖。
  // 关键：CSS 尺寸必须与绘图坐标系同为正方形，否则环形会被拉伸成椭圆。
  const dpr = window.devicePixelRatio || 1;
  const box = canvas.parentElement.getBoundingClientRect();
  const size = Math.max(140, Math.floor(Math.min(box.width, box.height)));
  canvas.width = size * dpr;
  canvas.height = size * dpr;
  canvas.style.width = size + 'px';
  canvas.style.height = size + 'px';

  const ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, size, size);

  const cx = size / 2;
  const cy = size / 2;
  const outer = size * 0.42;
  const inner = outer * 0.62;

  // 无数据时画一个灰色的占位环。
  if (total === 0) {
    ctx.beginPath();
    ctx.arc(cx, cy, (outer + inner) / 2, 0, Math.PI * 2);
    ctx.lineWidth = outer - inner;
    ctx.strokeStyle = trackColor;
    ctx.stroke();

    ctx.fillStyle = faintColor;
    ctx.font = '500 ' + Math.round(size * 0.085) + 'px -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText('暂无请求', cx, cy);
    return;
  }

  // 先画一圈底环，避免 100% 单分类时起点/终点接缝发虚。
  ctx.beginPath();
  ctx.arc(cx, cy, (outer + inner) / 2, 0, Math.PI * 2);
  ctx.lineWidth = outer - inner;
  ctx.strokeStyle = trackColor;
  ctx.stroke();

  let start = -Math.PI / 2;
  data.forEach((v, i) => {
    if (v <= 0) return;
    const angle = v / total * Math.PI * 2;
    ctx.beginPath();
    ctx.arc(cx, cy, (outer + inner) / 2, start, start + angle);
    ctx.lineWidth = outer - inner;
    ctx.strokeStyle = CHART_COLORS[i];
    ctx.stroke();
    start += angle;
  });

  // 圆心显示总请求数（数字 + 标签垂直居中排布）。
  ctx.textAlign = 'center';
  ctx.textBaseline = 'middle';
  ctx.fillStyle = textColor;
  ctx.font = '700 ' + Math.round(size * 0.15) + 'px -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif';
  ctx.fillText(total.toLocaleString(), cx, cy - size * 0.045);

  ctx.fillStyle = faintColor;
  ctx.font = '400 ' + Math.round(size * 0.07) + 'px -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif';
  ctx.fillText('总请求', cx, cy + size * 0.075);
}

// 侧边栏折叠/窗口缩放会改变图形容器宽度，防抖重绘。
let chartResizeTimer = null;
function scheduleChartRedraw() {
  clearTimeout(chartResizeTimer);
  chartResizeTimer = setTimeout(() => {
    if ($('panel-overview').classList.contains('active')) drawCategoryChart();
  }, 200);
}

function renderConnectList(s) {
  const proxyPort = (s.proxy_addr || '').split(':').pop() || '7710';
  // 主机地址取切换栏当前选中值；未初始化时回退到当前访问控制台用的 hostname。
  const host = state.accessHost || location.hostname || '127.0.0.1';

  // Docker daemon 跑在 NAS 本机，固定用 127.0.0.1 + 代理端口，不跟随切换的 host：
  // 即便用户切到外网地址配置其他客户端，Docker mirror 仍走本机 loopback 最稳。
  const dockerHost = '127.0.0.1';

  const items = [
    {
      tag: 'HTTP',
      title: 'HTTP 代理（推荐，全局生效）',
      code: host + ':' + proxyPort,
      desc: '在系统或浏览器中把 HTTP 代理设为该地址',
    },
    {
      tag: 'Git',
      title: 'Git 全局配置',
      code: 'git config --global http.proxy http://' + host + ':' + proxyPort,
      desc: '配置后 git clone / pull 自动走加速通道',
    },
    {
      tag: 'HOST',
      title: 'hosts 指向本机',
      code: host + '  github.com',
      desc: '把 GitHub 域名解析到 NAS，配合反代实现透明加速',
    },
    {
      tag: 'DOCKER',
      title: 'Docker 镜像加速（registry-mirrors，本机）',
      code: 'http://' + dockerHost + ':' + proxyPort,
      desc: 'Docker daemon 在 NAS 本机，固定用 127.0.0.1 + 代理端口',
    },
    {
      tag: 'URL',
      title: '前缀下载（wget / curl 直用）',
      code: 'http://' + host + ':' + proxyPort + '/https://github.com/用户/仓库/releases/download/...',
      desc: '在 GitHub 地址前加上本机前缀即可下载 Release / Raw 文件',
    },
  ];

  $('connect-list').innerHTML = items.map((it) => (
    '<div class="connect-item">' +
    '<div class="ci-icon">' + esc(it.tag) + '</div>' +
    '<div class="ci-body">' +
    '<strong>' + esc(it.title) + '</strong>' +
    '<code title="' + esc(it.code) + '">' + esc(it.code) + '</code>' +
    '</div>' +
    '<button class="btn btn-sm" data-copy="' + esc(it.code) + '">复制</button>' +
    '</div>'
  )).join('');
}

// renderAccessHostSelect 填充「接入方式」区域的主机地址下拉。
//
// 选项顺序：
//   1. 跟随当前访问（location.hostname）——默认选中，用户从内网访问就是内网地址，外网访问就是外网地址。
//   2. 各本机 LAN IP——飞牛多网卡时可切换不同网卡地址。
//   3. 已保存的外网地址——若 state.externalHost 非空。
//   4. 自定义……——选中后焦点跳到输入框，用户可填任意地址。
//
// 同时把输入框值同步为当前 accessHost（若不在下拉选项里则保留原值）。
function renderAccessHostSelect() {
  const sel = $('access-host-select');
  if (!sel) return;

  const cur = location.hostname || '127.0.0.1';
  const opts = [];
  opts.push({value: cur, label: '跟随当前访问（' + cur + '）'});
  (state.localIPs || []).forEach((ip) => {
    if (ip !== cur) opts.push({value: ip, label: '局域网 ' + ip});
  });
  if (state.externalHost) {
    opts.push({value: state.externalHost, label: '外网 ' + state.externalHost});
  }
  opts.push({value: '__custom__', label: '自定义……'});

  // 当前选中值若在下拉里就保留，否则回退到"跟随当前访问"。
  let selected = state.accessHost || cur;
  if (!opts.some((o) => o.value === selected)) selected = cur;

  sel.innerHTML = opts.map((o) =>
    '<option value="' + esc(o.value) + '"' + (o.value === selected ? ' selected' : '') + '>' +
    esc(o.label) + '</option>'
  ).join('');

  // 输入框：若当前是自定义地址则显示该值，否则清空（避免误导）。
  const inp = $('access-host-input');
  if (inp) {
    const isCustom = selected === '__custom__' ||
      !opts.some((o) => o.value === selected && o.value !== '__custom__');
    inp.value = (isCustom && state.accessHost) ? state.accessHost : '';
  }
}

// applyAccessHost 把下拉/输入框的当前值应用到接入方式列表并立即重渲染。
//
// 优先级：输入框非空 → 用输入框值；否则用下拉值；下拉选"自定义"且输入框空 → 提示输入。
function applyAccessHost() {
  const sel = $('access-host-select');
  const inp = $('access-host-input');
  if (!sel) return;

  const customVal = (inp && inp.value || '').trim();
  const selVal = sel.value;

  if (selVal === '__custom__') {
    if (!customVal) {
      if (inp) inp.focus();
      toast('请在右侧输入框填写自定义地址');
      return;
    }
    state.accessHost = customVal;
  } else {
    state.accessHost = selVal;
    if (inp) inp.value = '';
  }

  if (state.status) renderConnectList(state.status.status);
}

// saveExternalHost 把当前输入框/下拉选中的地址保存为外网地址，持久化到后端 config.yaml。
async function saveExternalHost() {
  const sel = $('access-host-select');
  const inp = $('access-host-input');
  let host = (inp && inp.value || '').trim();
  if (!host && sel && sel.value && sel.value !== '__custom__') host = sel.value.trim();
  if (!host) {
    toast('请先填写或选择一个地址再保存');
    return;
  }

  try {
    const res = await api('/api/network', {method: 'PUT', body: {host: host}});
    state.externalHost = (res && res.external_host) || host;
    state.accessHost = host;
    renderAccessHostSelect();
    if (state.status) renderConnectList(state.status.status);
    toast('已保存为外网地址');
  } catch (e) {
    toast('保存失败：' + (e && e.message || e));
  }
}

/* ---------- 加速源 ---------- */
const KIND_NAMES = {
  prefix: '前缀中转',
  raw: 'Raw CDN',
  git: '仓库镜像',
  direct: '官方直连',
};

function renderMirrors() {
  const body = $('mirror-body');
  const stats = state.mirrorStats || {};

  if (!state.mirrors.length) {
    body.innerHTML = '<tr><td colspan="7" class="muted" style="padding:20px;text-align:center">暂无加速源</td></tr>';
    return;
  }

  body.innerHTML = state.mirrors.map((m) => {
    const st = m.stat || {};
    const ok = !!st.ok;
    const tested = st.at && new Date(st.at).getFullYear() > 2000;
    const score = Math.max(0, Math.min(100, Number(st.score) || 0));

    let statusBadge;
    if (!m.enabled) {
      statusBadge = '<span class="badge">已停用</span>';
    } else if (!tested) {
      statusBadge = '<span class="badge">未测速</span>';
    } else if (ok) {
      statusBadge = '<span class="badge badge-ok">正常</span>';
    } else {
      statusBadge = '<span class="badge badge-err" title="' + esc(st.error || '') + '">不可用</span>';
    }

    return '<tr>' +
      '<td><label class="switch"><input type="checkbox" data-toggle="' + esc(m.id) + '"' +
        (m.enabled ? ' checked' : '') + '></label></td>' +
      '<td><div class="src-cell"><div class="meta">' +
        '<strong>' + esc(m.name) + ' ' + statusBadge + '</strong>' +
        '<small title="' + esc(m.url) + '">' + esc(m.url) + '</small>' +
      '</div></div></td>' +
      '<td><span class="badge">' + esc(KIND_NAMES[m.kind] || m.kind) + '</span></td>' +
      '<td class="mono">' + fmtLatency(st.latency_ms) + '</td>' +
      '<td class="mono">' + fmtSpeed(st.throughput_kbps) + '</td>' +
      '<td class="score-cell"><span class="score-bar"><i style="width:' + score + '%"></i></span>' +
        '<span class="mono">' + Math.round(score) + '</span></td>' +
      '<td><div class="row-actions">' +
        '<button class="btn btn-sm" data-test="' + esc(m.id) + '">测速</button>' +
        '<button class="btn btn-sm" data-edit="' + esc(m.id) + '">编辑</button>' +
        '<button class="btn btn-sm btn-danger" data-del="' + esc(m.id) + '">删除</button>' +
      '</div></td>' +
    '</tr>';
  }).join('');

  $('mirror-summary').textContent =
    state.mirrors.filter((m) => m.enabled).length + ' / ' + state.mirrors.length + ' 启用';

  bindMirrorEvents();
}

function bindMirrorEvents() {
  $$('[data-toggle]').forEach((el) => {
    el.onchange = async () => {
      const id = el.dataset.toggle;
      const m = state.mirrors.find((x) => x.id === id);
      if (!m) return;
      try {
        await api('/api/mirrors', {
          method: 'PUT',
          body: {id: id, enabled: el.checked},
        });
        toast((el.checked ? '已启用 ' : '已停用 ') + m.name, 'ok');
        await loadMirrors();
      } catch (e) {
        el.checked = !el.checked;
        toast(e.message, 'err');
      }
    };
  });

  $$('[data-test]').forEach((el) => {
    el.onclick = async () => {
      const id = el.dataset.test;
      el.disabled = true;
      el.textContent = '测试中';
      try {
        const results = await api('/api/mirrors/test?id=' + encodeURIComponent(id), {method: 'POST'});
        const r = (results || [])[0];
        if (r && r.ok) {
          toast('延迟 ' + fmtLatency(r.latency_ms) + '，吞吐 ' + fmtSpeed(r.throughput_kbps), 'ok');
        } else {
          toast('测速失败：' + ((r && r.error) || '无响应'), 'err');
        }
        await loadMirrors();
      } catch (e) {
        toast(e.message, 'err');
      } finally {
        el.disabled = false;
        el.textContent = '测速';
      }
    };
  });

  $$('[data-edit]').forEach((el) => {
    el.onclick = () => openMirrorModal(el.dataset.edit);
  });

  $$('[data-del]').forEach((el) => {
    el.onclick = async () => {
      const id = el.dataset.del;
      const m = state.mirrors.find((x) => x.id === id);
      if (!confirm('确定删除加速源「' + (m ? m.name : id) + '」？')) return;
      try {
        await api('/api/mirrors?id=' + encodeURIComponent(id), {method: 'DELETE'});
        toast('已删除', 'ok');
        await loadMirrors();
      } catch (e) {
        toast(e.message, 'err');
      }
    };
  });
}

function openMirrorModal(id) {
  const title = $('mirror-modal-title');
  if (id) {
    const m = state.mirrors.find((x) => x.id === id);
    if (!m) return;
    title.textContent = '编辑加速源';
    $('mf-id').value = m.id;
    $('mf-name').value = m.name;
    $('mf-url').value = m.url;
    $('mf-kind').value = m.kind;
    $('mf-weight').value = m.weight || 1;
    $('mf-note').value = m.note || '';
  } else {
    title.textContent = '添加上游源';
    $('mirror-form').reset();
    $('mf-id').value = '';
    $('mf-weight').value = '1';
  }
  $('mirror-modal').hidden = false;
  setTimeout(() => $('mf-name').focus(), 50);
}

async function saveMirror(ev) {
  ev.preventDefault();
  const id = $('mf-id').value;
  const payload = {
    id: id,
    name: $('mf-name').value.trim(),
    url: $('mf-url').value.trim(),
    kind: $('mf-kind').value,
    weight: Number($('mf-weight').value) || 1,
    note: $('mf-note').value.trim(),
    enabled: true,
  };

  try {
    await api('/api/mirrors', {method: id ? 'PUT' : 'POST', body: payload});
    $('mirror-modal').hidden = true;
    toast(id ? '已保存' : '已添加', 'ok');
    await loadMirrors();
  } catch (e) {
    toast(e.message, 'err');
  }
}

/* ---------- DNS 优选 ---------- */
function renderHosts(data) {
  const cfg = state.config || {};
  const hostsCfg = cfg.hosts || {};
  $('hosts-toggle').checked = !!hostsCfg.enabled;
  $('hosts-path').textContent = data.file || '';

  const notice = $('hosts-notice');
  if (!data.writable) {
    notice.className = 'notice err';
    notice.textContent = '没有权限写入 ' + data.file + '，请以 root 身份运行或手动授权。';
  } else if (!hostsCfg.enabled) {
    notice.className = 'notice';
    notice.textContent = 'hosts 加速当前已关闭。开启后程序会自动把 GitHub 域名解析到实测最快的 IP。';
  } else {
    notice.className = 'notice';
    notice.textContent = 'hosts 加速已启用，每 ' + (hostsCfg.refresh_minutes || 60) +
      ' 分钟自动重新优选一次。最后更新：' + fmtTime(state.status && state.status.status.last_hosts_sync);
  }

  const best = data.best || {};
  const hosts = (state.dnsHosts || []);
  const rows = hosts.length ? hosts : Object.keys(best).map((h) => ({host: h}));

  $('dns-body').innerHTML = rows.map((h) => {
    const b = best[h.host] || {};
    const ok = !!b.ok;
    return '<tr>' +
      '<td><span class="mono">' + esc(h.host) + '</span></td>' +
      '<td class="mono">' + (ok ? esc(b.ip) : '<span class="muted">待优选</span>') + '</td>' +
      '<td class="mono">' + (ok ? fmtLatency(b.latency_ms) : '—') + '</td>' +
      '<td class="mono">' + (ok ? fmtLatency(b.tls_ms) : '—') + '</td>' +
      '<td class="mono">' + (ok ? fmtLatency(b.total_ms) : '—') + '</td>' +
    '</tr>';
  }).join('') || '<tr><td colspan="5" class="muted" style="padding:20px;text-align:center">尚未执行过优选</td></tr>';

  const entries = data.entries || [];
  $('hosts-body').innerHTML = entries.map((e) => (
    '<tr><td class="mono">' + esc(e.IP || e.ip) + '</td>' +
    '<td class="mono">' + esc(e.Host || e.host) + '</td></tr>'
  )).join('') || '<tr><td colspan="2" class="muted" style="padding:20px;text-align:center">hosts 中暂无托管记录</td></tr>';
}

/* ---------- 日志 ---------- */
function appendLog(entry) {
  const view = $('log-view');
  if (!view) return;

  const level = (entry.level || 'info').toLowerCase();
  const filtered = $('log-level').value;
  if (filtered && level !== filtered) return;

  const line = document.createElement('div');
  line.className = 'log-line ' + level;
  line.dataset.level = level;
  line.dataset.seq = entry.seq;

  const d = new Date(entry.time);
  const pad = (n) => String(n).padStart(2, '0');
  const ts = pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());

  line.innerHTML = '<span class="t">' + ts + '</span>' +
    '<span class="l">' + esc(level) + '</span>' +
    '<span class="m">' + esc(entry.message) + '</span>';

  view.appendChild(line);

  // 限制 DOM 节点数量，避免长时间运行后页面卡顿。
  while (view.childElementCount > 800) {
    view.removeChild(view.firstChild);
  }

  if (state.logAutoScroll) {
    view.scrollTop = view.scrollHeight;
  }
}

function startLogStream() {
  if (state.logSource || !state.token) return;

  const src = new EventSource('/api/logs/stream?token=' + encodeURIComponent(state.token));
  state.logSource = src;

  src.onmessage = (ev) => {
    try {
      appendLog(JSON.parse(ev.data));
    } catch (e) { /* 忽略格式异常的心跳 */ }
  };

  src.onerror = () => {
    // EventSource 会自动重连，这里不做处理，避免刷屏。
  };
}

/* ---------- 设置 ---------- */
function renderSettings() {
  const cfg = state.config;
  if (!cfg) return;

  $('set-proxy-listen').value = cfg.proxy.listen || '';
  $('set-connect-timeout').value = cfg.proxy.connect_timeout_ms || 8000;
  $('set-read-timeout').value = cfg.proxy.read_timeout_ms || 30000;
  $('set-failover').value = cfg.proxy.failover_threshold || 3;
  $('set-cooldown').value = cfg.proxy.cooldown_seconds || 300;
  $('set-probe-interval').value = cfg.auto.probe_interval_minutes || 30;
  $('set-direct-ratio').value = cfg.auto.direct_better_ratio || 1.5;

  // 同步看门狗自动重启开关（设置页）。
  const wdToggle = $('watchdog-toggle');
  if (wdToggle) wdToggle.checked = !!(cfg.watchdog && cfg.watchdog.auto_restart);
}

/* ---------- Docker 加速 ---------- */
async function loadDocker() {
  const data = await api('/api/docker');
  state.docker = data;
  renderDocker();
}

function renderDocker() {
  const d = state.docker;
  if (!d) return;

  $('docker-toggle').checked = !!d.enabled;
  $('docker-mirror-url').value = d.registry_mirror || '';
  $('docker-daemon-json').value = d.daemon_json || '';
  $('docker-upstreams').value = (d.upstreams || []).join('\n');

  // applied 徽章：反映 daemon.json 是否已含本代理地址。
  const badge = $('docker-applied-badge');
  if (badge) {
    if (d.applied) {
      badge.textContent = '已应用到系统';
      badge.className = 'docker-applied-badge on';
    } else {
      badge.textContent = '未应用';
      badge.className = 'docker-applied-badge off';
    }
  }

  $('docker-registries').innerHTML = (d.supported_registries || []).map((r) => (
    '<tr><td><span class="mono">' + esc(r.registry) + '</span></td>' +
    '<td>' + esc(r.note) + '</td></tr>'
  )).join('');

  const proxyPort = (d.proxy_url || '').split(':').pop() || '7710';
  const host = d.proxy_url ? d.proxy_url.replace(/^https?:\/\//, '').replace(/:\d+$/, '') : 'NAS-IP';
  const base = 'http://' + host + ':' + proxyPort;

  $('snippet-git').value =
    'git config --global url."' + base + '/https://github.com/".insteadOf "https://github.com/"\n' +
    '# 取消加速：git config --global --unset url."' + base + '/https://github.com/".insteadOf';
  $('snippet-curl').value =
    'wget ' + base + '/https://github.com/用户/仓库/releases/download/v1.0/文件.zip';
  $('snippet-proxy').value =
    'export http_proxy=' + base + '   # 仅加速 GitHub，其他域名直连';
}

// applyDocker 一键写入 daemon.json 并重启 Docker，让小白用户无需 SSH。
// 强确认：会重启 Docker 服务，所有容器短暂中断。
async function applyDocker() {
  if (!confirm('确定一键应用到系统？\n\n' +
      '本操作会：\n' +
      '1. 把本机代理地址写入 /etc/docker/daemon.json（保留其他配置）\n' +
      '2. 重启 Docker 服务让配置生效\n\n' +
      '⚠️ 所有运行中的容器会短暂中断几秒，请确保没有正在执行的关键任务。')) return;

  const btn = $('btn-docker-apply') || $('btn-apps-docker-apply');
  const origText = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '应用中…'; }
  try {
    const res = await api('/api/docker/apply', {method: 'POST'});
    toast((res && res.message) || '已应用并重启 Docker', 'ok');
    // 重新加载 docker 状态（applied 徽章会刷新）。
    await loadDocker();
  } catch (e) {
    toast(e.message, 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = origText; }
  }
}

async function saveDocker() {
  const upstreams = $('docker-upstreams').value
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean);
  if (!upstreams.length) {
    toast('上游列表不能为空', 'err');
    return;
  }
  try {
    const data = await api('/api/docker', {
      method: 'PUT',
      body: {enabled: $('docker-toggle').checked, upstreams: upstreams},
    });
    state.docker = data;
    renderDocker();
    toast('Docker 加速配置已保存', 'ok');
  } catch (e) {
    toast(e.message, 'err');
  }
}

/* ---------- 证书管理 ---------- */
async function loadCert() {
  const data = await api('/api/cert');
  if (!data.available) {
    $('cert-brief').textContent = '证书不可用：' + (data.error || '未知原因');
    return;
  }
  const until = new Date(data.not_after);
  $('cert-brief').textContent = data.fingerprint
    ? '指纹 ' + data.fingerprint.slice(0, 47) + '…，有效期至 ' + until.getFullYear() + '-' + String(until.getMonth() + 1).padStart(2, '0') + '-' + String(until.getDate()).padStart(2, '0')
    : '已就绪';
}

/* ---------- 关于 ---------- */
async function loadAbout() {
  const data = await api('/api/about');
  state.about = data;

  const info = [
    ['软件名称', data.name],
    ['版本', data.version],
    ['作者', data.author],
    ['GitHub', data.github],
    ['开发语言', data.language + '（' + data.go_version + '）'],
    ['开源协议', data.license],
  ];
  $('about-info').innerHTML = info.map((row) => (
    '<div><span>' + esc(row[0]) + '</span><span class="mono">' + esc(row[1]) + '</span></div>'
  )).join('') +
  '<p class="muted" style="margin-bottom:0">' + esc(data.description) + '</p>' +
  '<p class="muted" style="margin-bottom:0"><small>' + esc(data.open_source) + '</small></p>';

  $('about-github-link').href = data.github;
  $('about-repo-link').href = data.repo;

  $('about-safety').innerHTML = (data.safety || []).map((s) => (
    '<div class="safety-item">' +
    '<strong>' + esc(s.point) + '</strong>' +
    '<p>' + esc(s.detail) + '</p>' +
    '</div>'
  )).join('');

  $('about-refs').innerHTML = (data.references || []).map((r) => (
    '<tr><td><a href="' + esc(r.url) + '" target="_blank" rel="noopener">' + esc(r.name) + '</a></td>' +
    '<td>' + esc(r.note) + '</td></tr>'
  )).join('');
}

/* ---------- 数据加载 ---------- */
async function loadStatus() {
  const data = await api('/api/status');
  state.status = data;
  state.version = data.version;
  renderStatus();
}

async function loadMirrors() {
  state.mirrors = await api('/api/mirrors');
  renderMirrors();
}

async function loadConfig() {
  state.config = await api('/api/config');
  renderSettings();
}

async function loadHosts() {
  const data = await api('/api/hosts');
  renderHosts(data);
}

async function loadDNS() {
  const data = await api('/api/dns');
  state.dnsHosts = data.hosts || [];
  renderHosts(await api('/api/hosts'));
}

// loadApps 加载「应用加速」页：hosts 加速状态、Docker 加速状态、系统代理状态。
// 复用 /api/status（已含 hosts/docker/system_proxy 字段）+ /api/sysproxy 兜底。
async function loadApps() {
  // 拿最新 status：若刚加载过就复用，否则现拉。
  if (!state.status) {
    state.status = await api('/api/status');
  }
  renderApps();
}

// renderApps 填充「应用加速」页的状态徽章与系统代理开关。
function renderApps() {
  const s = state.status && state.status.status;
  if (!s) return;

  // hosts 加速状态：来自 status.hosts_enabled（hosts 加速开关）。
  const hostsOn = !!(s.hosts_enabled);
  const hostsEl = $('apps-hosts-status');
  if (hostsEl) {
    hostsEl.textContent = hostsOn ? '已开启' : '未开启';
    hostsEl.className = 'apps-status-value ' + (hostsOn ? 'on' : 'off');
  }

  // Docker 加速状态：来自 status.docker_enabled。
  const dockerOn = !!(s.docker_enabled);
  const dockerEl = $('apps-docker-status');
  if (dockerEl) {
    dockerEl.textContent = dockerOn ? '已开启' : '未开启';
    dockerEl.className = 'apps-status-value ' + (dockerOn ? 'on' : 'off');
  }

  // 系统代理状态：来自 status.system_proxy_enabled（文件存在性）。
  const sysOn = !!(s.system_proxy_enabled);
  const sysEl = $('apps-sysproxy-status');
  if (sysEl) {
    sysEl.textContent = sysOn ? '已开启' : '未开启';
    sysEl.className = 'apps-status-value ' + (sysOn ? 'on' : 'off');
  }
  const sysToggle = $('sysproxy-toggle');
  if (sysToggle) sysToggle.checked = sysOn;
}

async function loadLogs() {
  const entries = await api('/api/logs?limit=120');
  const view = $('log-view');
  view.innerHTML = '';
  entries.forEach(appendLog);
}

/* ---------- 定时刷新 ---------- */
function stopAllTimers() {
  state.timers.forEach((t) => clearInterval(t));
  state.timers = [];
}

function startTimers() {
  stopAllTimers();
  state.timers.push(setInterval(() => {
    loadStatus().catch(() => {});
  }, 5000));
  state.timers.push(setInterval(() => {
    if ($('panel-mirrors').classList.contains('active')) {
      loadMirrors().catch(() => {});
    }
  }, 15000));
  state.timers.push(setInterval(() => {
    if ($('panel-hosts').classList.contains('active')) {
      loadHosts().catch(() => {});
    }
  }, 20000));
}

/* ---------- 事件绑定 ---------- */
function bindEvents() {
  $('login-form').addEventListener('submit', doLogin);
  $('btn-logout').addEventListener('click', () => doLogout(false));

  // 侧边栏折叠。
  const sidebar = $('sidebar');
  const toggleBtn = $('btn-sidebar-toggle');
  if (sidebar && toggleBtn) {
    toggleBtn.addEventListener('click', () => {
      sidebar.classList.toggle('collapsed');
      localStorage.setItem('sidebar-collapsed', sidebar.classList.contains('collapsed') ? '1' : '0');
      scheduleChartRedraw();
    });
    if (localStorage.getItem('sidebar-collapsed') === '1') sidebar.classList.add('collapsed');

    // 窄屏（≤960px）自动折叠为图标轨；回到宽屏时恢复用户偏好。
    const bp = window.matchMedia('(max-width: 960px)');
    const syncByViewport = (e) => {
      if (e.matches) {
        sidebar.classList.add('collapsed');
      } else if (localStorage.getItem('sidebar-collapsed') !== '1') {
        sidebar.classList.remove('collapsed');
      }
      scheduleChartRedraw();
    };
    if (bp.addEventListener) bp.addEventListener('change', syncByViewport);
    else bp.addListener(syncByViewport);
    if (bp.matches) sidebar.classList.add('collapsed');
  }

  // 窗口尺寸变化时重绘图表（防抖）。
  window.addEventListener('resize', scheduleChartRedraw);

  // 主题切换。
  const themeSelect = $('theme-select');
  const applyTheme = (t) => {
    document.documentElement.setAttribute('data-theme', t);
    if (themeSelect) themeSelect.value = t;
    localStorage.setItem('theme', t);
    drawCategoryChart();
  };
  const savedTheme = localStorage.getItem('theme') || 'indigo';
  applyTheme(savedTheme);
  if (themeSelect) {
    themeSelect.addEventListener('change', () => applyTheme(themeSelect.value));
  }

  $$('.tab').forEach((tab) => {
    tab.addEventListener('click', () => {
      $$('.tab').forEach((t) => t.classList.remove('active'));
      $$('.panel').forEach((p) => p.classList.remove('active'));
      tab.classList.add('active');
      const panel = $('panel-' + tab.dataset.tab);
      if (panel) panel.classList.add('active');

      // 切换标签时按需拉取该页数据。
      const loaders = {
        mirrors: loadMirrors,
        hosts: loadDNS,
        docker: loadDocker,
        apps: loadApps,
        logs: loadLogs,
        settings: loadConfig,
        about: loadAbout,
      };
      if (loaders[tab.dataset.tab]) {
        loaders[tab.dataset.tab]().catch((e) => toast(e.message, 'err'));
      }
      // 设置页额外加载证书状态。
      if (tab.dataset.tab === 'settings') {
        loadCert().catch(() => {});
      }
    });
  });

  $$('#mode-picker .mode-opt').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const mode = btn.dataset.mode;
      try {
        await api('/api/mode', {method: 'POST', body: {mode: mode}});
        toast('已切换到「' + btn.querySelector('strong').textContent + '」', 'ok');
        await Promise.all([loadStatus(), loadConfig()]);
      } catch (e) {
        toast(e.message, 'err');
      }
    });
  });

  $('btn-refresh').addEventListener('click', async (ev) => {
    const btn = ev.currentTarget;
    btn.disabled = true;
    btn.textContent = '测速中…';
    try {
      await api('/api/service', {method: 'POST', body: {action: 'refresh'}});
      toast('已开始测速与优化，约 30 秒后查看结果', 'ok');
      setTimeout(() => loadStatus().catch(() => {}), 30000);
    } catch (e) {
      toast(e.message, 'err');
    } finally {
      setTimeout(() => {
        btn.disabled = false;
        btn.textContent = '立即测速并优化';
      }, 3000);
    }
  });

  $('btn-restart').addEventListener('click', async () => {
    if (!confirm('确定重启代理服务？正在进行的下载会中断。')) return;
    try {
      await api('/api/service', {method: 'POST', body: {action: 'restart'}});
      toast('代理已重启', 'ok');
      setTimeout(() => loadStatus().catch(() => {}), 1500);
    } catch (e) {
      toast(e.message, 'err');
    }
  });

  // 总览页启停开关：调用 /api/service 的 start / stop。
  // 看门狗在 stop 流程下会让 rc=0 退出，不会自动拉起；start 时再重新 fork。
  $('btn-service-toggle').addEventListener('click', async () => {
    const running = !!(state.status && state.status.status && state.status.status.running);
    const action = running ? 'stop' : 'start';
    const confirmMsg = running
      ? '确定停止加速服务？所有 GitHub 加速通道将中断，需重新启动才会恢复。'
      : '确定启动加速服务？';
    if (!confirm(confirmMsg)) return;
    const btn = $('btn-service-toggle');
    btn.disabled = true;
    const origText = btn.textContent;
    btn.textContent = running ? '停止中…' : '启动中…';
    try {
      await api('/api/service', {method: 'POST', body: {action}});
      toast(running ? '服务已停止' : '服务已启动', 'ok');
      // 启动需要几秒初始化，停止后状态立即刷新。
      setTimeout(() => loadStatus().catch(() => {}), running ? 500 : 2500);
    } catch (e) {
      toast(e.message, 'err');
    } finally {
      // 状态刷新会重置按钮文案，这里只在出错时手动恢复。
      setTimeout(() => {
        btn.disabled = false;
        btn.textContent = origText;
      }, 600);
    }
  });

  // 接入方式区域：下拉切换 / 自定义输入 / 应用 / 保存为外网地址。
  $('access-host-select').addEventListener('change', () => {
    // 选"自定义……"时焦点跳到输入框，让用户直接键入；其他选项立即应用。
    if ($('access-host-select').value === '__custom__') {
      $('access-host-input').focus();
      return;
    }
    applyAccessHost();
  });
  $('access-host-input').addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') {
      ev.preventDefault();
      // 输入框有值时，回车 = 选"自定义"并应用。
      $('access-host-select').value = '__custom__';
      applyAccessHost();
    }
  });
  $('btn-access-apply').addEventListener('click', () => {
    // 输入框有值时强制走自定义路径。
    if ($('access-host-input').value.trim()) {
      $('access-host-select').value = '__custom__';
    }
    applyAccessHost();
  });
  $('btn-access-save-external').addEventListener('click', saveExternalHost);

  $('btn-test-all').addEventListener('click', async (ev) => {
    const btn = ev.currentTarget;
    btn.disabled = true;
    btn.textContent = '测速中…';
    try {
      await api('/api/mirrors/test-all', {method: 'POST'});
      toast('全量测速已在后台开始', 'ok');
      setTimeout(() => loadMirrors().catch(() => {}), 20000);
    } catch (e) {
      toast(e.message, 'err');
    } finally {
      setTimeout(() => {
        btn.disabled = false;
        btn.textContent = '全部测速';
      }, 3000);
    }
  });

  $('btn-add-mirror').addEventListener('click', () => openMirrorModal(null));
  $('mirror-form').addEventListener('submit', saveMirror);
  $('modal-close').addEventListener('click', () => { $('mirror-modal').hidden = true; });
  $('modal-cancel').addEventListener('click', () => { $('mirror-modal').hidden = true; });
  $('mirror-modal').addEventListener('click', (ev) => {
    if (ev.target === $('mirror-modal')) $('mirror-modal').hidden = true;
  });

  $('hosts-toggle').addEventListener('change', async (ev) => {
    const enabled = ev.currentTarget.checked;
    try {
      await api('/api/hosts', {method: 'PUT', body: {enabled: enabled}});
      toast(enabled ? '已启用 hosts 加速' : '已关闭并清理 hosts', 'ok');
      await Promise.all([loadConfig(), loadHosts()]);
    } catch (e) {
      ev.currentTarget.checked = !enabled;
      toast(e.message, 'err');
    }
  });

  // 看门狗自动重启开关：切换后立即写 config.yaml 与 watchdog_enabled 标记文件，
  // 下次进程崩溃时看门狗按新值决定是否拉起，无需重启服务。
  $('watchdog-toggle').addEventListener('change', async (ev) => {
    const enabled = ev.currentTarget.checked;
    try {
      await api('/api/watchdog', {method: 'PUT', body: {auto_restart: enabled}});
      toast(enabled ? '已开启崩溃自动重启' : '已关闭崩溃自动重启', 'ok');
      await loadConfig();
    } catch (e) {
      ev.currentTarget.checked = !enabled;
      toast(e.message, 'err');
    }
  });

  // 系统级 HTTP 代理开关：写入 /etc/profile.d/ghpp-proxy.sh，让新 login shell
  // 启动的软件默认走加速器。已运行的进程不受影响，需重启对应软件才生效。
  $('sysproxy-toggle').addEventListener('change', async (ev) => {
    const enabled = ev.currentTarget.checked;
    const confirmMsg = enabled
      ? '确定开启系统级 HTTP 代理？会在 /etc/profile.d/ghpp-proxy.sh 写入 http_proxy 指向 127.0.0.1:7710，所有新登录 shell 启动的软件默认走加速器。已运行的进程不受影响，需重启对应软件才生效。'
      : '确定关闭系统级 HTTP 代理？将删除 /etc/profile.d/ghpp-proxy.sh，新 shell 不再走代理。已运行的进程仍保留原环境变量直到重启。';
    if (!confirm(confirmMsg)) {
      ev.currentTarget.checked = !enabled;
      return;
    }
    try {
      await api('/api/sysproxy', {method: 'PUT', body: {enabled: enabled}});
      toast(enabled ? '已开启系统代理' : '已关闭系统代理', 'ok');
      // 刷新状态徽章（status.system_proxy_enabled 据文件存在性更新）。
      await loadStatus();
      renderApps();
    } catch (e) {
      ev.currentTarget.checked = !enabled;
      toast(e.message, 'err');
    }
  });

  // 「应用加速」页跳转按钮：切到对应 tab。
  $('btn-apps-go-hosts').addEventListener('click', () => {
    const tab = document.querySelector('.tab[data-tab="hosts"]');
    if (tab) tab.click();
  });
  // 应用加速页的 Docker 一键应用按钮：直接调 applyDocker（不跳转 tab）。
  $('btn-apps-docker-apply').addEventListener('click', applyDocker);
  $('btn-apps-scroll-sysproxy').addEventListener('click', () => {
    const card = document.querySelector('#panel-apps .card:nth-of-type(2)');
    if (card) card.scrollIntoView({behavior: 'smooth', block: 'start'});
  });

  $('btn-dns-sync').addEventListener('click', async () => {
    try {
      await api('/api/hosts', {method: 'POST'});
      toast('已开始 DNS 优选', 'ok');
      setTimeout(() => Promise.all([loadHosts(), loadConfig()]).catch(() => {}), 25000);
    } catch (e) {
      toast(e.message, 'err');
    }
  });

  $('btn-hosts-clear').addEventListener('click', async () => {
    if (!confirm('确定清理 hosts 中的加速记录？')) return;
    try {
      await api('/api/hosts', {method: 'DELETE'});
      toast('已清理 hosts', 'ok');
      await loadHosts();
    } catch (e) {
      toast(e.message, 'err');
    }
  });

  $('log-level').addEventListener('change', () => {
    const level = $('log-level').value;
    $$('#log-view .log-line').forEach((line) => {
      line.hidden = !!level && line.dataset.level !== level;
    });
  });

  $('log-follow').addEventListener('change', (ev) => {
    state.logAutoScroll = ev.currentTarget.checked;
    if (state.logAutoScroll) {
      const view = $('log-view');
      view.scrollTop = view.scrollHeight;
    }
  });

  $('log-view').addEventListener('scroll', (ev) => {
    const view = ev.currentTarget;
    // 用户手动上滚时暂停自动跟随，滚回底部后恢复。
    const atBottom = view.scrollHeight - view.scrollTop - view.clientHeight < 30;
    if (!atBottom && state.logAutoScroll) {
      $('log-follow').checked = false;
      state.logAutoScroll = false;
    }
  });

  $('btn-log-clear').addEventListener('click', () => { $('log-view').innerHTML = ''; });

  $('form-proxy').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    try {
      await api('/api/config', {
        method: 'PUT',
        body: {
          mode: state.config.mode,
          auto: state.config.auto,
          proxy: {
            listen: $('set-proxy-listen').value || '0.0.0.0:7710',
            connect_timeout_ms: Number($('set-connect-timeout').value),
            read_timeout_ms: Number($('set-read-timeout').value),
            failover_threshold: Number($('set-failover').value),
            cooldown_seconds: Number($('set-cooldown').value),
          },
          hosts: state.config.hosts,
        },
      });
      toast('代理参数已保存，监听地址修改后需重启代理生效', 'ok');
      await loadConfig();
    } catch (e) {
      toast(e.message, 'err');
    }
  });

  $('form-auto').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    try {
      await api('/api/config', {
        method: 'PUT',
        body: {
          mode: state.config.mode,
          proxy: state.config.proxy,
          hosts: state.config.hosts,
          auto: {
            probe_interval_minutes: Number($('set-probe-interval').value),
            direct_better_ratio: Number($('set-direct-ratio').value),
            min_improve_ratio: state.config.auto.min_improve_ratio || 1.3,
          },
        },
      });
      toast('决策参数已保存', 'ok');
      await loadConfig();
    } catch (e) {
      toast(e.message, 'err');
    }
  });

  $('form-password').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const oldPass = $('set-old-pass').value;
    const newPass = $('set-new-pass').value;
    if (newPass.length < 6) {
      toast('新密码至少 6 位', 'err');
      return;
    }
    try {
      await api('/api/password', {
        method: 'POST',
        body: {old_password: oldPass, new_password: newPass},
      });
      toast('密码已修改，请重新登录', 'ok');
      $('set-old-pass').value = '';
      $('set-new-pass').value = '';
      setTimeout(() => doLogout(false), 1200);
    } catch (e) {
      toast(e.message, 'err');
    }
  });

  // ---- Docker 加速 ----
  $('btn-docker-save').addEventListener('click', saveDocker);
  // Docker 加速页的一键应用按钮：写 daemon.json + 重启 Docker。
  $('btn-docker-apply').addEventListener('click', applyDocker);

  // ---- 证书管理 ----
  $('btn-cert-regenerate').addEventListener('click', async () => {
    if (!confirm('重新生成根证书？之前安装过证书的所有设备都需要重新安装。')) return;
    try {
      const data = await api('/api/cert/regenerate', {method: 'POST'});
      toast('已生成新证书并热生效', 'ok');
      loadCert().catch(() => {});
    } catch (e) {
      toast(e.message, 'err');
    }
  });

  $('form-cert-import').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const certPem = $('cert-pem').value.trim();
    const keyPem = $('cert-key').value.trim();
    if (!certPem || !keyPem) {
      toast('请同时填写证书与私钥（PEM 格式）', 'err');
      return;
    }
    try {
      await api('/api/cert/import', {
        method: 'POST',
        body: {cert_pem: certPem, key_pem: keyPem},
      });
      toast('证书已导入并热生效', 'ok');
      $('cert-pem').value = '';
      $('cert-key').value = '';
      loadCert().catch(() => {});
    } catch (e) {
      toast(e.message, 'err');
    }
  });

  // 复制按钮（事件委托，兼容动态生成的列表）。
  document.addEventListener('click', (ev) => {
    const btn = ev.target.closest('[data-copy],[data-copy-target]');
    if (!btn) return;
    // data-copy-target：复制另一个元素的当前值或文本。
    let text = btn.dataset.copy;
    if (btn.dataset.copyTarget) {
      const src = $(btn.dataset.copyTarget);
      if (!src) return;
      text = (src.tagName === 'TEXTAREA' || src.tagName === 'INPUT') ? src.value : src.textContent;
    }
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(
        () => toast('已复制到剪贴板', 'ok'),
        () => fallbackCopy(text)
      );
    } else {
      fallbackCopy(text);
    }
  });
}

function fallbackCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try {
    document.execCommand('copy');
    toast('已复制到剪贴板', 'ok');
  } catch (e) {
    toast('复制失败，请手动选择文本', 'err');
  }
  ta.remove();
}

/* ---------- 启动 ---------- */
async function boot() {
  try {
    await Promise.all([loadStatus(), loadConfig(), loadMirrors()]);
    startTimers();
    startLogStream();
  } catch (e) {
    toast(e.message, 'err');
    if (String(e.message).indexOf('登录') >= 0) {
      doLogout(true);
    }
  }
}

async function init() {
  bindEvents();

  if (!state.token) {
    showLogin();
    return;
  }

  // 用已有令牌探测服务，失败则回到登录页。
  try {
    await api('/api/health');
    showApp();
    await boot();
  } catch (e) {
    showLogin();
  }
}

document.addEventListener('DOMContentLoaded', init);
