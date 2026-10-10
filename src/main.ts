import { Hub, Settings, events, type Job, type Item, type MixEntry, type FollowedUser, type UserItem, type SettingsData, type HubStatus } from "./mygo";
import { platform, winCtl } from "./bridge";
import "./style.css";

const liveJobs = (jobs: (Job | null)[]): Job[] => jobs.filter((j): j is Job => j !== null);
const liveItems = (items: (Item | null)[] | undefined): Item[] => (items ?? []).filter((i): i is Item => i !== null);
const liveMixes = (mixes: (MixEntry | null)[] | undefined): MixEntry[] => (mixes ?? []).filter((x): x is MixEntry => x !== null);
const liveUsers = (users: (FollowedUser | null)[]): FollowedUser[] => users.filter((x): x is FollowedUser => x !== null);
const liveUserItems = (items: (UserItem | null)[] | undefined): UserItem[] => (items ?? []).filter((x): x is UserItem => x !== null);

// ---------- theme (UI-only preference, persisted in localStorage) ----------

type Theme = "system" | "light" | "dark";
const THEME_KEY = "vhub-theme";

const getTheme = (): Theme => (localStorage.getItem(THEME_KEY) as Theme) || "system";
const applyTheme = (t: Theme) => {
  localStorage.setItem(THEME_KEY, t);
  document.documentElement.dataset.theme = t;
};

// ---------- helpers ----------

const $ = <T extends HTMLElement = HTMLElement>(sel: string, root: ParentNode = document) =>
  root.querySelector<T>(sel)!;
const $$ = <T extends HTMLElement = HTMLElement>(sel: string, root: ParentNode = document) =>
  [...root.querySelectorAll<T>(sel)];

const esc = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);

const fmtBytes = (n: number) => {
  if (n <= 0) return "—";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n >= 100 ? Math.round(n) : n.toFixed(1)} ${u[i]}`;
};

const fmtTime = (iso?: string) => {
  if (!iso) return "";
  const d = new Date(iso);
  return isNaN(d.getTime()) ? "" : d.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
};

// fmtDay renders an aweme's unix-second publish time; same-year dates drop
// the year to keep the card caption short.
const fmtDay = (ts?: number) => {
  if (!ts) return "";
  const d = new Date(ts * 1000);
  if (isNaN(d.getTime())) return "";
  const md = `${d.getMonth() + 1}月${d.getDate()}日`;
  return d.getFullYear() === new Date().getFullYear() ? md : `${d.getFullYear()}年${md}`;
};

const TYPE_LABEL: Record<string, string> = {
  video: "视频",
  gallery: "图集",
  user: "主页",
  collection: "合集",
  music: "音乐",
};

const STATUS_LABEL: Record<string, string> = {
  pending: "排队中",
  resolving: "解析中",
  awaiting_pick: "待选择",
  downloading: "下载中",
  done: "已完成",
  failed: "失败",
  skipped: "已跳过",
};

const KIND_LABEL: Record<string, string> = {
  video: "视频",
  gallery: "图集",
  image: "图片",
};

// ---------- state ----------

type View = "download" | "tasks" | "users" | "settings";

const state = {
  view: (location.hash.slice(2) || "download") as View,
  jobs: [] as Job[],
  status: null as HubStatus | null,
  settings: null as SettingsData | null,
  users: [] as FollowedUser[],
  userInput: "",
  submitting: false,
  toast: "" as string,
};

// Users view state: latest-refresh items and checkbox picks per sec_uid.
const userItems = new Map<string, UserItem[]>();
const userPick = new Map<string, Set<string>>();
const userRefreshing = new Set<string>();

// Checkbox state for jobs awaiting selection; survives re-renders and is
// dropped once the job leaves awaiting_pick.
type PickState = { items: Set<string>; mixes: Set<string> };
const pickStates = new Map<string, PickState>();
function pickFor(j: Job): PickState {
  let ps = pickStates.get(j.id);
  if (!ps) {
    ps = {
      items: new Set(liveItems(j.items).map((i) => i.id)),
      mixes: new Set(),
    };
    pickStates.set(j.id, ps);
  }
  return ps;
}

let toastTimer = 0;
function toast(msg: string) {
  state.toast = msg;
  renderToast();
  clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => { state.toast = ""; renderToast(); }, 4200);
}

// ---------- app shell ----------

function shell() {
  const mac = platform() === "darwin";
  document.body.innerHTML = `
  <div class="app">
    <header class="titlebar">
      <div class="tb-brand">
        <span class="logo"><i></i></span>
        <span class="tb-title">VHub</span>
        <span class="tb-sub">抖音视频下载</span>
      </div>
      <div class="tb-status" id="tb-status"></div>
      ${mac ? "" : `
      <div class="tb-winbtns">
        <button data-win="minimize" title="最小化">–</button>
        <button data-win="toggleMaximize" title="最大化">▢</button>
        <button data-win="close" title="关闭">×</button>
      </div>`}
    </header>
    <div class="body">
      <aside class="sidebar">
        <nav class="nav">
          <button class="nav-item" data-view="download"><span class="nav-ico">⬇</span>下载</button>
          <button class="nav-item" data-view="users"><span class="nav-ico">♥</span>用户</button>
          <button class="nav-item" data-view="tasks"><span class="nav-ico">≡</span>任务<span class="nav-badge" id="nav-badge"></span></button>
          <button class="nav-item" data-view="settings"><span class="nav-ico">⚙</span>设置</button>
        </nav>
        <div class="side-foot">
          <div class="cap-note">
            <div class="cap-title">支持类型</div>
            <div class="caps">
              <span class="cap">视频</span><span class="cap">图集</span>
              <span class="cap">主页</span><span class="cap">合集</span><span class="cap">音乐</span>
            </div>
            <div class="cap-hint">受抖音风控影响，部分接口可能被拒绝</div>
          </div>
          <div class="cookie-state" id="cookie-state"></div>
        </div>
      </aside>
      <main class="main" id="main"></main>
    </div>
  </div>
  <div class="toast" id="toast"></div>`;

  $$(".tb-winbtns button").forEach((b) =>
    b.addEventListener("click", () => {
      const op = b.dataset.win as "minimize" | "toggleMaximize" | "close";
      winCtl()?.[op]();
    }),
  );
  $$(".nav-item").forEach((b) =>
    b.addEventListener("click", () => {
      state.view = b.dataset.view as View;
      location.hash = "/" + state.view;
      render();
    }),
  );
}

function renderChrome() {
  $$(".nav-item").forEach((b) => b.classList.toggle("active", b.dataset.view === state.view));
  const s = state.status;
  $("#tb-status").innerHTML = s
    ? `<span class="chip ${s.active > 0 ? "chip-run" : ""}">${s.active > 0 ? `下载中 ${s.active}` : "空闲"}</span>
       ${s.failed > 0 ? `<span class="chip chip-fail">${s.failed} 失败</span>` : ""}`
    : "";
  const active = state.jobs.filter((j) => ["pending", "resolving", "awaiting_pick", "downloading"].includes(j.status)).length;
  $("#nav-badge").textContent = active > 0 ? String(active) : "";
  $("#cookie-state").innerHTML = s
    ? `<span class="dot ${s.hasCookie ? "dot-ok" : "dot-warn"}"></span>${s.hasCookie ? "Cookie 已配置" : "未配置 Cookie"}`
    : "";
}

function renderToast() {
  const t = $("#toast");
  t.textContent = state.toast;
  t.classList.toggle("show", !!state.toast);
}

// ---------- views ----------

function viewDownload() {
  const m = $("#main");
  m.innerHTML = `
  <section class="page">
    <div class="hero card">
      <h2>下载抖音内容</h2>
      <p class="hint">粘贴抖音分享文本或链接，或直接粘贴用户 ID（sec_uid）。主页任务会先列出作品与合集供勾选后再下载，并自动加入「用户」页关注列表。</p>
      <textarea id="url-input" rows="5" placeholder="例：8.94 复制打开抖音，看看【xxx的作品】 https://v.douyin.com/xxxxx/ …" spellcheck="false"></textarea>
      <div class="hero-actions">
        <button class="btn-primary" id="btn-submit" ${state.submitting ? "disabled" : ""}>
          ${state.submitting ? "解析中…" : "解析并下载"}
        </button>
        <button class="btn-ghost" id="btn-paste">从剪贴板粘贴</button>
      </div>
      <div class="submit-report" id="submit-report"></div>
    </div>
    <div class="queue-head">
      <h3>进行中</h3>
      <button class="btn-ghost small" id="goto-tasks">查看全部 →</button>
    </div>
    <div id="active-jobs"></div>
  </section>`;

  $("#btn-paste").addEventListener("click", async () => {
    try {
      const t = await navigator.clipboard.readText();
      ($("#url-input") as HTMLTextAreaElement).value = t;
    } catch {
      toast("无法读取剪贴板");
    }
  });
  $("#goto-tasks").addEventListener("click", () => {
    state.view = "tasks"; location.hash = "/tasks"; render();
  });
  $("#btn-submit").addEventListener("click", async () => {
    const text = ($("#url-input") as HTMLTextAreaElement).value.trim();
    if (!text) { toast("请先粘贴链接"); return; }
    state.submitting = true;
    render();
    try {
      const r = await Hub.submit(text);
      const parts: string[] = [];
      if (r.created.length) parts.push(`已创建 ${r.created.length} 个任务`);
      if (r.skipped.length) parts.push(`${r.skipped.length} 条链接无法识别`);
      if (r.failed.length) parts.push(`${r.failed.length} 条解析失败`);
      $("#submit-report").innerHTML =
        parts.map((p) => `<span>${esc(p)}</span>`).join("") +
        (r.skipped.length || r.failed.length
          ? `<details><summary>明细</summary>${[...r.skipped, ...r.failed].map((u) => `<div class="rep-line">${esc(u)}</div>`).join("")}</details>`
          : "");
      if (r.created.length) {
        ($("#url-input") as HTMLTextAreaElement).value = "";
        state.view = "tasks"; location.hash = "/tasks"; render();
      }
    } catch (e) {
      toast(e instanceof Error ? e.message : String(e));
    } finally {
      state.submitting = false;
      if (state.view === "download") render();
    }
  });

  const active = state.jobs.filter((j) => !["done", "failed", "skipped"].includes(j.status)).slice(-4).reverse();
  const host = $("#active-jobs");
  host.innerHTML = active.length
    ? active.map(renderJobCard).join("")
    : `<div class="empty-mini">暂无进行中的任务</div>`;
  bindJobActions(host);
}

function viewTasks() {
  const m = $("#main");
  const jobs = [...state.jobs].reverse();
  const finished = jobs.filter((j) => ["done", "failed", "skipped"].includes(j.status)).length;
  m.innerHTML = `
  <section class="page">
    <div class="queue-head">
      <h3>任务队列 <span class="muted">${jobs.length} 个任务</span></h3>
      ${finished ? `<button class="btn-ghost small" id="clear-done">清除已完成 (${finished})</button>` : ""}
    </div>
    <div id="job-list"></div>
  </section>`;
  $("#clear-done")?.addEventListener("click", () => run(Hub.clearFinished()));
  const host = $("#job-list");
  host.innerHTML = jobs.length
    ? jobs.map(renderJobCard).join("")
    : `<div class="empty">
        <div class="empty-ico">⬇</div>
        <div>还没有下载任务</div>
        <button class="btn-primary" id="empty-go">去下载</button>
      </div>`;
  $("#empty-go")?.addEventListener("click", () => { state.view = "download"; location.hash = "/download"; render(); });
  bindJobActions(host);
}

function renderJobCard(j: Job): string {
  const items = liveItems(j.items);
  const pct = j.total > 0 ? Math.round(((j.done + j.failed + j.skipped) / j.total) * 100) : 0;
  const bar =
    j.status === "downloading" && items.length
      ? items.map((it) => renderItemRow(it)).join("")
      : "";
  const picking = j.status === "awaiting_pick";
  const ps = picking ? pickFor(j) : null;
  if (!picking) pickStates.delete(j.id);
  return `
  <div class="job card st-${j.status}" data-job="${j.id}">
    <div class="job-top">
      <span class="type-tag t-${j.type}">${TYPE_LABEL[j.type] ?? j.type}</span>
      <div class="job-title" title="${esc(j.title)}">${esc(j.title)}</div>
      <span class="badge b-${j.status}">${STATUS_LABEL[j.status] ?? j.status}</span>
    </div>
    <div class="job-meta">
      <span class="mono url">${esc(j.url)}</span>
      <span class="meta-right">
        ${j.total ? `${j.done}/${j.total} 完成${j.failed ? ` · ${j.failed} 失败` : ""}${j.skipped ? ` · ${j.skipped} 跳过` : ""} · ${pct}%` : ""}
        ${fmtTime(j.createdAt)}
      </span>
    </div>
    ${j.error ? `<div class="job-err">${esc(j.error)}</div>` : ""}
    ${picking && ps ? renderPickList(j, ps) : ""}
    ${bar ? `<div class="items">${bar}</div>` : ""}
    <div class="job-actions">
      ${picking ? `
        <button class="btn-ghost small" data-act="pick-all">全选</button>
        <button class="btn-ghost small" data-act="pick-none">清空</button>
        <button class="btn-ghost small" data-act="remove">取消</button>
        <button class="btn-primary small" data-act="pick-submit">下载选中 (${ps!.items.size + ps!.mixes.size})</button>` : ""}
      ${j.status === "failed" ? `<button class="btn-ghost small" data-act="retry">重试</button>` : ""}
      ${["done", "failed", "skipped"].includes(j.status) ? `<button class="btn-ghost small" data-act="remove">移除</button>` : ""}
      ${!picking && items.some((i) => i.path) ? `<button class="btn-ghost small" data-act="reveal">在文件夹中显示</button>` : ""}
    </div>
  </div>`;
}

function pickCard(
  pick: "i" | "m" | "u",
  id: string,
  title: string,
  cover: string | undefined,
  sub: string,
  on: boolean,
  isNew = false,
): string {
  return `
    <label class="pick-card ${on ? "on" : ""}">
      <input type="checkbox" data-pick="${pick}" data-id="${esc(id)}" ${on ? "checked" : ""} />
      <span class="pick-thumb">${cover ? `<img src="${esc(cover)}" loading="lazy" referrerpolicy="no-referrer" alt="" />` : `<i class="pick-ph">${pick === "m" ? "⊞" : "▷"}</i>`}</span>
      ${isNew ? `<span class="pick-new">新</span>` : ""}
      <span class="pick-check">✓</span>
      <span class="pick-cap">
        <span class="pick-title">${esc(title)}</span>
        <span class="pick-kind">${sub}</span>
      </span>
    </label>`;
}

// itemSub builds the caption line under a pick card's title:
// "视频 · 10月9日" (date omitted when the aweme lacks create_time).
const itemSub = (kindLabel: string, ts?: number) => {
  const day = fmtDay(ts);
  return day ? `${kindLabel} · ${day}` : kindLabel;
};

function renderPickList(j: Job, ps: PickState): string {
  const items = liveItems(j.items);
  const mixes = liveMixes(j.mixes);
  const sec = (title: string, cards: string) => `
    <div class="pick-sec">
      <div class="pick-head">${title}</div>
      <div class="pick-grid">${cards}</div>
    </div>`;
  return `
  <div class="pick">
    ${items.length ? sec(`作品 ${items.length}`, items.map((it) =>
      pickCard("i", it.id, it.title, it.cover, itemSub(KIND_LABEL[it.kind] ?? it.kind, it.time), ps.items.has(it.id))).join("")) : ""}
    ${mixes.length ? sec(`合集 ${mixes.length}`, mixes.map((mx) =>
      pickCard("m", mx.id, mx.title, mx.cover, mx.count ? `${mx.count} 个视频` : "合集", ps.mixes.has(mx.id))).join("")) : ""}
  </div>`;
}

function renderItemRow(it: Item): string {
  const pct = it.bytesTotal > 0 ? Math.min(100, Math.round((it.bytesDone / it.bytesTotal) * 100)) : (it.status === "done" ? 100 : 0);
  return `
  <div class="item">
    <div class="item-top">
      <span class="item-title">${esc(it.title)}</span>
      <span class="item-st">${STATUS_LABEL[it.status] ?? it.status}</span>
    </div>
    <div class="bar"><i style="width:${pct}%"></i></div>
    <div class="item-meta">${fmtBytes(it.bytesDone)} / ${fmtBytes(it.bytesTotal)}${it.error ? ` · <span class="err">${esc(it.error)}</span>` : ""}</div>
  </div>`;
}

function bindJobActions(root: ParentNode) {
  $$("[data-act]", root).forEach((b) =>
    b.addEventListener("click", () => {
      const jobEl = b.closest("[data-job]") as HTMLElement;
      const jobId = jobEl?.dataset.job ?? "";
      const job = state.jobs.find((j) => j.id === jobId);
      const act = b.dataset.act;
      if (act === "retry") run(Hub.retryJob(jobId));
      if (act === "remove") run(Hub.removeJob(jobId));
      if (act === "reveal") {
        const p = liveItems(job?.items).find((i) => i.path)?.path;
        if (p) run(Hub.revealItem(p));
      }
      const ps = job && pickStates.get(job.id);
      if ((act === "pick-all" || act === "pick-none") && job && ps) {
        const all = act === "pick-all";
        ps.items = new Set(all ? liveItems(job.items).map((i) => i.id) : []);
        ps.mixes = new Set(all ? liveMixes(job.mixes).map((x) => x.id) : []);
        $$<HTMLInputElement>("input[data-pick]", jobEl).forEach((cb) => {
          cb.checked = (cb.dataset.pick === "i" ? ps.items : ps.mixes).has(cb.dataset.id!);
          cb.closest(".pick-card")?.classList.toggle("on", cb.checked);
        });
        refreshPickCount(jobEl);
      }
      if (act === "pick-submit" && ps) {
        run(Hub.pickJob(jobId, [...ps.items], [...ps.mixes]));
      }
    }),
  );
  (root as HTMLElement).addEventListener("change", (e) => {
    const cb = e.target as HTMLInputElement;
    if (!cb.matches?.("input[data-pick]")) return;
    const jobEl = cb.closest("[data-job]") as HTMLElement;
    const ps = pickStates.get(jobEl?.dataset.job ?? "");
    if (!ps) return;
    const set = cb.dataset.pick === "i" ? ps.items : ps.mixes;
    if (cb.checked) set.add(cb.dataset.id!); else set.delete(cb.dataset.id!);
    cb.closest(".pick-card")?.classList.toggle("on", cb.checked);
    refreshPickCount(jobEl);
  });
}

function refreshPickCount(jobEl: Element) {
  const ps = pickStates.get((jobEl as HTMLElement).dataset.job ?? "");
  const btn = jobEl.querySelector('[data-act="pick-submit"]');
  if (ps && btn) btn.textContent = `下载选中 (${ps.items.size + ps.mixes.size})`;
}

// ---------- users view ----------

function viewUsers() {
  const m = $("#main");
  const users = state.users;
  m.innerHTML = `
  <section class="page">
    <div class="hero card">
      <h2>关注的抖音用户</h2>
      <p class="hint">粘贴主页链接或用户 ID 纳入关注；「检查更新」列出最新作品，勾选后批量下载。</p>
      <div class="field">
        <div class="row">
          <input id="u-input" class="mono" placeholder="https://www.douyin.com/user/MS4wLjAB… 或 sec_uid" spellcheck="false" value="${esc(state.userInput)}" />
          <button class="btn-primary" id="u-add">添加</button>
        </div>
      </div>
    </div>
    <div id="user-list"></div>
  </section>`;

  $("#u-add").addEventListener("click", async () => {
    const input = $("#u-input") as HTMLInputElement;
    const text = input.value.trim();
    if (!text) { toast("请输入主页链接或用户 ID"); return; }
    try {
      const f = await Hub.addUser(text);
      if (f) {
        input.value = "";
        state.userInput = "";
        toast(`已关注 ${f.nickname || "该用户"}`);
      }
    } catch (e) {
      toast(e instanceof Error ? e.message : String(e));
    }
  });
  ($("#u-input") as HTMLInputElement).addEventListener("input", (e) => {
    state.userInput = (e.target as HTMLInputElement).value;
  });
  const host = $("#user-list");
  host.innerHTML = users.length
    ? users.map(renderUserCard).join("")
    : `<div class="empty-mini">还没有关注的用户</div>`;
  bindUserActions(host);
}

function renderUserCard(u: FollowedUser): string {
  const hasRefresh = userItems.has(u.secUID);
  const items = userItems.get(u.secUID) ?? [];
  const ps = userPick.get(u.secUID) ?? new Set<string>();
  const newCount = items.filter((i) => i.new).length;
  const checked = u.lastCheck && !u.lastCheck.startsWith("0001") ? ` · 上次检查 ${fmtTime(u.lastCheck)}` : "";
  return `
  <div class="user card" data-user="${esc(u.secUID)}">
    <div class="user-top">
      ${u.avatar ? `<img class="user-ava" src="${esc(u.avatar)}" referrerpolicy="no-referrer" alt="" />` : `<span class="user-ava">音</span>`}
      <div class="user-info">
        <div class="user-name">${esc(u.nickname || "抖音用户")}</div>
        <div class="user-meta mono">${esc(u.secUID.slice(0, 26))}…${checked}</div>
      </div>
      <div class="user-ops">
        <button class="btn-ghost small" data-uact="refresh" ${userRefreshing.has(u.secUID) ? "disabled" : ""}>${userRefreshing.has(u.secUID) ? "更新中…" : "检查更新"}</button>
        <button class="btn-ghost small" data-uact="remove">移除</button>
      </div>
    </div>
    ${hasRefresh ? `
    <div class="user-items">
      ${items.length ? `
        ${newCount ? `<div class="user-new">${newCount} 条新内容</div>` : `<div class="user-new none">没有新内容</div>`}
        <div class="pick-grid">
          ${items.map((it) => pickCard("u", it.id, it.title, it.cover, itemSub(KIND_LABEL[it.kind] ?? it.kind, it.time), ps.has(it.id), it.new)).join("")}
        </div>
        <div class="job-actions">
          <button class="btn-ghost small" data-uact="pick-all">全选</button>
          <button class="btn-ghost small" data-uact="pick-none">清空</button>
          <button class="btn-primary small" data-uact="download">下载选中 (${ps.size})</button>
        </div>` : `<div class="empty-mini">没有获取到作品</div>`}
    </div>` : ""}
  </div>`;
}

function bindUserActions(root: ParentNode) {
  $$("[data-uact]", root).forEach((b) =>
    b.addEventListener("click", async () => {
      const el = b.closest("[data-user]") as HTMLElement;
      const sec = el?.dataset.user ?? "";
      const act = (b as HTMLElement).dataset.uact;
      if (act === "refresh") {
        userRefreshing.add(sec);
        render();
        try {
          const r = await Hub.refreshUser(sec);
          if (r) {
            const items = liveUserItems(r.items);
            userItems.set(sec, items);
            userPick.set(sec, new Set(items.filter((i) => i.new).map((i) => i.id)));
          }
        } catch (e) {
          toast(e instanceof Error ? e.message : String(e));
        }
        userRefreshing.delete(sec);
        render();
      }
      if (act === "remove") {
        await run(Hub.removeUser(sec));
        userItems.delete(sec);
        userPick.delete(sec);
        render();
      }
      if (act === "pick-all" || act === "pick-none") {
        const ps = new Set<string>(act === "pick-all" ? (userItems.get(sec) ?? []).map((i) => i.id) : []);
        userPick.set(sec, ps);
        $$<HTMLInputElement>("input[data-pick]", el).forEach((cb) => {
          cb.checked = ps.has(cb.dataset.id!);
          cb.closest(".pick-card")?.classList.toggle("on", cb.checked);
        });
        refreshUserPickCount(el);
      }
      if (act === "download") {
        const ps = userPick.get(sec);
        if (!ps || !ps.size) { toast("先勾选要下载的内容"); return; }
        try {
          const jobId = await Hub.downloadUserItems(sec, [...ps]);
          toast(`已创建下载任务 ${jobId}`);
        } catch (e) {
          toast(e instanceof Error ? e.message : String(e));
        }
      }
    }),
  );
  (root as HTMLElement).addEventListener("change", (e) => {
    const cb = e.target as HTMLInputElement;
    if (!cb.matches?.("input[data-pick]")) return;
    const el = cb.closest("[data-user]") as HTMLElement;
    const ps = userPick.get(el?.dataset.user ?? "");
    if (!ps) return;
    if (cb.checked) ps.add(cb.dataset.id!); else ps.delete(cb.dataset.id!);
    cb.closest(".pick-card")?.classList.toggle("on", cb.checked);
    refreshUserPickCount(el);
  });
}

function refreshUserPickCount(el: Element) {
  const ps = userPick.get((el as HTMLElement).dataset.user ?? "");
  const btn = el.querySelector('[data-uact="download"]');
  if (ps && btn) btn.textContent = `下载选中 (${ps.size})`;
}

function viewSettings() {
  const m = $("#main");
  const s = state.settings;
  m.innerHTML = `
  <section class="page narrow">
    <div class="card form">
      <h2>设置</h2>
      <div class="field">
        <span class="label">外观</span>
        <div class="seg" id="f-theme">
          <button data-t="system" ${getTheme() === "system" ? 'class="active"' : ""}>跟随系统</button>
          <button data-t="light" ${getTheme() === "light" ? 'class="active"' : ""}>浅色</button>
          <button data-t="dark" ${getTheme() === "dark" ? 'class="active"' : ""}>深色</button>
        </div>
      </div>
      <label class="field">
        <span class="label">下载目录</span>
        <div class="row">
          <input id="f-dir" class="mono" value="${esc(s?.downloadDir ?? "")}" />
          <button class="btn-ghost" id="pick-dir">选择…</button>
        </div>
      </label>
      <label class="field">
        <span class="label">Cookie <em>登录后的抖音 Cookie，用于解锁更多内容</em></span>
        <textarea id="f-cookie" rows="3" placeholder="sessionid=…; msToken=…; ttwid=…" spellcheck="false">${esc(s?.cookie ?? "")}</textarea>
      </label>
      <div class="field">
        <span class="label">应用内登录 <em>列表/详情接口被抖音风控，需经应用内窗口签名；扫码登录一次即可</em></span>
        <button class="btn-ghost" id="login-open">打开登录窗口</button>
      </div>
      <div class="grid2">
        <label class="field"><span class="label">视频画质</span>
          <select id="f-quality">
            <option value="highest" ${s?.quality !== "lowest" ? "selected" : ""}>最高</option>
            <option value="lowest" ${s?.quality === "lowest" ? "selected" : ""}>最低（省空间）</option>
          </select>
        </label>
        <label class="field"><span class="label">并发下载数</span>
          <input id="f-workers" type="number" min="1" max="8" value="${s?.workers ?? 3}" />
        </label>
        <label class="field"><span class="label">API 限速（次/秒）</span>
          <input id="f-rate" type="number" min="0.5" max="10" step="0.5" value="${s?.rateLimit ?? 2}" />
        </label>
        <label class="field"><span class="label">代理 <em>可选，http/socks5</em></span>
          <input id="f-proxy" class="mono" placeholder="socks5://127.0.0.1:7890" value="${esc(s?.proxy ?? "")}" />
        </label>
      </div>
      <div class="form-actions">
        <button class="btn-primary" id="save-settings">保存</button>
        <button class="btn-ghost" id="open-dir">打开下载目录</button>
      </div>
    </div>
    <div class="card note">
      <h4>关于风控</h4>
      <p>抖音自 2026-08 起对多数 Web API 启用了 ArgusSecurityPlugin 风控门禁。
      本应用经应用内窗口由抖音页面 SDK 自动签名请求；部分接口需要登录态，
      建议在上方「应用内登录」扫码一次。Cookie 输入框仅用于直连兜底与无水印下载。
      视频直链优先走 CDN 无水印地址，失败时自动降级到签名播放端点。</p>
    </div>
  </section>`;

  $$("#f-theme button").forEach((b) =>
    b.addEventListener("click", () => {
      applyTheme(b.dataset.t as Theme);
      $$("#f-theme button").forEach((x) => x.classList.toggle("active", x === b));
    }),
  );
  $("#pick-dir").addEventListener("click", async () => {
    try {
      const p = await Settings.pickDownloadDir();
      if (p) ($("#f-dir") as HTMLInputElement).value = p;
    } catch (e) { toast(String(e)); }
  });
  $("#open-dir").addEventListener("click", () => run(Settings.openDownloadDir()));
  $("#login-open").addEventListener("click", () => run(Hub.openLogin()));
  $("#save-settings").addEventListener("click", async () => {
    const next: SettingsData = {
      downloadDir: ($("#f-dir") as HTMLInputElement).value.trim(),
      cookie: ($("#f-cookie") as HTMLTextAreaElement).value.trim(),
      proxy: ($("#f-proxy") as HTMLInputElement).value.trim(),
      quality: ($("#f-quality") as HTMLSelectElement).value,
      workers: Number(($("#f-workers") as HTMLInputElement).value) || 3,
      rateLimit: Number(($("#f-rate") as HTMLInputElement).value) || 2,
    };
    try {
      await Settings.update(next);
      state.settings = next;
      toast("设置已保存");
      refreshStatus();
    } catch (e) {
      toast(e instanceof Error ? e.message : String(e));
    }
  });
}

async function run(p: Promise<unknown>) {
  try {
    await p;
  } catch (e) {
    toast(e instanceof Error ? e.message : String(e));
  }
}

async function refreshStatus() {
  try {
    state.status = await Hub.status();
    renderChrome();
  } catch { /* bridge not ready yet */ }
}

// ---------- render ----------

function render() {
  renderChrome();
  if (state.view === "download") viewDownload();
  else if (state.view === "tasks") viewTasks();
  else if (state.view === "users") viewUsers();
  else viewSettings();
}

async function boot() {
  shell();
  render();
  const [jobs, settings, users] = await Promise.all([Hub.jobs(), Settings.get(), Hub.users()]);
  state.jobs = liveJobs(jobs);
  state.settings = settings;
  state.users = liveUsers(users);
  render();
  refreshStatus();
  events.vhubUsers.on((us) => {
    state.users = liveUsers(us);
    if (state.view === "users") render();
  });
  events.vhubJobs.on((jobs) => {
    state.jobs = liveJobs(jobs);
    renderChrome();
    // live-update lists without nuking a settings form mid-edit
    if (state.view !== "settings") render();
  });
  setInterval(refreshStatus, 5000);
}

window.addEventListener("hashchange", () => {
  const v = location.hash.slice(2) as View;
  if (v && v !== state.view) { state.view = v; render(); }
});

boot().catch((e) => {
  document.body.innerHTML = `<div class="boot-err">初始化失败：${esc(String(e))}</div>`;
});
