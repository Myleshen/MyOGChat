/* MyOGChat — vanilla JS SPA (no build step). Depends on vendored marked + highlight.js. */
"use strict";

/* ============================== DOM refs ============================== */

const loginView = document.getElementById("login-view");
const loginForm = document.getElementById("login-form");
const loginToken = document.getElementById("login-token");
const loginError = document.getElementById("login-error");
const loginBtn = document.getElementById("login-btn");

const appEl = document.getElementById("app");
const sidebar = document.getElementById("sidebar");
const backdrop = document.getElementById("backdrop");
const menuBtn = document.getElementById("menu-btn");
const collapseBtn = document.getElementById("collapse-btn");
const newChatBtn = document.getElementById("new-chat-btn");
const newChatLabel = document.getElementById("new-chat-label");
const logoutBtn = document.getElementById("logout-btn");
const newProjectBtn = document.getElementById("new-project-btn");
const projectsList = document.getElementById("projects-list");
const projectsEmpty = document.getElementById("projects-empty");
const convoList = document.getElementById("convo-list");
const convoEmpty = document.getElementById("convo-empty");
const convoLoading = document.getElementById("convo-loading");

const modelSelect = document.getElementById("model-select");
const modelThinkingBadge = document.getElementById("model-thinking-badge");
const effortSelect = document.getElementById("effort-select");
const totalCostEl = document.getElementById("total-cost");
const settingsBtn = document.getElementById("settings-btn");

const messagesEl = document.getElementById("messages");
const welcomeEl = document.getElementById("welcome");
const msgsLoading = document.getElementById("msgs-loading");

const composer = document.getElementById("composer");
const input = document.getElementById("input");
const sendBtn = document.getElementById("send-btn");
const stopBtn = document.getElementById("stop-btn");

const toastEl = document.getElementById("toast");

const hljsDark = document.getElementById("hljs-dark");
const hljsLight = document.getElementById("hljs-light");

const settingsModal = document.getElementById("settings-modal");
const settingsClose = document.getElementById("settings-close");
const settingShowTps = document.getElementById("setting-showTps");
const settingEnterToSend = document.getElementById("setting-enterToSend");

const projectModal = document.getElementById("project-modal");
const projectForm = document.getElementById("project-form");
const projectModalTitle = document.getElementById("project-modal-title");
const projectName = document.getElementById("project-name");
const projectInstructions = document.getElementById("project-instructions");
const projectModel = document.getElementById("project-model");
const projectClose = document.getElementById("project-close");
const projectCancel = document.getElementById("project-cancel");

/* ============================== State ============================== */

const state = {
  models: [],              // GET /api/models registry
  conversations: [],       // GET /api/conversations (server order)
  projects: [],            // GET /api/projects
  expandedProjects: new Set(), // project ids whose chats are shown inline
  activeProjectId: null,   // project new chats are filed into
  current: null,           // open conversation object (with messages)
  currentMessages: [],     // messages of the open conversation
  streaming: false,
  abort: null,             // AbortController while streaming
};

const JSON_HEADERS = { "Content-Type": "application/json" };

const ICONS = {
  pencil:
    '<svg viewBox="0 0 16 16" width="16" height="16" fill="currentColor" aria-hidden="true">' +
    '<path d="M11.013 1.427a1.75 1.75 0 0 1 2.474 0l1.086 1.086a1.75 1.75 0 0 1 0 2.474l-8.61 8.61c-.21.21-.47.364-.756.445l-3.251.93a.75.75 0 0 1-.927-.928l.929-3.25c.081-.286.235-.547.445-.758l8.61-8.61Zm1.46 1.061a.25.25 0 0 0-.353 0l-8.61 8.61a.746.746 0 0 0-.126.167l-.722 2.528 2.529-.722a.75.75 0 0 0 .167-.126l8.61-8.61a.25.25 0 0 0 0-.353Z"/></svg>',
  trash:
    '<svg viewBox="0 0 16 16" width="16" height="16" fill="currentColor" aria-hidden="true">' +
    '<path d="M11 1.75V3h2.25a.75.75 0 0 1 0 1.5H2.75a.75.75 0 0 1 0-1.5H5V1.75C5 .784 5.784 0 6.75 0h2.5C10.216 0 11 .784 11 1.75ZM4.496 6.675l.66 6.6a.25.25 0 0 0 .249.225h5.19a.25.25 0 0 0 .249-.225l.66-6.6a.75.75 0 0 1 1.492.149l-.66 6.6A1.748 1.748 0 0 1 10.595 15h-5.19a1.75 1.75 0 0 1-1.741-1.575l-.66-6.6a.75.75 0 1 1 1.492-.15ZM6.75 2.5h2.5V1.75a.25.25 0 0 0-.25-.25h-2.5a.25.25 0 0 0-.25.25Z"/></svg>',
  more:
    '<svg viewBox="0 0 16 16" width="16" height="16" fill="currentColor" aria-hidden="true">' +
    '<circle cx="3" cy="8" r="1.5"/><circle cx="8" cy="8" r="1.5"/><circle cx="13" cy="8" r="1.5"/></svg>',
  folder:
    '<svg viewBox="0 0 16 16" width="16" height="16" fill="currentColor" aria-hidden="true">' +
    '<path d="M1.75 2.5A1.75 1.75 0 0 0 0 4.25v7.5c0 .966.784 1.75 1.75 1.75h12.5A1.75 1.75 0 0 0 16 11.75v-6.5A1.75 1.75 0 0 0 14.25 3.5H7.81L6.56 2.25A1.5 1.5 0 0 0 5.5 1.75H1.75Z"/></svg>',
};

/* ============================== Settings ============================== */

/* Namespaced localStorage keys (contract §8.8). */
const settings = {
  theme: "system",        // myogchat.theme: system|light|dark
  thinking: "collapsed",  // myogchat.thinking: expanded|collapsed|hidden
  showTps: true,          // myogchat.showTps: 0|1
  enterToSend: true,      // myogchat.enterToSend: 0|1
  sidebarCollapsed: false, // myogchat.sidebarCollapsed: 0|1
};

const prefersLight = window.matchMedia("(prefers-color-scheme: light)");

function lsGet(key, fallback) {
  try {
    const v = localStorage.getItem(key);
    return v == null ? fallback : v;
  } catch {
    return fallback;
  }
}

function lsSet(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* storage unavailable */
  }
}

function loadSettings() {
  const theme = lsGet("myogchat.theme", "system");
  settings.theme = theme === "light" || theme === "dark" ? theme : "system";

  const thinking = lsGet("myogchat.thinking", "collapsed");
  settings.thinking =
    thinking === "expanded" || thinking === "hidden" ? thinking : "collapsed";

  settings.showTps = lsGet("myogchat.showTps", "1") === "1";
  settings.enterToSend = lsGet("myogchat.enterToSend", "1") === "1";
  settings.sidebarCollapsed = lsGet("myogchat.sidebarCollapsed", "0") === "1";
}

function effectiveTheme() {
  if (settings.theme === "system") return prefersLight.matches ? "light" : "dark";
  return settings.theme;
}

/* Set <html data-theme> and toggle the correct highlight stylesheet. */
function applyTheme() {
  const theme = effectiveTheme();
  document.documentElement.setAttribute("data-theme", theme);
  const dark = theme === "dark";
  if (hljsDark) hljsDark.disabled = !dark;
  if (hljsLight) hljsLight.disabled = dark;
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute("content", dark ? "#0d1117" : "#ffffff");
}

function applySidebarCollapsed() {
  appEl.classList.toggle("sidebar-collapsed", settings.sidebarCollapsed);
  collapseBtn.setAttribute("aria-expanded", settings.sidebarCollapsed ? "false" : "true");
  const label = settings.sidebarCollapsed ? "Expand sidebar" : "Collapse sidebar";
  collapseBtn.setAttribute("aria-label", label);
  collapseBtn.title = label;
}

function applyEnterHint() {
  input.enterKeyHint = settings.enterToSend ? "send" : "enter";
}

/* ============================== Helpers ============================== */

class AuthError extends Error {}

function el(tag, className) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  return node;
}

function enc(s) {
  return encodeURIComponent(s);
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  }[c]));
}

let toastTimer = null;
function toast(message) {
  toastEl.textContent = message;
  toastEl.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    toastEl.hidden = true;
  }, 4000);
}

function modelById(id) {
  return state.models.find((m) => m.id === id) || null;
}

function modelName(id) {
  const m = modelById(id);
  return m ? m.name : id || "";
}

function isFreeModel(id) {
  const m = modelById(id);
  return !!m && m.input_per_m === 0 && m.output_per_m === 0;
}

function isResponsesModel(id) {
  const m = modelById(id);
  return !!m && m.dialect === "responses";
}

function projectById(id) {
  return state.projects.find((p) => p.id === id) || null;
}

function modelOptionLabel(m) {
  return m.name + (m.thinking ? " · thinking" : "");
}

/* micro-dollars -> display string; null when no cost info */
function fmtMicros(micros) {
  if (typeof micros !== "number") return null;
  if (micros > 0 && micros < 100) return "<$0.0001";
  return "$" + (micros / 1e6).toFixed(4);
}

function fmtMsgCost(micros, modelId) {
  if (modelId && isFreeModel(modelId)) return "Free";
  return fmtMicros(micros);
}

/* duration_ms -> compact human string, e.g. 0.8s / 3.2s / 1m 12s */
function fmtDuration(ms) {
  if (typeof ms !== "number" || ms < 0) return "";
  if (ms < 1000) return (ms / 1000).toFixed(1) + "s";
  if (ms < 60000) {
    const s = (ms / 1000).toFixed(1);
    return (s.endsWith(".0") ? s.slice(0, -2) : s) + "s";
  }
  const m = Math.floor(ms / 60000);
  const s = Math.round((ms % 60000) / 1000);
  return m + "m " + s + "s";
}

function calcTps(m) {
  if (!settings.showTps) return null;
  if (typeof m.duration_ms !== "number" || m.duration_ms <= 0) return null;
  if (typeof m.completion_tokens !== "number") return null;
  const v = m.completion_tokens / (m.duration_ms / 1000);
  return isFinite(v) && v > 0 ? v : null;
}

function fmtTps(v) {
  if (v == null) return null;
  return (v >= 10 ? v.toFixed(1) : v.toFixed(2)) + " tok/s";
}

function footerMetaText(modelId, cost, tps) {
  const parts = [];
  const name = modelName(modelId);
  if (name) parts.push(name);
  if (cost) parts.push(cost);
  if (tps) parts.push(tps);
  return parts.join(" · ");
}

async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try {
    document.execCommand("copy");
  } catch {
    /* best effort */
  }
  ta.remove();
}

/* ============================== API ============================== */

async function api(path, options) {
  const res = await fetch(path, options);
  if (res.status === 401) {
    handleUnauthorized();
    throw new AuthError("unauthorized");
  }
  if (res.status === 204) return null;
  if (!res.ok) {
    let message = "Request failed (" + res.status + ")";
    try {
      const j = await res.json();
      if (j && j.error) message = j.error;
    } catch {
      /* keep default message */
    }
    throw new Error(message);
  }
  return res.json();
}

function handleUnauthorized() {
  if (state.abort) state.abort.abort();
  state.current = null;
  state.currentMessages = [];
  state.conversations = [];
  state.projects = [];
  appEl.hidden = true;
  showLogin();
}

/* ============================== Auth ============================== */

function showLogin(message) {
  appEl.hidden = true;
  loginView.hidden = false;
  if (message) {
    loginError.textContent = message;
    loginError.hidden = false;
  } else {
    loginError.hidden = true;
  }
  setTimeout(() => loginToken.focus(), 0);
}

function showLoginError(message) {
  loginError.textContent = message;
  loginError.hidden = false;
}

async function checkAuth() {
  try {
    const res = await fetch("/api/me");
    if (res.status === 204) {
      enterApp();
    } else if (res.status === 401) {
      showLogin();
    } else {
      showLogin("Unexpected server response (" + res.status + ").");
    }
  } catch {
    showLogin("Cannot reach the server. Check your connection and reload.");
  }
}

/* ============================== App bootstrap ============================== */

async function enterApp() {
  loginView.hidden = true;
  loginError.hidden = true;
  appEl.hidden = false;
  state.current = null;
  state.currentMessages = [];
  state.activeProjectId = null;
  try {
    await loadModels();
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message || "Failed to load models");
    return;
  }
  try {
    await loadProjects();
    await loadConversations();
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message || "Failed to load conversations");
    return;
  }
  updateNewChatLabel();
  if (state.conversations.length > 0) {
    openConversation(state.conversations[0].id);
  } else {
    showWelcome();
  }
}

async function loadModels() {
  state.models = await api("/api/models");
  if (!Array.isArray(state.models) || state.models.length === 0) {
    throw new Error("No models available");
  }
  renderModelSelect();
  const prefs = loadPrefs();
  modelSelect.value = prefs.model && modelById(prefs.model) ? prefs.model : state.models[0].id;
  effortSelect.value = prefs.effort || "medium";
  updateEffortVisibility();
}

async function loadProjects() {
  try {
    const projects = await api("/api/projects");
    state.projects = Array.isArray(projects) ? projects : [];
  } catch (err) {
    if (err instanceof AuthError) throw err;
    // Degrade gracefully: conversations simply all render as unfiled.
    state.projects = [];
  }
}

/* ============================== Preferences (for next new chat) ============================== */

function loadPrefs() {
  try {
    return JSON.parse(localStorage.getItem("myogchat.prefs")) || {};
  } catch {
    return {};
  }
}

function savePrefs() {
  try {
    localStorage.setItem(
      "myogchat.prefs",
      JSON.stringify({ model: modelSelect.value, effort: effortSelect.value })
    );
  } catch {
    /* storage unavailable */
  }
}

/* ============================== Modals ============================== */

const openModals = [];

function openModal(node) {
  if (openModals.includes(node)) return;
  node.hidden = false;
  openModals.push(node);
  document.body.classList.add("modal-open");
}

function closeModal(node) {
  if (node.hidden) return;
  node.hidden = true;
  const i = openModals.indexOf(node);
  if (i !== -1) openModals.splice(i, 1);
  if (openModals.length === 0) document.body.classList.remove("modal-open");
}

function closeTopModal() {
  const node = openModals[openModals.length - 1];
  if (node) closeModal(node);
}

function openSettings() {
  applySettingsToUI();
  openModal(settingsModal);
  setTimeout(() => {
    const first = settingsModal.querySelector("input:checked") || settingsClose;
    first.focus();
  }, 0);
}

function applySettingsToUI() {
  settingsModal.querySelectorAll('input[name="theme"]').forEach((r) => {
    r.checked = r.value === settings.theme;
  });
  settingsModal.querySelectorAll('input[name="thinking"]').forEach((r) => {
    r.checked = r.value === settings.thinking;
  });
  settingShowTps.checked = settings.showTps;
  settingEnterToSend.checked = settings.enterToSend;
}

/* ============================== Popup menus ============================== */

let activeMenu = null;

function closeMenu() {
  if (!activeMenu) return;
  activeMenu.remove();
  activeMenu = null;
  document.removeEventListener("click", onDocClickForMenu, true);
  document.removeEventListener("contextmenu", onDocClickForMenu, true);
  window.removeEventListener("resize", closeMenu);
  window.removeEventListener("scroll", closeMenu, true);
}

function onDocClickForMenu(e) {
  if (activeMenu && !activeMenu.contains(e.target)) closeMenu();
}

function showMenu(anchor, items) {
  closeMenu();
  const menu = el("div", "menu");
  menu.setAttribute("role", "menu");
  for (const item of items) {
    if (item.separator) {
      menu.appendChild(el("div", "menu-sep"));
      continue;
    }
    const btn = el("button", "menu-item" + (item.danger ? " danger" : ""));
    btn.type = "button";
    btn.setAttribute("role", "menuitem");
    btn.textContent = item.label;
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      closeMenu();
      item.onClick();
    });
    menu.appendChild(btn);
  }
  document.body.appendChild(menu);

  const r = anchor.getBoundingClientRect();
  const mw = menu.offsetWidth;
  const mh = menu.offsetHeight;
  let left = r.left;
  let top = r.bottom + 4;
  if (left + mw > window.innerWidth - 8) left = Math.max(8, window.innerWidth - mw - 8);
  if (top + mh > window.innerHeight - 8) top = Math.max(8, r.top - mh - 4);
  menu.style.left = left + "px";
  menu.style.top = top + "px";
  activeMenu = menu;

  setTimeout(() => {
    document.addEventListener("click", onDocClickForMenu, true);
    document.addEventListener("contextmenu", onDocClickForMenu, true);
    window.addEventListener("resize", closeMenu);
    window.addEventListener("scroll", closeMenu, true);
  }, 0);
}

/* ============================== Sidebar ============================== */

function openSidebar() {
  sidebar.classList.add("open");
  backdrop.hidden = false;
  menuBtn.setAttribute("aria-expanded", "true");
}

function closeSidebar() {
  sidebar.classList.remove("open");
  backdrop.hidden = true;
  menuBtn.setAttribute("aria-expanded", "false");
}

async function loadConversations() {
  convoLoading.hidden = false;
  try {
    state.conversations = await api("/api/conversations");
    if (!Array.isArray(state.conversations)) state.conversations = [];
    renderConvoList();
  } finally {
    convoLoading.hidden = true;
  }
}

function updateNewChatLabel() {
  const project = state.activeProjectId ? projectById(state.activeProjectId) : null;
  newChatLabel.textContent = project ? "New chat in " + project.name : "New chat";
  newChatBtn.title = project ? "New chat in " + project.name : "New chat";
}

function buildConvoItem(conv) {
  const title = conv.title || "Untitled";
  const li = el("li", "convo-item");
  if (state.current && state.current.id === conv.id) li.classList.add("active");
  li.dataset.id = conv.id;

  const link = el("button", "convo-link");
  link.type = "button";
  link.textContent = title;
  link.title = title;

  const menuBtn2 = el("button", "icon-btn convo-menu");
  menuBtn2.type = "button";
  menuBtn2.innerHTML = ICONS.more;
  menuBtn2.setAttribute("aria-label", "Options for " + title);

  li.append(link, menuBtn2);
  return li;
}

function buildProjectItem(project, chats) {
  const expanded = state.expandedProjects.has(project.id);
  const li = el("li", "project-item");
  if (expanded) li.classList.add("expanded");
  if (state.activeProjectId === project.id) li.classList.add("active");
  li.dataset.projectId = project.id;

  const row = el("div", "project-row");
  const link = el("button", "project-link");
  link.type = "button";
  link.setAttribute("aria-expanded", expanded ? "true" : "false");

  const caret = el("span", "project-caret");
  caret.textContent = expanded ? "▾" : "▸";

  const name = el("span", "project-name");
  name.textContent = project.name;

  const count = el("span", "project-count");
  if (chats.length) count.textContent = String(chats.length);
  else count.hidden = true;

  link.append(caret, name, count);

  const menuBtn2 = el("button", "icon-btn project-menu");
  menuBtn2.type = "button";
  menuBtn2.innerHTML = ICONS.more;
  menuBtn2.setAttribute("aria-label", "Options for project " + project.name);

  row.append(link, menuBtn2);
  li.appendChild(row);

  if (expanded) {
    const ul = el("ul", "convo-list project-chats");
    ul.setAttribute("aria-label", project.name + " chats");
    for (const conv of chats) ul.appendChild(buildConvoItem(conv));
    li.appendChild(ul);
  }
  return li;
}

function renderConvoList() {
  projectsList.innerHTML = "";
  convoList.innerHTML = "";

  const byProject = new Map();
  const unfiled = [];
  const knownProjects = new Set(state.projects.map((p) => p.id));
  for (const conv of state.conversations) {
    if (conv.project_id && knownProjects.has(conv.project_id)) {
      if (!byProject.has(conv.project_id)) byProject.set(conv.project_id, []);
      byProject.get(conv.project_id).push(conv);
    } else {
      // No project, or a project we don't know about (e.g. projects failed to
      // load) — keep it visible under "Chats" rather than dropping it.
      unfiled.push(conv);
    }
  }

  projectsEmpty.hidden = state.projects.length !== 0;
  for (const project of state.projects) {
    projectsList.appendChild(buildProjectItem(project, byProject.get(project.id) || []));
  }

  convoEmpty.hidden = unfiled.length !== 0;
  for (const conv of unfiled) convoList.appendChild(buildConvoItem(conv));
}

function toggleProject(id) {
  const project = projectById(id);
  if (!project) return;
  if (state.expandedProjects.has(id)) {
    state.expandedProjects.delete(id);
    if (state.activeProjectId === id) state.activeProjectId = null;
  } else {
    state.expandedProjects.add(id);
    state.activeProjectId = id;
    if (project.default_model && modelById(project.default_model)) {
      modelSelect.value = project.default_model;
      updateEffortVisibility();
    }
  }
  updateNewChatLabel();
  renderConvoList();
}

/* ============================== Conversation row menu ============================== */

function openConvoMenu(id, anchor) {
  const conv = state.conversations.find((c) => c.id === id);
  if (!conv) return;
  showMenu(anchor, [
    { label: "Rename", onClick: () => renameConversation(id) },
    { label: "Move to project…", onClick: () => openMoveMenu(id, anchor) },
    { separator: true },
    { label: "Delete", danger: true, onClick: () => deleteConversation(id) },
  ]);
}

function openMoveMenu(id, anchor) {
  const conv = state.conversations.find((c) => c.id === id);
  if (!conv) return;
  const items = [{ label: "Remove from project", onClick: () => moveConversationToProject(id, "") }];
  if (state.projects.length > 0) items.push({ separator: true });
  for (const p of state.projects) {
    items.push({ label: p.name, onClick: () => moveConversationToProject(id, p.id) });
  }
  showMenu(anchor, items);
}

async function moveConversationToProject(id, projectId) {
  try {
    const updated = await api("/api/conversations/" + enc(id), {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ project_id: projectId || "" }),
    });
    applyConvUpdate(updated);
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
  }
}

async function renameConversation(id) {
  const conv = state.conversations.find((c) => c.id === id);
  if (!conv) return;
  const title = window.prompt("Rename conversation", conv.title || "");
  if (title === null) return;
  try {
    const updated = await api("/api/conversations/" + enc(id), {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ title: title.trim().slice(0, 200) }),
    });
    applyConvUpdate(updated);
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
  }
}

async function deleteConversation(id) {
  const conv = state.conversations.find((c) => c.id === id);
  if (!conv) return;
  if (!window.confirm('Delete "' + (conv.title || "Untitled") + '"? This cannot be undone.')) return;
  try {
    await api("/api/conversations/" + enc(id), { method: "DELETE" });
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
    return;
  }
  state.conversations = state.conversations.filter((c) => c.id !== id);
  if (state.current && state.current.id === id) {
    state.current = null;
    state.currentMessages = [];
    showWelcome();
  }
  renderConvoList();
  toast("Conversation deleted");
}

/* ============================== Projects ============================== */

function openProjectMenu(id, anchor) {
  const project = projectById(id);
  if (!project) return;
  showMenu(anchor, [
    { label: "Rename", onClick: () => renameProject(id) },
    { label: "Edit instructions", onClick: () => openProjectEditor(project) },
    { label: "Set default model", onClick: () => openProjectModelMenu(id, anchor) },
    { separator: true },
    { label: "Delete", danger: true, onClick: () => deleteProject(id) },
  ]);
}

function openProjectModelMenu(id, anchor) {
  const items = [{ label: "(none)", onClick: () => patchProject(id, { default_model: "" }) }];
  for (const m of state.models) {
    items.push({ label: modelOptionLabel(m), onClick: () => patchProject(id, { default_model: m.id }) });
  }
  showMenu(anchor, items);
}

function replaceProject(updated) {
  if (!updated) return;
  const idx = state.projects.findIndex((p) => p.id === updated.id);
  if (idx !== -1) state.projects[idx] = updated;
  renderConvoList();
  updateNewChatLabel();
}

async function patchProject(id, body) {
  try {
    const updated = await api("/api/projects/" + enc(id), {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    replaceProject(updated);
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
  }
}

async function renameProject(id) {
  const project = projectById(id);
  if (!project) return;
  const name = window.prompt("Rename project", project.name || "");
  if (name === null || !name.trim()) return;
  await patchProject(id, { name: name.trim().slice(0, 120) });
}

async function deleteProject(id) {
  const project = projectById(id);
  if (!project) return;
  if (!window.confirm('Delete project "' + project.name + '"? Its chats will be moved out of the project.')) {
    return;
  }
  try {
    await api("/api/projects/" + enc(id), { method: "DELETE" });
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
    return;
  }
  state.projects = state.projects.filter((p) => p.id !== id);
  state.expandedProjects.delete(id);
  if (state.activeProjectId === id) state.activeProjectId = null;
  // Chats were unfiled server-side; re-pull the list so ordering/sections match.
  try {
    await loadConversations();
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
  }
  if (state.current && state.current.project_id === id) {
    state.current.project_id = null;
    renderMessages();
  }
  updateNewChatLabel();
  renderConvoList();
  toast("Project deleted");
}

let editingProjectId = null;

function renderProjectModelOptions(selected) {
  projectModel.innerHTML = "";
  const none = document.createElement("option");
  none.value = "";
  none.textContent = "(none)";
  projectModel.appendChild(none);
  for (const m of state.models) {
    const opt = document.createElement("option");
    opt.value = m.id;
    opt.textContent = modelOptionLabel(m);
    projectModel.appendChild(opt);
  }
  projectModel.value = selected && modelById(selected) ? selected : "";
}

function openProjectEditor(project) {
  editingProjectId = project ? project.id : null;
  projectModalTitle.textContent = project ? "Edit project" : "New project";
  projectName.value = project ? project.name : "";
  projectInstructions.value = project ? project.instructions || "" : "";
  renderProjectModelOptions(project ? project.default_model : "");
  openModal(projectModal);
  setTimeout(() => projectName.focus(), 0);
}

async function saveProject() {
  const name = projectName.value.trim();
  if (!name) {
    projectName.focus();
    return;
  }
  const body = {
    name: name.slice(0, 120),
    instructions: projectInstructions.value.slice(0, 8000),
  };
  if (projectModel.value) {
    body.default_model = projectModel.value;
  } else if (editingProjectId) {
    body.default_model = ""; // clear on edit
  }
  const editing = editingProjectId;
  try {
    if (editing) {
      const updated = await api("/api/projects/" + enc(editing), {
        method: "PATCH",
        headers: JSON_HEADERS,
        body: JSON.stringify(body),
      });
      replaceProject(updated);
    } else {
      const created = await api("/api/projects", {
        method: "POST",
        headers: JSON_HEADERS,
        body: JSON.stringify(body),
      });
      if (created && created.id) state.projects.push(created);
      renderConvoList();
    }
    closeModal(projectModal);
    // Re-pull so sort_order / conversation_count / defaults match the server.
    await loadProjects();
    renderConvoList();
    updateNewChatLabel();
    toast(editing ? "Project updated" : "Project created");
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
  }
}

/* ============================== Model / effort pickers ============================== */

function renderModelSelect() {
  modelSelect.innerHTML = "";
  const groups = [
    ["Free", (m) => m.output_per_m === 0],
    ["Budget", (m) => m.output_per_m > 0 && m.output_per_m <= 0.6],
    ["Mid", (m) => m.output_per_m > 0.6 && m.output_per_m <= 2],
    ["Premium", (m) => m.output_per_m > 2],
  ];
  for (const [label, pred] of groups) {
    const models = state.models.filter(pred);
    if (models.length === 0) continue;
    const og = document.createElement("optgroup");
    og.label = label;
    for (const m of models) {
      const opt = document.createElement("option");
      opt.value = m.id;
      opt.textContent = modelOptionLabel(m);
      og.appendChild(opt);
    }
    modelSelect.appendChild(og);
  }
}

function updateEffortVisibility() {
  effortSelect.hidden = !isResponsesModel(modelSelect.value);
  const m = modelById(modelSelect.value);
  modelThinkingBadge.hidden = !(m && m.thinking);
}

async function patchConversation(body) {
  return api("/api/conversations/" + enc(state.current.id), {
    method: "PATCH",
    headers: JSON_HEADERS,
    body: JSON.stringify(body),
  });
}

function applyConvUpdate(updated) {
  if (!updated) return;
  if (state.current && state.current.id === updated.id) {
    Object.assign(state.current, updated);
    if ("project_id" in updated) {
      state.activeProjectId = updated.project_id || null;
      if (state.activeProjectId) state.expandedProjects.add(state.activeProjectId);
      updateNewChatLabel();
    }
  }
  const idx = state.conversations.findIndex((c) => c.id === updated.id);
  if (idx !== -1) state.conversations[idx] = Object.assign({}, state.conversations[idx], updated);
  else state.conversations.unshift(updated);
  renderConvoList();
  updateEffortVisibility();
  renderTotalCost();
}

/* ============================== Conversation view ============================== */

function showWelcome() {
  clearMessages();
  welcomeEl.hidden = false;
  totalCostEl.hidden = true;
  const prefs = loadPrefs();
  if (prefs.model && modelById(prefs.model)) modelSelect.value = prefs.model;
  effortSelect.value = prefs.effort || "medium";
  updateEffortVisibility();
  updateNewChatLabel();
  renderConvoList();
}

async function openConversation(id) {
  closeSidebar();
  clearMessages();
  welcomeEl.hidden = true;
  msgsLoading.hidden = false;
  try {
    const conv = await api("/api/conversations/" + enc(id));
    setCurrentConversation(conv);
  } catch (err) {
    if (!(err instanceof AuthError)) {
      toast(err.message || "Failed to load conversation");
      showWelcome();
    }
  } finally {
    msgsLoading.hidden = true;
  }
}

function setCurrentConversation(conv) {
  state.current = conv;
  state.currentMessages = Array.isArray(conv.messages) ? conv.messages : [];
  state.activeProjectId = conv.project_id || null;
  if (state.activeProjectId) state.expandedProjects.add(state.activeProjectId);
  if (modelById(conv.model)) {
    modelSelect.value = conv.model;
  } else if (state.models.length > 0) {
    modelSelect.value = state.models[0].id;
  }
  updateEffortVisibility();
  effortSelect.value = conv.effort || "medium";
  renderMessages();
  renderConvoList();
  updateNewChatLabel();
  renderTotalCost();
  input.focus({ preventScroll: true });
}

/* Build the POST /api/conversations body for a new chat. When a project is
   active, send its id and (per contract §8.3) omit `model` so the backend
   applies the project's default_model; only send a model if the project has no
   usable default. Otherwise send the picker's model explicitly. */
function buildNewChatBody() {
  const body = {};
  const project = state.activeProjectId ? projectById(state.activeProjectId) : null;
  const model = modelSelect.value;
  if (project) {
    body.project_id = project.id;
    if (!(project.default_model && modelById(project.default_model))) {
      body.model = model;
    }
  } else {
    body.model = model;
  }
  const effective = body.model || (project ? project.default_model : model);
  if (isResponsesModel(effective)) body.effort = effortSelect.value || "medium";
  return body;
}

async function createConversation() {
  const conv = await api("/api/conversations", {
    method: "POST",
    headers: JSON_HEADERS,
    body: JSON.stringify(buildNewChatBody()),
  });
  state.conversations.unshift(conv);
  if (conv.project_id) state.expandedProjects.add(conv.project_id);
  setCurrentConversation(Object.assign({ messages: [] }, conv));
  return conv;
}

async function ensureConversation() {
  if (state.current) return state.current;
  return createConversation();
}

/* Re-pull the open conversation so server-computed fields (reasoning_tokens,
   duration_ms, cost, title) are reflected. */
async function refreshCurrentConversation() {
  if (!state.current) return;
  const id = state.current.id;
  try {
    const conv = await api("/api/conversations/" + enc(id));
    if (!state.current || state.current.id !== id) return;
    state.current = conv;
    state.currentMessages = Array.isArray(conv.messages) ? conv.messages : [];
    renderMessages();
    renderTotalCost();
  } catch (err) {
    if (!(err instanceof AuthError)) {
      /* keep the optimistic rendering */
    }
  }
  loadConversations().catch((err) => {
    if (!(err instanceof AuthError)) toast(err.message);
  });
}

/* ============================== Message rendering ============================== */

function clearMessages() {
  messagesEl.querySelectorAll(".msg").forEach((n) => n.remove());
}

function renderMessages() {
  clearMessages();
  welcomeEl.hidden = true;
  let lastAssistant = -1;
  state.currentMessages.forEach((m, i) => {
    if (m.role === "assistant") lastAssistant = i;
  });
  state.currentMessages.forEach((m, i) => {
    messagesEl.appendChild(
      m.role === "user" ? buildUserMessage(m) : buildAssistantMessage(m, i === lastAssistant)
    );
  });
  messagesEl.scrollTop = messagesEl.scrollHeight;
}

function buildUserMessage(m) {
  const wrap = el("div", "msg msg-user");
  const bubble = el("div", "msg-bubble");
  bubble.textContent = m.content;
  wrap.appendChild(bubble);
  return wrap;
}

function thinkingSummary(opts) {
  if (typeof opts.durationMs === "number" && typeof opts.reasoningTokens === "number") {
    return "Thought for " + fmtDuration(opts.durationMs) + " · " + opts.reasoningTokens + " tokens";
  }
  return "Thought";
}

function buildThinking(text, opts) {
  opts = opts || {};
  const details = el("details", "thinking");
  if (settings.thinking === "expanded") details.open = true;
  if (settings.thinking === "hidden") details.classList.add("thinking-hidden");
  const summary = document.createElement("summary");
  summary.textContent = opts.summary || thinkingSummary(opts);
  const body = el("div", "thinking-body");
  body.textContent = text;
  details.append(summary, body);
  return details;
}

function actionButton(label, onClick) {
  const btn = el("button", "msg-action");
  btn.type = "button";
  btn.textContent = label;
  btn.addEventListener("click", (e) => {
    e.preventDefault();
    onClick(btn);
  });
  return btn;
}

async function copyMessage(m, btn) {
  const raw = typeof m.content === "string" ? m.content : "";
  await copyText(raw);
  btn.textContent = "Copied!";
  setTimeout(() => {
    btn.textContent = "Copy";
  }, 1500);
}

function buildAssistantFooter(m, isLast) {
  const footer = el("div", "msg-footer");
  const meta = el("span", "msg-meta");
  meta.textContent = footerMetaText(
    m.model,
    fmtMsgCost(m.cost_micros, m.model),
    fmtTps(calcTps(m))
  );
  footer.appendChild(meta);

  const actions = el("span", "msg-actions");
  actions.appendChild(actionButton("Copy", (btn) => copyMessage(m, btn)));
  if (isLast) actions.appendChild(actionButton("Retry", () => regenerate()));
  footer.appendChild(actions);
  return footer;
}

function buildAssistantMessage(m, isLast) {
  const wrap = el("div", "msg msg-assistant");
  const bubble = el("div", "msg-bubble");
  if (m.reasoning) {
    bubble.appendChild(
      buildThinking(m.reasoning, {
        durationMs: typeof m.duration_ms === "number" ? m.duration_ms : null,
        reasoningTokens: typeof m.reasoning_tokens === "number" ? m.reasoning_tokens : null,
      })
    );
  }
  const content = el("div", "msg-content markdown");
  content.innerHTML = renderMarkdown(m.content);
  enhanceMarkdown(content);
  bubble.appendChild(content);
  bubble.appendChild(buildAssistantFooter(m, isLast));
  wrap.appendChild(bubble);
  return wrap;
}

function renderTotalCost() {
  const hasCost = state.currentMessages.some((m) => typeof m.cost_micros === "number");
  if (!state.current || !hasCost) {
    totalCostEl.hidden = true;
    return;
  }
  const total = state.currentMessages.reduce(
    (sum, m) => sum + (typeof m.cost_micros === "number" ? m.cost_micros : 0),
    0
  );
  const free = total === 0 && state.current && isFreeModel(state.current.model);
  totalCostEl.textContent = "Total: " + (free ? "Free" : fmtMicros(total));
  totalCostEl.hidden = false;
}

/* ============================== Markdown ============================== */

function configureMarked() {
  marked.use({
    gfm: true,
    breaks: true,
    renderer: {
      /* Neutralize raw HTML in model output (no sanitizer vendored). */
      html(token) {
        return escapeHtml(token.text);
      },
      link(token) {
        const text = this.parser.parseInline(token.tokens);
        const title = token.title ? ' title="' + escapeHtml(token.title) + '"' : "";
        return (
          '<a href="' +
          escapeHtml(sanitizeUrl(token.href)) +
          '"' +
          title +
          ' target="_blank" rel="noopener noreferrer">' +
          text +
          "</a>"
        );
      },
    },
  });
}

function sanitizeUrl(url) {
  const u = String(url || "").trim();
  if (/^(https?:|mailto:|#|\/)/i.test(u)) return u;
  return "#";
}

function renderMarkdown(text) {
  try {
    return marked.parse(text || "");
  } catch {
    return "<p>" + escapeHtml(text) + "</p>";
  }
}

function enhanceMarkdown(container) {
  container.querySelectorAll("pre code").forEach((code) => {
    if (code.dataset.highlighted) return;
    try {
      hljs.highlightElement(code);
    } catch {
      /* highlighting is best-effort */
    }
  });
  container.querySelectorAll("pre").forEach((pre) => {
    if (pre.querySelector(".copy-btn")) return;
    const btn = el("button", "copy-btn");
    btn.type = "button";
    btn.textContent = "Copy";
    btn.setAttribute("aria-label", "Copy code to clipboard");
    btn.addEventListener("click", async () => {
      const code = pre.querySelector("code");
      await copyText(code ? code.innerText : pre.innerText);
      btn.textContent = "Copied!";
      setTimeout(() => {
        btn.textContent = "Copy";
      }, 1500);
    });
    pre.appendChild(btn);
  });
}

/* ============================== Scrolling ============================== */

function isNearBottom() {
  return messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight < 120;
}

function scrollToBottom() {
  messagesEl.scrollTop = messagesEl.scrollHeight;
}

/* ============================== SSE parsing ============================== */

/* Manual SSE frame parser: lines "event: X" / "data: Y", blank line dispatches,
   ": comment" lines (keepalives) are ignored. */
function createSSEParser(onEvent) {
  let buf = "";
  let eventType = "message";
  let dataLines = [];

  function dispatch() {
    if (dataLines.length > 0) {
      onEvent(eventType, dataLines.join("\n"));
    }
    eventType = "message";
    dataLines = [];
  }

  function handleLine(line) {
    if (line === "") {
      dispatch();
      return;
    }
    if (line.startsWith(":")) return; // comment / keepalive
    const colon = line.indexOf(":");
    let field = line;
    let value = "";
    if (colon !== -1) {
      field = line.slice(0, colon);
      value = line.slice(colon + 1);
      if (value.startsWith(" ")) value = value.slice(1);
    }
    if (field === "event") eventType = value;
    else if (field === "data") dataLines.push(value);
  }

  return {
    feed(chunk) {
      buf += chunk;
      let idx;
      while ((idx = buf.indexOf("\n")) !== -1) {
        let line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (line.endsWith("\r")) line = line.slice(0, -1);
        handleLine(line);
      }
    },
    end() {
      if (buf.length > 0) {
        let line = buf;
        if (line.endsWith("\r")) line = line.slice(0, -1);
        handleLine(line);
        buf = "";
      }
      dispatch();
    },
  };
}

/* ============================== Streaming chat ============================== */

function setStreaming(on) {
  state.streaming = on;
  input.disabled = on;
  sendBtn.hidden = on;
  stopBtn.hidden = !on;
  updateSendBtn();
  if (!on) input.focus({ preventScroll: true });
}

function updateSendBtn() {
  sendBtn.disabled = state.streaming || input.value.trim() === "";
}

function newStreamUI() {
  return {
    bubble: null,
    contentEl: null,
    thinkingEl: null,
    thinkingBody: null,
    footerEl: null,
    footerMeta: null,
    textBuf: "",
    reasoningBuf: "",
    usage: null,
    model: null,
    messageId: null,
    done: false,
    failed: false,
    firstTokenAt: 0,
    durationMs: null,
  };
}

function ensureStreamBubble(ui) {
  if (ui.bubble) return;
  const wrap = el("div", "msg msg-assistant");
  ui.bubble = el("div", "msg-bubble");
  wrap.appendChild(ui.bubble);
  messagesEl.appendChild(wrap);
}

function renderStreamThinking(ui) {
  if (!ui.thinkingEl) {
    ui.thinkingEl = buildThinking("", { summary: "Thinking…" });
    ui.thinkingBody = ui.thinkingEl.querySelector(".thinking-body");
    ui.bubble.insertBefore(ui.thinkingEl, ui.bubble.firstChild);
  }
  ui.thinkingBody.textContent = ui.reasoningBuf;
}

/* chars/4 tokens over elapsed seconds — approximate, marked with "~". */
function liveTps(ui) {
  if (!settings.showTps || !ui.firstTokenAt) return null;
  const elapsed = (performance.now() - ui.firstTokenAt) / 1000;
  if (elapsed <= 0) return null;
  const tokens = (ui.textBuf.length + ui.reasoningBuf.length) / 4;
  const v = tokens / elapsed;
  if (!isFinite(v) || v <= 0) return null;
  return (v >= 10 ? v.toFixed(1) : v.toFixed(2)) + " tok/s";
}

function renderStreamFooter(ui) {
  ensureStreamBubble(ui);
  if (!ui.footerEl) {
    ui.footerEl = el("div", "msg-footer");
    ui.footerMeta = el("span", "msg-meta");
    ui.footerEl.appendChild(ui.footerMeta);
    ui.bubble.appendChild(ui.footerEl);
  }
  const cost = ui.usage ? fmtMsgCost(ui.usage.cost_micros, ui.model) : null;
  const live = liveTps(ui);
  ui.footerMeta.textContent = footerMetaText(ui.model, cost, live ? "~" + live : null);
}

function showStreamError(ui, message) {
  ensureStreamBubble(ui);
  const err = el("div", "msg-error");
  err.textContent = "⚠ " + message;
  ui.bubble.appendChild(err);
  scrollToBottom();
}

function markStopped(ui) {
  if (!ui.bubble) return;
  const note = el("div", "msg-footer");
  note.textContent = "Stopped";
  ui.bubble.appendChild(note);
}

function handleStreamEvent(type, data, ui) {
  let payload = {};
  if (data) {
    try {
      payload = JSON.parse(data);
    } catch {
      payload = {};
    }
  }
  switch (type) {
    case "meta":
      ui.messageId = payload.message_id || null;
      ui.model = payload.model || null;
      ensureStreamBubble(ui);
      break;
    case "reasoning":
      ensureStreamBubble(ui);
      if (!ui.firstTokenAt) ui.firstTokenAt = performance.now();
      ui.reasoningBuf += payload.delta || "";
      renderStreamThinking(ui);
      renderStreamFooter(ui);
      break;
    case "text": {
      ensureStreamBubble(ui);
      if (!ui.firstTokenAt) ui.firstTokenAt = performance.now();
      const stick = isNearBottom();
      ui.textBuf += payload.delta || "";
      if (!ui.contentEl) {
        ui.contentEl = el("div", "msg-content markdown");
        if (ui.footerEl) ui.bubble.insertBefore(ui.contentEl, ui.footerEl);
        else ui.bubble.appendChild(ui.contentEl);
      }
      ui.contentEl.innerHTML = renderMarkdown(ui.textBuf);
      enhanceMarkdown(ui.contentEl);
      renderStreamFooter(ui);
      if (stick) scrollToBottom();
      break;
    }
    case "usage":
      ui.usage = payload;
      renderStreamFooter(ui);
      break;
    case "done":
      ui.done = true;
      ui.durationMs = ui.firstTokenAt ? Math.round(performance.now() - ui.firstTokenAt) : null;
      if (payload.title && state.current) {
        state.current.title = payload.title;
        const conv = state.conversations.find((c) => c.id === state.current.id);
        if (conv) conv.title = payload.title;
      }
      finalizeStream(ui);
      break;
    case "error":
      ui.failed = true;
      showStreamError(ui, payload.message || "Something went wrong");
      break;
  }
}

function finalizeStream(ui) {
  // Ignore a stream that finished after the user navigated elsewhere.
  if (!state.current || (ui.convId && state.current.id !== ui.convId)) return;
  const usage = ui.usage || {};
  state.currentMessages.push({
    role: "assistant",
    content: ui.textBuf,
    reasoning: ui.reasoningBuf || null,
    model: ui.model,
    cost_micros: typeof usage.cost_micros === "number" ? usage.cost_micros : null,
    prompt_tokens: typeof usage.prompt_tokens === "number" ? usage.prompt_tokens : null,
    completion_tokens: typeof usage.completion_tokens === "number" ? usage.completion_tokens : null,
    cached_tokens: typeof usage.cached_tokens === "number" ? usage.cached_tokens : null,
    reasoning_tokens:
      typeof usage.reasoning_tokens === "number" ? usage.reasoning_tokens : null,
    duration_ms: ui.durationMs,
  });
  // Rebuild so Copy/Retry + footers appear, then enrich with server fields.
  renderMessages();
  renderTotalCost();
  refreshCurrentConversation();
}

function rollbackOptimisticUserMessage(text) {
  state.currentMessages.pop();
  const nodes = messagesEl.querySelectorAll(".msg");
  if (nodes.length > 0) nodes[nodes.length - 1].remove();
  input.value = text;
  autosizeInput();
  updateSendBtn();
}

/* Shared SSE POST path. `body` is JSON-encoded when present. `onFatal` runs
   when the request fails before/without streaming (rollback hook). */
async function streamAssistant(opts) {
  setStreaming(true);
  const controller = new AbortController();
  state.abort = controller;
  const ui = newStreamUI();
  ui.convId = opts.convId || null;

  let res;
  try {
    res = await fetch(opts.path, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
      body: opts.body ? JSON.stringify(opts.body) : undefined,
      signal: controller.signal,
    });
  } catch (err) {
    setStreaming(false);
    state.abort = null;
    if (err.name !== "AbortError") {
      toast("Network error — message not sent");
      if (opts.onFatal) opts.onFatal();
    }
    return;
  }

  if (res.status === 401) {
    setStreaming(false);
    state.abort = null;
    handleUnauthorized();
    return;
  }
  if (!res.ok || !res.body) {
    let message = "Request failed (" + res.status + ")";
    try {
      const j = await res.json();
      if (j && j.error) message = j.error;
    } catch {
      /* keep default */
    }
    setStreaming(false);
    state.abort = null;
    toast(message);
    if (opts.onFatal) opts.onFatal();
    return;
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  const parser = createSSEParser((type, data) => handleStreamEvent(type, data, ui));
  try {
    for (;;) {
      const chunk = await reader.read();
      if (chunk.done) break;
      parser.feed(decoder.decode(chunk.value, { stream: true }));
    }
    parser.feed(decoder.decode());
    parser.end();
  } catch (err) {
    if (err.name === "AbortError") {
      markStopped(ui);
    } else {
      showStreamError(ui, "Connection interrupted");
    }
  } finally {
    setStreaming(false);
    state.abort = null;
  }
}

async function sendMessage(conv, text) {
  const userMsg = { role: "user", content: text, cost_micros: null };
  state.currentMessages.push(userMsg);
  messagesEl.appendChild(buildUserMessage(userMsg));
  scrollToBottom();

  await streamAssistant({
    path: "/api/conversations/" + enc(conv.id) + "/messages",
    convId: conv.id,
    body: { content: text },
    onFatal: () => rollbackOptimisticUserMessage(text),
  });
}

async function regenerate() {
  if (!state.current || state.streaming) return;
  const msgs = state.currentMessages;
  let lastUser = -1;
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i].role === "user") {
      lastUser = i;
      break;
    }
  }
  if (lastUser === -1) {
    toast("Nothing to retry");
    return;
  }
  // Server deletes assistant messages after the last user message.
  state.currentMessages = msgs.slice(0, lastUser + 1);
  renderMessages();
  await streamAssistant({
    path: "/api/conversations/" + enc(state.current.id) + "/regenerate",
    convId: state.current.id,
    onFatal: () => refreshCurrentConversation(),
  });
}

/* ============================== Composer ============================== */

function autosizeInput() {
  input.style.height = "auto";
  input.style.height = Math.min(input.scrollHeight, 200) + "px";
}

/* ============================== Sidebar nav delegation ============================== */

function onNavClick(e) {
  const projectMenu = e.target.closest(".project-menu");
  if (projectMenu) {
    const item = projectMenu.closest(".project-item");
    if (item) openProjectMenu(item.dataset.projectId, projectMenu);
    return;
  }
  const projectLink = e.target.closest(".project-link");
  if (projectLink) {
    const item = projectLink.closest(".project-item");
    if (item) toggleProject(item.dataset.projectId);
    return;
  }
  const convoMenu = e.target.closest(".convo-menu");
  if (convoMenu) {
    const item = convoMenu.closest(".convo-item");
    if (item) openConvoMenu(item.dataset.id, convoMenu);
    return;
  }
  const convoLink = e.target.closest(".convo-link");
  if (convoLink) {
    const item = convoLink.closest(".convo-item");
    if (!item) return;
    const id = item.dataset.id;
    if (!state.current || state.current.id !== id) openConversation(id);
    else closeSidebar();
  }
}

/* ============================== Events ============================== */

function wireEvents() {
  loginForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    loginError.hidden = true;
    loginBtn.disabled = true;
    try {
      const res = await fetch("/api/login", {
        method: "POST",
        headers: JSON_HEADERS,
        body: JSON.stringify({ token: loginToken.value }),
      });
      if (res.status === 204) {
        loginToken.value = "";
        enterApp();
      } else if (res.status === 401) {
        showLoginError("Invalid token.");
      } else {
        showLoginError("Login failed (" + res.status + ").");
      }
    } catch {
      showLoginError("Cannot reach the server.");
    } finally {
      loginBtn.disabled = false;
    }
  });

  logoutBtn.addEventListener("click", async () => {
    try {
      await fetch("/api/logout", { method: "POST" });
    } catch {
      /* clear locally regardless */
    }
    handleUnauthorized();
  });

  menuBtn.addEventListener("click", () => {
    if (sidebar.classList.contains("open")) closeSidebar();
    else openSidebar();
  });
  backdrop.addEventListener("click", closeSidebar);

  collapseBtn.addEventListener("click", () => {
    settings.sidebarCollapsed = !settings.sidebarCollapsed;
    lsSet("myogchat.sidebarCollapsed", settings.sidebarCollapsed ? "1" : "0");
    applySidebarCollapsed();
  });

  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    if (openModals.length > 0) {
      closeTopModal();
      return;
    }
    if (activeMenu) {
      closeMenu();
      return;
    }
    if (sidebar.classList.contains("open")) closeSidebar();
  });

  /* Settings */
  settingsBtn.addEventListener("click", openSettings);
  settingsClose.addEventListener("click", () => closeModal(settingsModal));
  settingsModal.addEventListener("click", (e) => {
    if (e.target === settingsModal) closeModal(settingsModal);
  });
  settingsModal.querySelectorAll('input[name="theme"]').forEach((r) => {
    r.addEventListener("change", () => {
      if (!r.checked) return;
      settings.theme = r.value;
      lsSet("myogchat.theme", r.value);
      applyTheme();
    });
  });
  settingsModal.querySelectorAll('input[name="thinking"]').forEach((r) => {
    r.addEventListener("change", () => {
      if (!r.checked) return;
      settings.thinking = r.value;
      lsSet("myogchat.thinking", r.value);
      if (state.current && !state.streaming) renderMessages();
    });
  });
  settingShowTps.addEventListener("change", () => {
    settings.showTps = settingShowTps.checked;
    lsSet("myogchat.showTps", settings.showTps ? "1" : "0");
    if (state.current && !state.streaming) renderMessages();
  });
  settingEnterToSend.addEventListener("change", () => {
    settings.enterToSend = settingEnterToSend.checked;
    lsSet("myogchat.enterToSend", settings.enterToSend ? "1" : "0");
    applyEnterHint();
  });
  prefersLight.addEventListener("change", () => {
    if (settings.theme === "system") applyTheme();
  });

  /* Projects */
  newProjectBtn.addEventListener("click", () => openProjectEditor(null));
  projectClose.addEventListener("click", () => closeModal(projectModal));
  projectCancel.addEventListener("click", () => closeModal(projectModal));
  projectModal.addEventListener("click", (e) => {
    if (e.target === projectModal) closeModal(projectModal);
  });
  projectForm.addEventListener("submit", (e) => {
    e.preventDefault();
    saveProject();
  });

  newChatBtn.addEventListener("click", async () => {
    closeSidebar();
    // Reuse the open empty conversation when it belongs to the active project.
    if (
      state.current &&
      state.currentMessages.length === 0 &&
      (state.current.project_id || null) === state.activeProjectId
    ) {
      input.focus();
      return;
    }
    try {
      await createConversation();
    } catch (err) {
      if (!(err instanceof AuthError)) toast(err.message);
    }
  });

  projectsList.addEventListener("click", onNavClick);
  convoList.addEventListener("click", onNavClick);

  modelSelect.addEventListener("change", async () => {
    updateEffortVisibility();
    savePrefs();
    if (!state.current) return;
    const newModel = modelSelect.value;
    try {
      const updated = await patchConversation({ model: newModel });
      applyConvUpdate(updated);
    } catch (err) {
      if (!(err instanceof AuthError)) {
        toast(err.message);
        modelSelect.value = state.current.model;
        updateEffortVisibility();
      }
    }
  });

  effortSelect.addEventListener("change", async () => {
    savePrefs();
    if (!state.current) return;
    const newEffort = effortSelect.value;
    try {
      const updated = await patchConversation({ effort: newEffort });
      applyConvUpdate(updated);
    } catch (err) {
      if (!(err instanceof AuthError)) {
        toast(err.message);
        effortSelect.value = state.current.effort || "medium";
      }
    }
  });

  composer.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (state.streaming) return;
    const text = input.value.trim();
    if (!text) return;
    input.value = "";
    autosizeInput();
    updateSendBtn();
    try {
      const conv = await ensureConversation();
      await sendMessage(conv, text);
    } catch (err) {
      if (!(err instanceof AuthError)) toast(err.message);
    }
  });

  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      if (settings.enterToSend) {
        e.preventDefault();
        composer.requestSubmit();
      }
      // else: allow the default newline
    }
  });
  input.addEventListener("input", () => {
    autosizeInput();
    updateSendBtn();
  });

  stopBtn.addEventListener("click", () => {
    if (state.abort) state.abort.abort();
  });
}

/* ============================== Service worker ============================== */

function registerSW() {
  if ("serviceWorker" in navigator && window.isSecureContext) {
    window.addEventListener("load", () => {
      navigator.serviceWorker.register("sw.js").catch(() => {
        /* offline shell is a progressive enhancement */
      });
    });
  }
}

/* ============================== Init ============================== */

loadSettings();
applyTheme();
applySidebarCollapsed();
applyEnterHint();
applySettingsToUI();
configureMarked();
wireEvents();
registerSW();
updateSendBtn();
updateNewChatLabel();
checkAuth();
