/* MyOGChat — vanilla JS SPA (no build step). Depends on vendored marked + highlight.js. */
"use strict";

/* ============================== DOM refs ============================== */

const loginView = document.getElementById("login-view");
const loginForm = document.getElementById("login-form");
const loginIdentifier = document.getElementById("login-identifier");
const loginPassword = document.getElementById("login-password");
const loginError = document.getElementById("login-error");
const loginBtn = document.getElementById("login-btn");
const showRegisterBtn = document.getElementById("show-register-btn");

const setupForm = document.getElementById("setup-form");
const setupUsername = document.getElementById("setup-username");
const setupEmail = document.getElementById("setup-email");
const setupPassword = document.getElementById("setup-password");
const setupConfirm = document.getElementById("setup-confirm");
const setupError = document.getElementById("setup-error");
const setupBtn = document.getElementById("setup-btn");

const registerForm = document.getElementById("register-form");
const registerUsername = document.getElementById("register-username");
const registerEmail = document.getElementById("register-email");
const registerPassword = document.getElementById("register-password");
const registerConfirm = document.getElementById("register-confirm");
const registerPending = document.getElementById("register-pending");
const registerError = document.getElementById("register-error");
const registerBtn = document.getElementById("register-btn");
const showLoginBtn = document.getElementById("show-login-btn");

const appEl = document.getElementById("app");
const sidebar = document.getElementById("sidebar");
const backdrop = document.getElementById("backdrop");
const menuBtn = document.getElementById("menu-btn");
const collapseBtn = document.getElementById("collapse-btn");
const newChatBtn = document.getElementById("new-chat-btn");
const newChatLabel = document.getElementById("new-chat-label");
const logoutBtn = document.getElementById("logout-btn");
const userNameEl = document.getElementById("user-name");
const userAdminBadge = document.getElementById("user-admin-badge");
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
const settingsPendingBadge = document.getElementById("settings-pending-badge");

const messagesEl = document.getElementById("messages");
const welcomeEl = document.getElementById("welcome");
const msgsLoading = document.getElementById("msgs-loading");

const composer = document.getElementById("composer");
const input = document.getElementById("input");
const sendBtn = document.getElementById("send-btn");
const stopBtn = document.getElementById("stop-btn");
const attachBtn = document.getElementById("attach-btn");
const fileInput = document.getElementById("file-input");
const attachmentsBar = document.getElementById("attachments-bar");
/* Voice (features 3.5 / 3.6). */
const micBtn = document.getElementById("mic-btn");
const audioInput = document.getElementById("audio-input");

/* Image generation (feature 3.1) — self-contained composer affordance. */
const imageBtn = document.getElementById("image-btn");
const imageBar = document.getElementById("image-bar");
const imagePrompt = document.getElementById("image-prompt");
const imageGenerate = document.getElementById("image-generate");
const imageCancel = document.getElementById("image-cancel");
/* Prompt preview & edit modal (feature 4.3). */
const imageModal = document.getElementById("image-modal");
const imageModalPrompt = document.getElementById("image-modal-prompt");
const imageModalNote = document.getElementById("image-modal-note");
const imageModalError = document.getElementById("image-modal-error");
const imageModalGenerate = document.getElementById("image-modal-generate");
const imageModalReenhance = document.getElementById("image-modal-reenhance");
const imageModalClose = document.getElementById("image-modal-close");

const toastEl = document.getElementById("toast");

const hljsDark = document.getElementById("hljs-dark");
const hljsLight = document.getElementById("hljs-light");

const settingsPage = document.getElementById("settings-page");
const settingsBack = document.getElementById("settings-back");
const settingShowTps = document.getElementById("setting-showTps");
const settingEnterToSend = document.getElementById("setting-enterToSend");
const accountUsername = document.getElementById("account-username");
const accountEmail = document.getElementById("account-email");
const accountLogout = document.getElementById("account-logout");
const appVersionEl = document.getElementById("app-version");

const usersSection = document.getElementById("users-section");
const usersTitle = document.getElementById("users-title");
const usersList = document.getElementById("users-list");
const addUserForm = document.getElementById("add-user-form");
const addUserUsername = document.getElementById("add-user-username");
const addUserEmail = document.getElementById("add-user-email");
const addUserPassword = document.getElementById("add-user-password");
const addUserAdmin = document.getElementById("add-user-admin");
const usersError = document.getElementById("users-error");
const settingAllowRegistration = document.getElementById("setting-allow-registration");

const keyStatus = document.getElementById("key-status");
const keyForm = document.getElementById("key-form");
const keyInput = document.getElementById("key-input");
const keyError = document.getElementById("key-error");
const keyRemove = document.getElementById("key-remove");
const shareKeyRow = document.getElementById("share-key-row");
const settingShareServerKey = document.getElementById("setting-share-server-key");

const groupsList = document.getElementById("groups-list");
const groupsEmpty = document.getElementById("groups-empty");
const groupForm = document.getElementById("group-form");
const groupName = document.getElementById("group-name");
const groupShareKey = document.getElementById("group-share-key");
const groupError = document.getElementById("group-error");

/* Calendar (per-user, feature 3.3) — self-contained block; safe to relocate. */
const calendarSection = document.getElementById("calendar-section");
const calendarStatus = document.getElementById("calendar-status");
const calendarKind = document.getElementById("calendar-kind");
const calendarProvider = document.getElementById("calendar-provider");
const calendarCaldavFields = document.getElementById("calendar-caldav-fields");
const calendarICSFields = document.getElementById("calendar-ics-fields");
const calendarEndpoint = document.getElementById("calendar-endpoint");
const calendarUsername = document.getElementById("calendar-username");
const calendarPassword = document.getElementById("calendar-password");
const calendarURL = document.getElementById("calendar-url");
const calendarError = document.getElementById("calendar-error");
const calendarSave = document.getElementById("calendar-save");
const calendarDisconnect = document.getElementById("calendar-disconnect");

/* Integrations (admin only, Phase 2.4 / 2.8). */
const integrationsSection = document.getElementById("integrations-section");
const settingToolsEnabled = document.getElementById("setting-tools-enabled");
/* Web search block (self-contained; safe to relocate). */
const searchStatus = document.getElementById("search-status");
const searchProvider = document.getElementById("search-provider");
const searchBraveKey = document.getElementById("search-brave-key");
const searchBraveMode = document.getElementById("search-brave-mode");
const searchError = document.getElementById("search-error");
const searchSave = document.getElementById("search-save");
const searchClearKey = document.getElementById("search-clear-key");
/* Maps block (self-contained, 3.4; safe to relocate). */
const mapsStatus = document.getElementById("maps-status");
const mapsProvider = document.getElementById("maps-provider");
const mapsRegion = document.getElementById("maps-region");
const mapsUnits = document.getElementById("maps-units");
const mapsError = document.getElementById("maps-error");
const mapsSave = document.getElementById("maps-save");

/* Per-user web search block (2.12; visible to all users, not admin-only). */
const mySearchStatus = document.getElementById("my-search-status");
const mySerperForm = document.getElementById("my-serper-form");
const mySerperKey = document.getElementById("my-serper-key");
const mySerperError = document.getElementById("my-serper-error");
const mySerperClear = document.getElementById("my-serper-clear");
/* Per-user Linkwarden block (2.8; visible to all users, not admin-only). */
const linkwardenForm = document.getElementById("linkwarden-form");
const linkwardenUrl = document.getElementById("linkwarden-url");
const linkwardenToken = document.getElementById("linkwarden-token");
const linkwardenStatus = document.getElementById("linkwarden-status");
const linkwardenError = document.getElementById("linkwarden-error");
const linkwardenClear = document.getElementById("linkwarden-clear");

/* Image generation (feature 3.1) — self-contained settings block. */
const imagegenUrl = document.getElementById("imagegen-url");
const imagegenToken = document.getElementById("imagegen-token");
const imagegenStatus = document.getElementById("imagegen-status");
const imagegenError = document.getElementById("imagegen-error");
const imagegenSave = document.getElementById("imagegen-save");
const imagegenClear = document.getElementById("imagegen-clear");

/* ==== BEGIN code-sandbox tools (2.6/2.7) — self-contained, safe to relocate ==== */
const toolsSection = document.getElementById("tools-section");
const settingSandboxEnabled = document.getElementById("setting-sandbox-enabled");
const sandboxStatus = document.getElementById("sandbox-status");
/* ==== END code-sandbox tools ==== */

/* Image OCR (feature 2.5) — self-contained. */
const ocrSection = document.getElementById("ocr-section");
const settingOcrEnabled = document.getElementById("setting-ocr-enabled");
const ocrStatus = document.getElementById("ocr-status");

/* ===== Usage & quotas (2.11) — self-contained, safe to relocate ===== */
const usageSection = document.getElementById("usage-section");
const usageRange = document.getElementById("usage-range");
const usageTotals = document.getElementById("usage-totals");
const usageByModel = document.getElementById("usage-by-model");
const usageQuotaStatus = document.getElementById("usage-quota-status");
const usageAdmin = document.getElementById("usage-admin");
const settingDailyTokenLimit = document.getElementById("setting-daily-token-limit");
const settingMonthlyCostLimit = document.getElementById("setting-monthly-cost-limit");
const usageError = document.getElementById("usage-error");
const usageSave = document.getElementById("usage-save");
const usageUsersList = document.getElementById("usage-users-list");
/* ===== end Usage & quotas (2.11) ===== */

/* Voice / speech (features 3.5 / 3.6) — self-contained, safe to relocate. */
const voiceSection = document.getElementById("voice-section");
const settingSttEnabled = document.getElementById("setting-stt-enabled");
const settingSttUrl = document.getElementById("setting-stt-url");
const settingTtsEnabled = document.getElementById("setting-tts-enabled");
const settingTtsUrl = document.getElementById("setting-tts-url");
const settingTtsVoice = document.getElementById("setting-tts-voice");
const voiceStatus = document.getElementById("voice-status");
const voiceError = document.getElementById("voice-error");
const voiceSave = document.getElementById("voice-save");

/* ==== BEGIN email (read-only IMAP, feature 3.2) — self-contained, safe to relocate ==== */
const emailForm = document.getElementById("email-form");
const emailProvider = document.getElementById("email-provider");
const emailAddress = document.getElementById("email-address");
const emailHostField = document.getElementById("email-host-field");
const emailHost = document.getElementById("email-host");
const emailPortField = document.getElementById("email-port-field");
const emailPort = document.getElementById("email-port");
const emailPassword = document.getElementById("email-password");
const emailStatus = document.getElementById("email-status");
const emailError = document.getElementById("email-error");
const emailTest = document.getElementById("email-test");
const emailDisconnect = document.getElementById("email-disconnect");
const emailEnabledRow = document.getElementById("email-enabled-row");
const settingEmailEnabled = document.getElementById("setting-email-enabled");
/* ==== END email ==== */

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
  user: null,              // signed-in user {id, username, email, is_admin, status} from /api/auth/me
  registrationOpen: false, // GET /api/auth/status registration_open (remembered for login screen)
  pendingUsers: 0,         // GET /api/settings pending_users (admin badge)
  models: [],              // GET /api/models registry
  conversations: [],       // GET /api/conversations (server order)
  projects: [],            // GET /api/projects
  groups: [],              // GET /api/groups
  expandedProjects: new Set(), // project ids whose chats are shown inline
  activeProjectId: null,   // project new chats are filed into
  current: null,           // open conversation object (with messages)
  currentMessages: [],     // messages of the open conversation
  streaming: false,
  abort: null,             // AbortController while streaming
  pendingAttachments: [],  // uploaded files awaiting send: {id,name,mime,size,chars,uploading?}
  speech: {                // GET /api/speech/status (features 3.5 / 3.6)
    stt_available: false,
    stt_enabled: false,
    tts_available: false,
    tts_enabled: false,
    tts_voice: "",
  },
  mail: { connected: false, enabled: false, presets: [] }, // GET /api/me/mail (feature 3.2)
};

const JSON_HEADERS = { "Content-Type": "application/json" };

/* Shown in Settings → About. Bump alongside notable frontend changes. */
const APP_VERSION = "1.0";

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
class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}
function isForbidden(err) {
  return err instanceof ApiError && err.status === 403;
}

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
    throw new ApiError(message, res.status);
  }
  return res.json();
}

/* Read an error body's {error} message without throwing. */
async function readError(res) {
  try {
    const j = await res.json();
    if (j && j.error) return j.error;
  } catch {
    /* no JSON body */
  }
  return "";
}

function handleUnauthorized() {
  if (state.abort) state.abort.abort();
  state.user = null;
  userNameEl.textContent = "";
  userAdminBadge.hidden = true;
  usersSection.hidden = true;
  settingsPendingBadge.hidden = true;
  state.pendingUsers = 0;
  state.current = null;
  state.currentMessages = [];
  state.conversations = [];
  state.projects = [];
  state.groups = [];
  state.pendingAttachments = [];
  renderAttachments();
  stopSpeaking();
  state.speech = { stt_available: false, stt_enabled: false, tts_available: false, tts_enabled: false, tts_voice: "" };
  appEl.hidden = true;
  hideSettingsPage();
  if (location.hash) history.replaceState(null, "", location.pathname + location.search);
  showLogin();
}

/* ============================== Auth ============================== */

/* Auth gate shows exactly one of: setup / login / register. */
function showAuthCard(card) {
  appEl.hidden = true;
  loginView.hidden = false;
  setupForm.hidden = card !== setupForm;
  loginForm.hidden = card !== loginForm;
  registerForm.hidden = card !== registerForm;
}

function setFormError(node, message) {
  if (message) {
    node.textContent = message;
    node.hidden = false;
  } else {
    node.hidden = true;
  }
}

/* First-run: no users exist yet. */
function showSetup() {
  showAuthCard(setupForm);
  setFormError(setupError, "");
  setTimeout(() => setupUsername.focus(), 0);
}

function showLogin(message) {
  showAuthCard(loginForm);
  setFormError(loginError, message || "");
  // "Create account" is only offered when registrations are open (§9.6).
  showRegisterBtn.hidden = !state.registrationOpen;
  setTimeout(() => loginIdentifier.focus(), 0);
}

function showRegister() {
  showAuthCard(registerForm);
  setFormError(registerError, "");
  setFormError(registerPending, "");
  setTimeout(() => registerUsername.focus(), 0);
}

/* §9.6: registration returns 202 pending — no session is created. */
function showRegisterPending(message) {
  showAuthCard(registerForm);
  setFormError(registerError, "");
  setFormError(registerPending, message || "Your account is awaiting admin approval.");
  setTimeout(() => showLoginBtn.focus(), 0);
}

/* Clear the session server-side (best effort) and return to the login screen. */
async function doLogout() {
  try {
    await fetch("/api/auth/logout", { method: "POST" });
  } catch {
    /* clear locally regardless */
  }
  handleUnauthorized();
}

/* GET /api/auth/me → sets state.user; returns true when signed in. */
async function loadMe() {
  const res = await fetch("/api/auth/me");
  if (res.ok) {
    state.user = await res.json();
    return true;
  }
  if (res.status === 401) {
    state.user = null;
    return false;
  }
  throw new Error("Unexpected server response (" + res.status + ").");
}

/* After a successful setup/login/register cookie is set, pull /me and enter. */
async function afterAuth() {
  try {
    if (!(await loadMe())) {
      showLogin("Session not established. Please sign in again.");
      return;
    }
  } catch (err) {
    showLogin(err.message || "Cannot reach the server.");
    return;
  }
  enterApp();
}

/* Boot: status first, then me (§9.6). */
async function boot() {
  let status;
  try {
    const res = await fetch("/api/auth/status");
    if (!res.ok) {
      showLogin("Unexpected server response (" + res.status + ").");
      return;
    }
    status = await res.json();
  } catch {
    showLogin("Cannot reach the server. Check your connection and reload.");
    return;
  }
  state.registrationOpen = !!(status && status.registration_open);
  if (status && status.needs_setup) {
    showSetup();
    return;
  }
  try {
    if (await loadMe()) enterApp();
    else showLogin();
  } catch (err) {
    showLogin(err.message || "Cannot reach the server.");
  }
}

/* ============================== App bootstrap ============================== */

async function enterApp() {
  loginView.hidden = true;
  appEl.hidden = false;
  state.current = null;
  state.currentMessages = [];
  state.activeProjectId = null;
  renderUser();
  // Voice availability for the composer (non-admin friendly).
  loadSpeechStatus().catch(() => {});
  // Admins: prime the pending-users count for the gear badge (silent).
  if (state.user && state.user.is_admin) {
    loadSettingsInfo(true).catch(() => {});
  }
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
  // Honour a deep link / reload on #/settings.
  applyRoute();
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

/* ============================== Settings page (route #/settings) ============================== */

/* Hash route for the settings surface. The browser/Android back button drives
   hashchange, so it always returns to the previous view; the in-page back
   button mirrors it (history.back) — or clears the hash when settings was the
   entry point (deep link / reload, where there is no earlier in-app entry). */
const SETTINGS_HASH = "#/settings";
let settingsPushed = false; // true when settings was opened from within the app

function isSettingsRoute() {
  return location.hash === SETTINGS_HASH;
}

function navigateToSettings() {
  if (isSettingsRoute()) return;
  settingsPushed = true;
  location.hash = SETTINGS_HASH; // pushes a history entry; hashchange applies it
}

function leaveSettings() {
  if (!isSettingsRoute()) {
    applyRoute();
    return;
  }
  if (settingsPushed) {
    history.back(); // hashchange hides the page and returns to the previous view
  } else {
    // Deep link / reload: no in-app entry to return to — clear the hash without
    // stacking another history entry.
    history.replaceState(null, "", location.pathname + location.search);
    applyRoute();
  }
}

/* Show/hide the settings page to match the current hash. */
function applyRoute() {
  if (state.user && isSettingsRoute()) showSettingsPage();
  else hideSettingsPage();
}

/* ============================== Settings categories (4.4) ============================== */
/* Group the settings sections into categories. Navigation is a left rail on
   desktop/tablet and a horizontal chip bar on mobile; only the active category
   is shown. Role visibility still uses the [hidden] attribute and is evaluated
   separately, so a section shows only when it is both role-visible AND in the
   active category. */
const SETTINGS_CATEGORIES = [
  { id: "account", label: "Account", icon: "👤", sections: ["account-section"] },
  { id: "appearance", label: "Appearance", icon: "🎨", sections: ["appearance-section"] },
  { id: "ai", label: "AI & access", icon: "🔑", sections: ["key-section", "groups-section"] },
  {
    id: "integrations",
    label: "Integrations",
    icon: "🔌",
    sections: ["my-search-section", "linkwarden-section", "calendar-section", "email-section"],
  },
  {
    id: "tools",
    label: "Tools",
    icon: "🧰",
    sections: ["tools-section", "integrations-section", "ocr-section"],
  },
  { id: "voice", label: "Voice", icon: "🗣️", sections: ["voice-section"] },
  { id: "usage", label: "Usage", icon: "📊", sections: ["usage-section"] },
  { id: "users", label: "Users", icon: "👥", sections: ["users-section"] },
  { id: "about", label: "About", icon: "ℹ️", sections: ["about-section"] },
];

let settingsCategory = "account";
let settingsNavBuilt = false;

/* Build the nav + content wrapper once, moving the existing sections into the
   content pane so their ids and BEGIN/END blocks stay intact. */
function buildSettingsNav() {
  if (settingsNavBuilt) return;
  const body = settingsPage.querySelector(".settings-body");
  if (!body) return;
  const nav = el("nav", "settings-nav");
  nav.setAttribute("aria-label", "Settings categories");
  const content = el("div", "settings-content");
  body.querySelectorAll(":scope > .settings-section").forEach((s) => content.appendChild(s));
  for (const cat of SETTINGS_CATEGORIES) {
    const btn = el("button", "settings-nav-item");
    btn.type = "button";
    btn.dataset.cat = cat.id;
    const icon = el("span", "settings-nav-icon");
    icon.setAttribute("aria-hidden", "true");
    icon.textContent = cat.icon;
    const label = el("span", "settings-nav-label");
    label.textContent = cat.label;
    btn.append(icon, label);
    btn.addEventListener("click", () => selectSettingsCategory(cat.id));
    nav.appendChild(btn);
  }
  body.append(nav, content);
  for (const cat of SETTINGS_CATEGORIES) {
    for (const id of cat.sections) {
      const node = document.getElementById(id);
      if (node) node.dataset.cat = cat.id;
    }
  }
  settingsNavBuilt = true;
}

/* A category is navigable when at least one of its sections is role-visible. */
function categoryHasVisibleSection(id) {
  const cat = SETTINGS_CATEGORIES.find((c) => c.id === id);
  if (!cat) return false;
  return cat.sections.some((sid) => {
    const n = document.getElementById(sid);
    return n && !n.hidden;
  });
}

/* Hide nav entries whose sections are all role-hidden (admin-only for others). */
function refreshSettingsNav() {
  for (const cat of SETTINGS_CATEGORIES) {
    const btn = settingsPage.querySelector('.settings-nav-item[data-cat="' + cat.id + '"]');
    if (btn) btn.hidden = !categoryHasVisibleSection(cat.id);
  }
}

/* Show only the chosen category's sections. */
function selectSettingsCategory(id) {
  if (!categoryHasVisibleSection(id)) {
    const first = SETTINGS_CATEGORIES.find((c) => categoryHasVisibleSection(c.id));
    if (first) id = first.id;
  }
  settingsCategory = id;
  settingsPage.querySelectorAll(".settings-nav-item").forEach((b) => {
    const on = b.dataset.cat === id;
    b.classList.toggle("active", on);
    if (on) b.setAttribute("aria-current", "true");
    else b.removeAttribute("aria-current");
  });
  settingsPage.querySelectorAll(".settings-section").forEach((s) => {
    s.classList.toggle("cat-hidden", s.dataset.cat !== id);
  });
  const activeBtn = settingsPage.querySelector('.settings-nav-item[data-cat="' + id + '"]');
  if (activeBtn && activeBtn.scrollIntoView) {
    try {
      activeBtn.scrollIntoView({ block: "nearest", inline: "center" });
    } catch (e) {
      /* scrollIntoView options unsupported — ignore */
    }
  }
  const body = settingsPage.querySelector(".settings-body");
  if (body) body.scrollTop = 0;
}

function showSettingsPage() {
  applySettingsToUI();
  renderAccount();
  const isAdmin = !!(state.user && state.user.is_admin);
  usersSection.hidden = !isAdmin;
  shareKeyRow.hidden = !isAdmin;
  integrationsSection.hidden = !isAdmin;
  toolsSection.hidden = !isAdmin; /* code-sandbox tools (2.6/2.7) */
  ocrSection.hidden = !isAdmin;
  usageAdmin.hidden = !isAdmin; /* usage & quotas (2.11) */
  voiceSection.hidden = !isAdmin; /* voice / speech (3.5/3.6) */
  settingsPage.hidden = false;
  buildSettingsNav();
  refreshSettingsNav();
  selectSettingsCategory(settingsCategory);
  loadKeyStatus().catch(() => {});
  loadGroups().catch(() => {});
  loadUsage().catch(() => {}); /* usage & quotas (2.11) */
  loadMySearch().catch(() => {}); /* per-user web search (2.12) */
  loadMyLinkwarden().catch(() => {}); /* per-user Linkwarden (2.8) */
  loadCalendarStatus().catch(() => {});
  loadMail().catch(() => {}); /* email (feature 3.2) — per-user, all users */
  if (isAdmin) {
    loadUsers().catch(() => {});
    loadSettingsInfo(false).catch(() => {});
    loadAllUsersUsage().catch(() => {});
  }
  setTimeout(() => settingsBack.focus({ preventScroll: true }), 0);
}

function hideSettingsPage() {
  if (settingsPage.hidden) return;
  settingsPage.hidden = true;
  settingsPushed = false;
}

/* Account section: username/email from the loaded session user + logout. */
function renderAccount() {
  const u = state.user;
  accountUsername.textContent = u ? u.username : "";
  accountEmail.textContent = u ? u.email || "" : "";
}

function applySettingsToUI() {
  settingsPage.querySelectorAll('input[name="theme"]').forEach((r) => {
    r.checked = r.value === settings.theme;
  });
  settingsPage.querySelectorAll('input[name="thinking"]').forEach((r) => {
    r.checked = r.value === settings.thinking;
  });
  settingShowTps.checked = settings.showTps;
  settingEnterToSend.checked = settings.enterToSend;
}

/* ============================== Auth: signed-in user ============================== */

/* Sidebar footer + admin-only settings visibility (contract §9.6). */
function renderUser() {
  const user = state.user;
  userNameEl.textContent = user ? user.username : "";
  const isAdmin = !!(user && user.is_admin);
  userAdminBadge.hidden = !isAdmin;
  usersSection.hidden = !isAdmin;
  ocrSection.hidden = !isAdmin;
  usageAdmin.hidden = !isAdmin;
  voiceSection.hidden = !isAdmin; /* voice / speech (3.5/3.6) */
}

/* ============================== Admin: users panel ============================== */

async function loadUsers() {
  try {
    const users = await api("/api/users");
    renderUsers(Array.isArray(users) ? users : []);
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      usersSection.hidden = true;
      toast("Admin access required");
      return;
    }
    toast(err.message);
  }
}

function userActionButton(label, onClick, danger) {
  const btn = el("button", "user-action" + (danger ? " danger" : ""));
  btn.type = "button";
  btn.textContent = label;
  btn.addEventListener("click", onClick);
  return btn;
}

function renderUsers(users) {
  usersList.innerHTML = "";
  if (users.length === 0) {
    const empty = el("li", "nav-empty");
    empty.textContent = "No users.";
    usersList.appendChild(empty);
    return;
  }
  // Pending registrations sort first so they are easy to act on.
  const rank = (s) => (s === "pending" ? 0 : s === "disabled" ? 2 : 1);
  const sorted = users.slice().sort((a, b) => rank(a.status) - rank(b.status));

  for (const u of sorted) {
    const status = u.status || "active";
    const isSelf = !!(state.user && state.user.id === u.id);
    const li = el("li", "user-item" + (status === "pending" ? " user-pending" : ""));

    const info = el("div", "user-item-info");
    const line = el("div", "user-item-line");
    const name = el("span", "user-item-name");
    name.textContent = u.username;
    line.appendChild(name);
    if (u.is_admin) {
      const adminBadge = el("span", "badge");
      adminBadge.textContent = "admin";
      line.appendChild(adminBadge);
    }
    const statusBadge = el("span", "badge user-status user-status-" + status);
    statusBadge.textContent = status;
    line.appendChild(statusBadge);

    const email = el("span", "user-item-email");
    email.textContent = u.email;
    info.append(line, email);

    const actions = el("div", "user-actions");
    // Never offer status changes on your own row (contract §9.6).
    if (!isSelf) {
      if (status === "pending") {
        actions.appendChild(userActionButton("Approve", () => patchUserStatus(u, "active")));
        actions.appendChild(userActionButton("Reject", () => deleteUser(u), true));
      } else if (status === "disabled") {
        actions.appendChild(userActionButton("Enable", () => patchUserStatus(u, "active")));
      } else {
        actions.appendChild(userActionButton("Disable", () => patchUserStatus(u, "disabled")));
      }
    }
    // Generic delete for non-pending rows (pending uses Reject); disabled on self.
    if (status !== "pending") {
      const del = el("button", "icon-btn user-delete");
      del.type = "button";
      del.innerHTML = ICONS.trash;
      del.setAttribute("aria-label", "Delete user " + u.username);
      if (isSelf) {
        del.disabled = true;
        del.title = "You cannot delete your own account";
      } else {
        del.addEventListener("click", () => deleteUser(u));
      }
      actions.appendChild(del);
    }

    li.append(info, actions);
    usersList.appendChild(li);
  }
}

async function patchUserStatus(user, status) {
  const wasPending = user.status === "pending";
  try {
    await api("/api/users/" + enc(user.id), {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ status }),
    });
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(isForbidden(err) ? "Admin access required" : err.message);
    return;
  }
  await loadUsers();
  await loadSettingsInfo(true);
  if (status === "active") toast(wasPending ? "User approved" : "User enabled");
  else toast("User disabled");
}

async function deleteUser(user) {
  if (
    !window.confirm(
      'Delete user "' + user.username + '"? Their conversations and projects will be deleted.'
    )
  ) {
    return;
  }
  try {
    await api("/api/users/" + enc(user.id), { method: "DELETE" });
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(isForbidden(err) ? "Admin access required" : err.message);
    return;
  }
  await loadUsers();
  await loadSettingsInfo(true);
  toast("User deleted");
}

async function addUser() {
  setFormError(usersError, "");
  const username = addUserUsername.value.trim();
  const email = addUserEmail.value.trim();
  const password = addUserPassword.value;
  if (password.length < 8) {
    setFormError(usersError, "Password must be at least 8 characters.");
    return;
  }
  try {
    await api("/api/users", {
      method: "POST",
      headers: JSON_HEADERS,
      body: JSON.stringify({ username, email, password, is_admin: addUserAdmin.checked }),
    });
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      toast("Admin access required");
      return;
    }
    setFormError(usersError, err.message);
    return;
  }
  addUserForm.reset();
  await loadUsers();
  toast("User added");
}

async function loadSettingsInfo(silent) {
  try {
    const s = await api("/api/settings");
    settingAllowRegistration.checked = !!(s && s.allow_registration);
    if (settingShareServerKey) settingShareServerKey.checked = !!(s && s.share_server_key);
    setPendingCount(s && typeof s.pending_users === "number" ? s.pending_users : 0);
    applyIntegrationSettings(s);
    applySandboxSettings(s); /* code-sandbox tools (2.6/2.7) */
    applyOCRSettings(s);
    applyUsageSettings(s); /* usage & quotas (2.11) */
    applyMapsSettings(s); /* maps (3.4) */
    applyVoiceSettings(s); /* voice / speech (3.5/3.6) */
    applyImagegenSettings(s); /* image generation (3.1) */
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      usersSection.hidden = true;
      integrationsSection.hidden = true;
      toolsSection.hidden = true; /* code-sandbox tools (2.6/2.7) */
      ocrSection.hidden = true;
      usageAdmin.hidden = true; /* usage & quotas (2.11) */
      voiceSection.hidden = true; /* voice / speech (3.5/3.6) */
      return;
    }
    if (!silent) toast(err.message);
  }
}

/* Populate the admin Integrations section from GET /api/settings. */
function applyIntegrationSettings(s) {
  if (!s) return;
  if (settingToolsEnabled) settingToolsEnabled.checked = s.tools_enabled !== false;
  applySearchSettings(s);
}

/* Populate the self-contained web search block. */
function applySearchSettings(s) {
  if (searchProvider) searchProvider.value = s.search_provider || "auto";
  if (searchBraveKey) searchBraveKey.value = "";
  if (searchBraveMode) searchBraveMode.value = s.brave_mode || "web";
  if (!searchStatus) return;
  const providers = [
    "Tavily: " + (s.tavily_configured ? "configured" : "not configured"),
    "Brave: " + (s.brave_configured ? "configured" : "not configured"),
    "Serper: " + (s.serper_configured ? "configured" : "not configured"),
    "DuckDuckGo: always available",
  ];
  const active = s.search_active ? s.search_active : "none";
  searchStatus.textContent = "Server — active provider: " + active + " · " + providers.join(" · ");
}

/* Populate the self-contained maps block (3.4). */
function applyMapsSettings(s) {
  if (!s) return;
  if (mapsProvider) mapsProvider.value = s.maps_provider || "osm";
  if (mapsRegion) mapsRegion.value = s.maps_region || "";
  if (mapsUnits) mapsUnits.value = s.maps_units || "metric";
  if (mapsStatus) {
    const active = s.maps_active ? s.maps_active : "osm";
    mapsStatus.textContent =
      "Active provider: " + active + " · OpenStreetMap (keyless, © OpenStreetMap contributors)";
  }
}

async function saveMaps() {
  setFormError(mapsError, "");
  const body = {
    maps_provider: mapsProvider ? mapsProvider.value : "osm",
    maps_region: mapsRegion ? mapsRegion.value.trim() : "",
    maps_units: mapsUnits ? mapsUnits.value : "metric",
  };
  if (mapsSave) mapsSave.disabled = true;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    applyMapsSettings(s);
    toast("Maps settings saved");
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      toast("Admin access required");
      return;
    }
    setFormError(mapsError, err.message);
  } finally {
    if (mapsSave) mapsSave.disabled = false;
  }
}

async function saveSearch() {
  setFormError(searchError, "");
  const body = { search_provider: searchProvider ? searchProvider.value : "auto" };
  const key = searchBraveKey ? searchBraveKey.value.trim() : "";
  // Only send the key when provided so saving the provider keeps the stored one.
  if (key) body.brave_key = key;
  if (searchBraveMode) body.brave_mode = searchBraveMode.value;
  if (searchSave) searchSave.disabled = true;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    applyIntegrationSettings(s);
    toast("Web search saved");
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      toast("Admin access required");
      return;
    }
    setFormError(searchError, err.message);
  } finally {
    if (searchSave) searchSave.disabled = false;
  }
}

/* ==== BEGIN per-user web search (2.12) — self-contained, safe to relocate ==== */
/* Populate the per-user Serper key block from GET /api/me/serper (all users). */
async function loadMySearch() {
  if (!mySearchStatus) return;
  try {
    const s = await api("/api/me/serper");
    if (mySerperKey) mySerperKey.value = "";
    const providers = [
      "Tavily: " + (s.tavily_configured ? "configured" : "not configured"),
      "Brave: " + (s.brave_configured ? "configured" : "not configured"),
      "Serper: " + (s.serper_configured ? "configured" : "not configured"),
      "DuckDuckGo: always available",
    ];
    const active = s.search_active ? s.search_active : "none";
    const mine = s.has_key
      ? "your Serper key set" + (s.key_hint ? " (…" + s.key_hint + ")" : "")
      : s.server_key_present
        ? "using the server Serper key"
        : "no Serper key";
    mySearchStatus.textContent =
      "Active provider: " + active + " · " + providers.join(" · ") + " · " + mine + ".";
  } catch (err) {
    if (err instanceof AuthError) return;
    mySearchStatus.textContent = "Web search: " + err.message;
  }
}

async function saveMySerper() {
  setFormError(mySerperError, "");
  const key = mySerperKey ? mySerperKey.value.trim() : "";
  if (!key) {
    setFormError(mySerperError, "Enter a Serper API key (or use Clear key).");
    return;
  }
  try {
    await api("/api/me/serper", {
      method: "PUT",
      headers: JSON_HEADERS,
      body: JSON.stringify({ api_key: key }),
    });
    toast("Serper key saved");
    await loadMySearch();
  } catch (err) {
    if (err instanceof AuthError) return;
    setFormError(mySerperError, err.message);
  }
}

async function clearMySerper() {
  if (!confirm("Clear your stored Serper API key?")) return;
  setFormError(mySerperError, "");
  try {
    await api("/api/me/serper", {
      method: "PUT",
      headers: JSON_HEADERS,
      body: JSON.stringify({ api_key: "" }),
    });
    toast("Serper key cleared");
    await loadMySearch();
  } catch (err) {
    if (err instanceof AuthError) return;
    setFormError(mySerperError, err.message);
  }
}
/* ==== END per-user web search ==== */

/* ==== BEGIN code-sandbox tools (2.6/2.7) — self-contained, safe to relocate ==== */
/* Populate the admin Code sandbox section from GET /api/settings. */
function applySandboxSettings(s) {
  if (!s) return;
  if (settingSandboxEnabled) settingSandboxEnabled.checked = s.sandbox_enabled !== false;
  if (sandboxStatus) {
    sandboxStatus.textContent = s.sandbox_available
      ? "Sandbox: available (Docker reachable)."
      : "Sandbox: unavailable — Docker is not reachable, or images/network are missing (run deploy/sandbox-setup.sh).";
  }
}

async function clearBraveKey() {
  if (!confirm("Clear the stored Brave API key?")) return;
  setFormError(searchError, "");
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ brave_key: "" }),
    });
    applyIntegrationSettings(s);
    toast("Brave key cleared");
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(isForbidden(err) ? "Admin access required" : err.message);
  }
}

async function saveSandboxEnabled() {
  const value = settingSandboxEnabled.checked;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ sandbox_enabled: value }),
    });
    applySandboxSettings(s);
    toast(value ? "Code sandbox enabled" : "Code sandbox disabled");
  } catch (err) {
    if (err instanceof AuthError) return;
    settingSandboxEnabled.checked = !value;
    toast(isForbidden(err) ? "Admin access required" : err.message);
  }
}
/* ==== END code-sandbox tools ==== */

async function saveToolsEnabled() {
  const value = settingToolsEnabled.checked;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ tools_enabled: value }),
    });
    applyIntegrationSettings(s);
    toast(value ? "Tools enabled" : "Tools disabled");
  } catch (err) {
    if (err instanceof AuthError) return;
    settingToolsEnabled.checked = !value;
    toast(isForbidden(err) ? "Admin access required" : err.message);
  }
}

/* Image OCR (feature 2.5) — populate the admin OCR block from GET /api/settings. */
function applyOCRSettings(s) {
  if (!s) return;
  if (settingOcrEnabled) settingOcrEnabled.checked = s.ocr_enabled !== false;
  if (ocrStatus) {
    if (s.ocr_available === false) {
      ocrStatus.textContent = "OCR engine not configured on the server.";
    } else {
      ocrStatus.textContent = s.ocr_enabled === false
        ? "Image OCR is off."
        : "Image OCR is on.";
    }
  }
}

async function saveOcrEnabled() {
  const value = settingOcrEnabled.checked;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ ocr_enabled: value }),
    });
    applyOCRSettings(s);
    toast(value ? "Image OCR enabled" : "Image OCR disabled");
  } catch (err) {
    if (err instanceof AuthError) return;
    settingOcrEnabled.checked = !value;
    toast(isForbidden(err) ? "Admin access required" : err.message);
  }
}

/* ===== BEGIN Usage & quotas (2.11) — self-contained, safe to relocate ===== */

/* Load the signed-in user's usage for the selected range. */
async function loadUsage() {
  if (!usageTotals) return;
  const range = usageRange ? usageRange.value : "30d";
  usageTotals.textContent = "Loading…";
  try {
    const data = await api("/api/me/usage?range=" + enc(range));
    renderUsage(data);
  } catch (err) {
    if (err instanceof AuthError) return;
    usageTotals.textContent = "Could not load usage.";
  }
}

function fmtTokens(n) {
  return typeof n === "number" ? n.toLocaleString() : "0";
}

function fmtCost(micros) {
  return "$" + ((typeof micros === "number" ? micros : 0) / 1e6).toFixed(4);
}

function renderUsage(data) {
  if (!usageTotals) return;
  const t = (data && data.totals) || {};
  usageTotals.textContent =
    fmtTokens(t.prompt_tokens) + " prompt · " +
    fmtTokens(t.completion_tokens) + " completion · " +
    fmtTokens(t.cached_tokens) + " cached · " +
    fmtCost(t.cost_micros) + " · " +
    fmtTokens(t.messages) + " replies";

  if (usageByModel) {
    usageByModel.innerHTML = "";
    const rows = Array.isArray(data && data.by_model) ? data.by_model : [];
    for (const m of rows) {
      const line = el("div", "usage-model-line");
      line.textContent =
        (m.model || "unknown") + " · " +
        fmtTokens((m.prompt_tokens || 0) + (m.completion_tokens || 0)) + " tokens · " +
        fmtCost(m.cost_micros);
      usageByModel.appendChild(line);
    }
  }

  if (usageQuotaStatus) {
    const q = (data && data.quota) || {};
    const parts = [];
    if (q.daily_token_limit > 0) {
      parts.push(
        "Daily tokens: " + fmtTokens(q.today_tokens) + " / " + fmtTokens(q.daily_token_limit) +
        (q.daily_exceeded ? " (limit reached)" : "")
      );
    }
    if (q.monthly_cost_limit > 0) {
      parts.push(
        "Monthly cost: " + fmtCost(q.month_cost_micros) + " / " + fmtCost(q.monthly_cost_limit) +
        (q.monthly_exceeded ? " (limit reached)" : "")
      );
    }
    usageQuotaStatus.textContent = parts.length ? parts.join(" · ") : "No quota limits set.";
  }
}

/* Populate the admin quota controls from GET /api/settings. */
function applyUsageSettings(s) {
  if (!s) return;
  if (settingDailyTokenLimit) {
    settingDailyTokenLimit.value = typeof s.daily_token_limit === "number" ? s.daily_token_limit : 0;
  }
  if (settingMonthlyCostLimit) {
    const micros = typeof s.monthly_cost_limit === "number" ? s.monthly_cost_limit : 0;
    settingMonthlyCostLimit.value = (micros / 1e6).toString();
  }
}

async function saveUsageLimits() {
  setFormError(usageError, "");
  const daily = parseInt(settingDailyTokenLimit ? settingDailyTokenLimit.value : "0", 10);
  const monthlyDollars = parseFloat(settingMonthlyCostLimit ? settingMonthlyCostLimit.value : "0");
  if (isNaN(daily) || daily < 0 || isNaN(monthlyDollars) || monthlyDollars < 0) {
    setFormError(usageError, "Limits must be 0 or a positive number.");
    return;
  }
  const body = {
    daily_token_limit: daily,
    monthly_cost_limit: Math.round(monthlyDollars * 1e6),
  };
  if (usageSave) usageSave.disabled = true;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    applyUsageSettings(s);
    await loadUsage();
    toast("Quota limits saved");
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      toast("Admin access required");
      return;
    }
    setFormError(usageError, err.message);
  } finally {
    if (usageSave) usageSave.disabled = false;
  }
}

/* Admin: all users' usage for the selected range. */
async function loadAllUsersUsage() {
  if (!usageUsersList) return;
  const range = usageRange ? usageRange.value : "30d";
  try {
    const data = await api("/api/users/usage?range=" + enc(range));
    renderAllUsersUsage(data);
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      if (usageAdmin) usageAdmin.hidden = true;
      return;
    }
    toast(err.message);
  }
}

function renderAllUsersUsage(data) {
  if (!usageUsersList) return;
  usageUsersList.innerHTML = "";
  const users = Array.isArray(data && data.users) ? data.users : [];
  if (users.length === 0) {
    const empty = el("li", "nav-empty");
    empty.textContent = "No usage yet.";
    usageUsersList.appendChild(empty);
    return;
  }
  for (const u of users) {
    const li = el("li", "usage-user-item");
    const name = el("span", "usage-user-name");
    name.textContent = u.username || u.user_id || "unknown";
    const detail = el("span", "usage-user-detail");
    detail.textContent =
      fmtTokens((u.prompt_tokens || 0) + (u.completion_tokens || 0)) + " tokens · " +
      fmtCost(u.cost_micros) + " · " + fmtTokens(u.messages) + " replies";
    li.append(name, detail);
    usageUsersList.appendChild(li);
  }
}

/* ===== end Usage & quotas (2.11) ===== */

/* Voice / speech (features 3.5 / 3.6) — populate the admin Voice block. */
function applyVoiceSettings(s) {
  if (!s) return;
  if (settingSttEnabled) settingSttEnabled.checked = s.stt_enabled !== false;
  if (settingTtsEnabled) settingTtsEnabled.checked = s.tts_enabled !== false;
  if (settingSttUrl) settingSttUrl.value = s.stt_url || "";
  if (settingTtsUrl) settingTtsUrl.value = s.tts_url || "";
  if (settingTtsVoice) settingTtsVoice.value = s.tts_voice || "";
  if (voiceStatus) {
    const stt = s.stt_available ? "STT ready" : "STT not configured";
    const tts = s.tts_available ? "TTS ready" : "TTS not configured";
    const voice = s.tts_voice ? " · voice " + s.tts_voice : "";
    voiceStatus.textContent = stt + " · " + tts + voice;
  }
}

async function saveVoiceSettings() {
  setFormError(voiceError, "");
  const body = {
    stt_enabled: !!(settingSttEnabled && settingSttEnabled.checked),
    tts_enabled: !!(settingTtsEnabled && settingTtsEnabled.checked),
    stt_url: settingSttUrl ? settingSttUrl.value.trim() : "",
    tts_url: settingTtsUrl ? settingTtsUrl.value.trim() : "",
    tts_voice: settingTtsVoice ? settingTtsVoice.value.trim() : "",
  };
  if (voiceSave) voiceSave.disabled = true;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    applyVoiceSettings(s);
    loadSpeechStatus();
    toast("Voice settings saved");
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      toast("Admin access required");
      return;
    }
    setFormError(voiceError, err.message);
  } finally {
    if (voiceSave) voiceSave.disabled = false;
  }
}

/* ===== end Voice / speech (3.5/3.6) ===== */

/* ==== BEGIN email (read-only IMAP, feature 3.2) — self-contained, safe to relocate ==== */
/* Load the caller's mailbox status (per-user, not admin-gated). */
async function loadMail() {
  try {
    const m = await api("/api/me/mail");
    state.mail = m || { connected: false, enabled: false, presets: [] };
    applyMailStatus();
  } catch (err) {
    if (err instanceof AuthError) return;
    if (!isForbidden(err)) toast(err.message);
  }
}

function populateEmailProviders(presets) {
  if (!emailProvider) return;
  const list = Array.isArray(presets) && presets.length ? presets : state.mail.presets || [];
  // Rebuild only when the option set changes, so the current selection is kept.
  if (emailProvider.options.length === list.length) return;
  const current = emailProvider.value;
  emailProvider.innerHTML = "";
  list.forEach((p) => {
    const opt = document.createElement("option");
    opt.value = p.id;
    opt.textContent = p.name || p.id;
    opt.dataset.host = p.host || "";
    opt.dataset.port = p.port ? String(p.port) : "";
    emailProvider.appendChild(opt);
  });
  if (current) emailProvider.value = current;
}

function providerLabel(id) {
  const opt = emailProvider
    ? Array.from(emailProvider.options).find((o) => o.value === id)
    : null;
  return opt ? opt.textContent : id || "";
}

function updateEmailCustomFields() {
  const custom = emailProvider && emailProvider.value === "custom";
  if (emailHostField) emailHostField.hidden = !custom;
  if (emailPortField) emailPortField.hidden = !custom;
}

function applyMailStatus() {
  const m = state.mail || {};
  populateEmailProviders(m.presets);
  if (m.connected) {
    const label = providerLabel(m.provider);
    emailStatus.textContent =
      "Connected: " + (m.email || "") + (label ? " (" + label + ")" : "") +
      " · " + (m.enabled ? "tools on" : "tools off") + ".";
    emailAddress.value = m.email || "";
    if (m.provider) emailProvider.value = m.provider;
    if (emailHost) emailHost.value = m.imap_host || "";
    if (emailPort) emailPort.value = m.imap_port ? String(m.imap_port) : "";
    emailPassword.value = "";
    emailDisconnect.hidden = false;
    emailEnabledRow.hidden = false;
    settingEmailEnabled.checked = m.enabled !== false;
  } else {
    emailStatus.textContent = "Not connected.";
    emailDisconnect.hidden = true;
    emailEnabledRow.hidden = true;
  }
  updateEmailCustomFields();
}

function emailPayload() {
  return {
    provider: emailProvider ? emailProvider.value : "custom",
    email: emailAddress ? emailAddress.value.trim() : "",
    password: emailPassword ? emailPassword.value : "",
    imap_host: emailHost ? emailHost.value.trim() : "",
    imap_port: emailPort && emailPort.value ? parseInt(emailPort.value, 10) : 0,
  };
}

async function saveMail(e) {
  if (e) e.preventDefault();
  setFormError(emailError, "");
  const payload = emailPayload();
  if (!payload.email) {
    setFormError(emailError, "Enter the mailbox email address.");
    return;
  }
  if (!payload.password) {
    setFormError(emailError, "Enter the app-specific password.");
    return;
  }
  const btn = emailForm.querySelector('button[type="submit"]');
  btn.disabled = true;
  btn.textContent = "Testing…";
  try {
    const m = await api("/api/me/mail", {
      method: "PUT",
      headers: JSON_HEADERS,
      body: JSON.stringify(payload),
    });
    state.mail = m;
    applyMailStatus();
    toast("Mailbox connected");
  } catch (err) {
    if (err instanceof AuthError) return;
    setFormError(emailError, err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "Save";
  }
}

async function testMail() {
  setFormError(emailError, "");
  const payload = emailPayload();
  if (!payload.email || !payload.password) {
    setFormError(emailError, "Enter the email address and app-specific password.");
    return;
  }
  emailTest.disabled = true;
  emailTest.textContent = "Testing…";
  try {
    await api("/api/me/mail/test", {
      method: "POST",
      headers: JSON_HEADERS,
      body: JSON.stringify(payload),
    });
    toast("Connection OK");
  } catch (err) {
    if (err instanceof AuthError) return;
    setFormError(emailError, err.message);
  } finally {
    emailTest.disabled = false;
    emailTest.textContent = "Test connection";
  }
}

async function disconnectMail() {
  if (!confirm("Disconnect this mailbox and delete its stored password?")) return;
  setFormError(emailError, "");
  try {
    await api("/api/me/mail", { method: "DELETE" });
    state.mail = { connected: false, enabled: false, presets: state.mail.presets };
    emailForm.reset();
    applyMailStatus();
    toast("Mailbox disconnected");
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(isForbidden(err) ? "Not allowed" : err.message);
  }
}

async function toggleEmailEnabled() {
  const value = settingEmailEnabled.checked;
  try {
    const m = await api("/api/me/mail", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ enabled: value }),
    });
    state.mail = m;
    applyMailStatus();
    toast(value ? "Email tools enabled" : "Email tools disabled");
  } catch (err) {
    if (err instanceof AuthError) return;
    settingEmailEnabled.checked = !value;
    toast(isForbidden(err) ? "Not allowed" : err.message);
  }
}
/* ==== END email ==== */

/* ==== BEGIN per-user Linkwarden (2.8) — self-contained, safe to relocate ==== */
/* Populate the per-user Linkwarden block from GET /api/me/linkwarden (all users). */
async function loadMyLinkwarden() {
  if (!linkwardenStatus) return;
  try {
    const s = await api("/api/me/linkwarden");
    if (linkwardenUrl) linkwardenUrl.value = s.url || "";
    if (linkwardenToken) linkwardenToken.value = "";
    linkwardenStatus.textContent = s.configured
      ? "Configured" + (s.token_hint ? " (token …" + s.token_hint + ")" : "") + "."
      : "Not configured.";
  } catch (err) {
    if (err instanceof AuthError) return;
    linkwardenStatus.textContent = "Linkwarden: " + err.message;
  }
}

async function saveLinkwarden(e) {
  if (e) e.preventDefault();
  setFormError(linkwardenError, "");
  const url = linkwardenUrl.value.trim();
  const token = linkwardenToken.value.trim();
  if (!url) {
    setFormError(linkwardenError, "Enter the Linkwarden instance URL.");
    return;
  }
  const body = { url };
  // Only send the token when provided so saving the URL keeps the stored one.
  if (token) body.token = token;
  const btn = linkwardenForm.querySelector('button[type="submit"]');
  if (btn) btn.disabled = true;
  try {
    await api("/api/me/linkwarden", {
      method: "PUT",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    toast("Linkwarden saved");
    await loadMyLinkwarden();
  } catch (err) {
    if (err instanceof AuthError) return;
    setFormError(linkwardenError, err.message);
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function clearLinkwarden() {
  if (!confirm("Disconnect Linkwarden and clear your stored URL and token?")) return;
  setFormError(linkwardenError, "");
  try {
    await api("/api/me/linkwarden", { method: "DELETE" });
    toast("Linkwarden disconnected");
    await loadMyLinkwarden();
  } catch (err) {
    if (err instanceof AuthError) return;
    setFormError(linkwardenError, err.message);
  }
}
/* ==== END per-user Linkwarden ==== */

/* ===== Image generation (feature 3.1) — self-contained settings block ===== */
/* Populate the admin Image generation block from GET /api/settings. */
function applyImagegenSettings(s) {
  if (!s) return;
  if (imagegenUrl) imagegenUrl.value = s.imagegen_url || "";
  if (imagegenToken) imagegenToken.value = "";
  if (imagegenStatus) {
    if (!s.imagegen_configured) {
      imagegenStatus.textContent = "Not configured.";
    } else if (s.imagegen_available) {
      imagegenStatus.textContent =
        "Configured and online." +
        (s.imagegen_token_hint ? " (token …" + s.imagegen_token_hint + ")" : "");
    } else {
      imagegenStatus.textContent = "Configured, but the backend is unreachable.";
    }
  }
}

async function saveImagegen() {
  setFormError(imagegenError, "");
  const url = imagegenUrl ? imagegenUrl.value.trim() : "";
  if (!url) {
    setFormError(imagegenError, "Enter the ComfyUI URL.");
    return;
  }
  const body = { imagegen_url: url };
  // Only send the token when provided so saving the URL keeps the stored one.
  const token = imagegenToken ? imagegenToken.value.trim() : "";
  if (token) body.imagegen_token = token;
  if (imagegenSave) imagegenSave.disabled = true;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    applyImagegenSettings(s);
    toast("Image generation saved");
  } catch (err) {
    if (err instanceof AuthError) return;
    if (isForbidden(err)) {
      toast("Admin access required");
      return;
    }
    setFormError(imagegenError, err.message);
  } finally {
    if (imagegenSave) imagegenSave.disabled = false;
  }
}

async function clearImagegen() {
  if (!confirm("Clear the image generation URL and token?")) return;
  setFormError(imagegenError, "");
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ imagegen_url: "", imagegen_token: "" }),
    });
    applyImagegenSettings(s);
    toast("Image generation cleared");
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(isForbidden(err) ? "Admin access required" : err.message);
  }
}
/* ===== end image generation settings ===== */

/* Users section header "Users (N pending)" + optional gear badge. */
function setPendingCount(n) {
  state.pendingUsers = n;
  const pending = n > 0;
  if (usersTitle) usersTitle.textContent = pending ? "Users (" + n + " pending)" : "Users";
  settingsPendingBadge.hidden = !pending;
  settingsPendingBadge.textContent = String(n);
  settingsBtn.title = pending ? pending + " user(s) awaiting approval" : "Settings";
}

async function saveRegistrationSetting() {
  const value = settingAllowRegistration.checked;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ allow_registration: value }),
    });
    const now = !!(s && s.allow_registration);
    settingAllowRegistration.checked = now;
    state.registrationOpen = now;
    setPendingCount(s && typeof s.pending_users === "number" ? s.pending_users : state.pendingUsers);
    toast(now ? "Registrations open" : "Registrations closed");
  } catch (err) {
    if (err instanceof AuthError) return;
    settingAllowRegistration.checked = !value;
    toast(isForbidden(err) ? "Admin access required" : err.message);
  }
}

/* ============================== OpenCode Go key (BYOK, §2.1) ============================== */

async function loadKeyStatus() {
  try {
    const k = await api("/api/me/key");
    keyRemove.hidden = !(k && k.has_key);
    if (k && k.has_key) {
      keyStatus.textContent = "Using your own key" + (k.key_hint ? " (…" + k.key_hint + ")" : "");
    } else if (k && k.using_group_key) {
      const who = k.group_name ? k.group_name + "'s" : "your group's";
      keyStatus.textContent = "Using " + who + " shared key.";
    } else if (k && k.using_shared_key) {
      keyStatus.textContent = "Using the shared server key.";
    } else if (k && k.server_key_present && !k.shared_key_available) {
      keyStatus.textContent = "No key set. The server key exists but sharing is off — add your own key.";
    } else {
      keyStatus.textContent = "No key set. Add your OpenCode Go key to start chatting.";
    }
  } catch (err) {
    if (err instanceof AuthError) return;
    keyStatus.textContent = "Could not load key status.";
  }
}

function showKeyError(msg) {
  keyError.textContent = msg;
  keyError.hidden = false;
}

async function saveKey(e) {
  if (e) e.preventDefault();
  const value = keyInput.value.trim();
  if (!value) {
    showKeyError("Enter a key, or use Remove.");
    return;
  }
  keyError.hidden = true;
  const btn = keyForm.querySelector('button[type="submit"]');
  btn.disabled = true;
  btn.textContent = "Checking…";
  try {
    await api("/api/me/key", {
      method: "PUT",
      headers: JSON_HEADERS,
      body: JSON.stringify({ api_key: value }),
    });
    keyInput.value = "";
    await loadKeyStatus();
    toast("Key saved");
  } catch (err) {
    if (err instanceof AuthError) return;
    showKeyError(err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "Save key";
  }
}

async function removeKey() {
  if (!confirm("Remove your stored key? Chats will use the shared key if available.")) return;
  try {
    await api("/api/me/key", {
      method: "PUT",
      headers: JSON_HEADERS,
      body: JSON.stringify({ api_key: "" }),
    });
    await loadKeyStatus();
    toast("Key removed");
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(err.message);
  }
}

async function saveShareServerKey() {
  const value = settingShareServerKey.checked;
  try {
    const s = await api("/api/settings", {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify({ share_server_key: value }),
    });
    settingShareServerKey.checked = !!(s && s.share_server_key);
    await loadKeyStatus();
    toast(value ? "Server key shared with users" : "Server key not shared");
  } catch (err) {
    if (err instanceof AuthError) return;
    settingShareServerKey.checked = !value;
    toast(isForbidden(err) ? "Admin access required" : err.message);
  }
}

/* ============================== Calendar (feature 3.3) ============================== */

/* Default CalDAV endpoints per provider preset; discovery refines them. */
const CALENDAR_ENDPOINTS = {
  apple: "https://caldav.icloud.com",
  zoho: "https://calendar.zoho.com",
  custom: "",
};

function updateCalendarFieldVisibility() {
  const kind = calendarKind ? calendarKind.value : "caldav";
  calendarCaldavFields.hidden = kind !== "caldav";
  calendarICSFields.hidden = kind !== "ics";
}

function applyCalendarProviderPreset() {
  const preset = CALENDAR_ENDPOINTS[calendarProvider.value];
  if (preset && !calendarEndpoint.value.trim()) calendarEndpoint.value = preset;
}

function showCalendarError(msg) {
  calendarError.textContent = msg;
  calendarError.hidden = false;
}

/* Render the connected state (never shows the secret). */
function applyCalendarStatus(s) {
  if (s && s.connected) {
    const kindLabel = s.kind === "ics" ? "Google ICS (read-only)" : "CalDAV";
    const who = s.username ? " as " + s.username : "";
    calendarStatus.textContent =
      "Connected: " + (s.provider ? s.provider + " " : "") + kindLabel + who +
      (s.can_write ? " — read/write." : " — read-only.");
    calendarDisconnect.hidden = false;
    calendarKind.value = s.kind === "ics" ? "ics" : "caldav";
    if (s.provider) calendarProvider.value = s.provider;
    if (s.kind === "caldav") {
      if (s.endpoint) calendarEndpoint.value = s.endpoint;
      if (s.username) calendarUsername.value = s.username;
    }
    calendarSave.textContent = "Update";
  } else {
    calendarStatus.textContent = "Not connected. The assistant cannot read your calendar.";
    calendarDisconnect.hidden = true;
    calendarSave.textContent = "Connect";
  }
  updateCalendarFieldVisibility();
}

async function loadCalendarStatus() {
  try {
    const s = await api("/api/me/calendar");
    applyCalendarStatus(s);
  } catch (err) {
    if (err instanceof AuthError) return;
    calendarStatus.textContent = "Could not load calendar status.";
  }
}

async function saveCalendar() {
  calendarError.hidden = true;
  const kind = calendarKind.value;
  const body = { kind };
  if (kind === "caldav") {
    body.provider = calendarProvider.value;
    body.endpoint = calendarEndpoint.value.trim();
    body.username = calendarUsername.value.trim();
    body.password = calendarPassword.value;
    if (!body.endpoint) return showCalendarError("Enter the CalDAV server URL.");
    if (!body.username) return showCalendarError("Enter your username (Apple ID / Zoho email).");
    if (!body.password) return showCalendarError("Enter your app-specific password.");
  } else {
    body.provider = "google";
    body.url = calendarURL.value.trim();
    if (!body.url) return showCalendarError("Enter the secret ICS URL.");
  }
  const original = calendarSave.textContent;
  calendarSave.disabled = true;
  calendarSave.textContent = "Connecting…";
  try {
    const s = await api("/api/me/calendar", {
      method: "PUT",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    calendarPassword.value = "";
    calendarURL.value = "";
    applyCalendarStatus(s);
    toast("Calendar connected");
  } catch (err) {
    if (err instanceof AuthError) return;
    calendarSave.textContent = original;
    showCalendarError(err.message);
  } finally {
    calendarSave.disabled = false;
  }
}

async function disconnectCalendar() {
  if (!confirm("Disconnect this calendar?")) return;
  try {
    await api("/api/me/calendar", { method: "DELETE" });
    calendarEndpoint.value = "";
    calendarUsername.value = "";
    calendarPassword.value = "";
    calendarURL.value = "";
    applyCalendarStatus({ connected: false });
    toast("Calendar disconnected");
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(err.message);
  }
}

/* ============================== Groups & shared keys (§2.2) ============================== */

function groupById(id) {
  return state.groups.find((g) => g.id === id) || null;
}

async function loadGroups() {
  try {
    const groups = await api("/api/groups");
    state.groups = Array.isArray(groups) ? groups : [];
    renderGroups();
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(err.message);
  }
}

function renderGroups() {
  groupsList.innerHTML = "";
  groupsEmpty.hidden = state.groups.length !== 0;
  for (const g of state.groups) groupsList.appendChild(buildGroupCard(g));
}

function buildGroupCard(g) {
  const isAdmin = !!(state.user && state.user.is_admin);
  const li = el("li", "group-item");

  const head = el("div", "group-head");
  const name = el("span", "group-name");
  name.textContent = g.name;
  head.appendChild(name);
  if (g.is_owner) {
    const ownerBadge = el("span", "badge");
    ownerBadge.textContent = "owner";
    head.appendChild(ownerBadge);
  }
  const shareBadge = el("span", "badge" + (g.share_key ? "" : " group-share-off"));
  shareBadge.textContent = g.share_key ? "sharing key" : "not sharing";
  head.appendChild(shareBadge);
  li.appendChild(head);

  const members = el("ul", "group-members");
  const list = g.members || [];
  for (const m of list) {
    const row = el("li", "group-member");
    const info = el("span", "group-member-info");
    info.textContent = m.username + (m.role && m.role !== "member" ? " (" + m.role + ")" : "");
    info.title = m.email || "";
    row.appendChild(info);
    if (g.is_owner || isAdmin) {
      const rm = el("button", "icon-btn group-member-remove");
      rm.type = "button";
      rm.innerHTML = ICONS.trash;
      rm.setAttribute("aria-label", "Remove " + m.username + " from " + g.name);
      rm.addEventListener("click", () => removeGroupMember(g, m));
      row.appendChild(rm);
    }
    members.appendChild(row);
  }
  if (list.length === 0) {
    const none = el("li", "group-member group-member-empty");
    none.textContent = "No members yet.";
    members.appendChild(none);
  }
  li.appendChild(members);

  if (g.is_owner) {
    const controls = el("div", "group-controls");

    const shareToggle = el("label", "setting-toggle group-share-toggle");
    const shareLabel = el("span");
    shareLabel.textContent = "Share my key with this group";
    const shareBox = document.createElement("input");
    shareBox.type = "checkbox";
    shareBox.checked = !!g.share_key;
    shareBox.addEventListener("change", () => patchGroup(g.id, { share_key: shareBox.checked }));
    shareToggle.append(shareLabel, shareBox);
    controls.appendChild(shareToggle);

    const addForm = el("form", "group-add-form");
    addForm.autocomplete = "off";
    const addInput = el("input", "group-add-input");
    addInput.type = "text";
    addInput.placeholder = "username or email";
    addInput.setAttribute("aria-label", "Add member to " + g.name);
    const addBtn = el("button", "btn btn-ghost");
    addBtn.type = "submit";
    addBtn.textContent = "Add";
    addForm.append(addInput, addBtn);
    addForm.addEventListener("submit", (e) => {
      e.preventDefault();
      addGroupMember(g.id, addInput.value.trim());
    });
    controls.appendChild(addForm);

    const actions = el("div", "user-actions");
    actions.appendChild(userActionButton("Rename", () => renameGroup(g.id)));
    actions.appendChild(userActionButton("Delete", () => deleteGroup(g.id), true));
    controls.appendChild(actions);
    li.appendChild(controls);
  }
  return li;
}

function replaceGroup(updated) {
  if (!updated || !updated.id) return;
  const idx = state.groups.findIndex((g) => g.id === updated.id);
  if (idx !== -1) state.groups[idx] = updated;
  renderGroups();
}

async function createGroup(e) {
  if (e) e.preventDefault();
  setFormError(groupError, "");
  const name = groupName.value.trim();
  if (!name) {
    setFormError(groupError, "Enter a group name.");
    return;
  }
  try {
    const created = await api("/api/groups", {
      method: "POST",
      headers: JSON_HEADERS,
      body: JSON.stringify({ name: name.slice(0, 120), share_key: groupShareKey.checked }),
    });
    groupForm.reset();
    groupShareKey.checked = true;
    if (created && created.id) state.groups.push(created);
    renderGroups();
    await loadKeyStatus();
    toast("Group created");
  } catch (err) {
    if (err instanceof AuthError) return;
    setFormError(groupError, err.message);
  }
}

async function patchGroup(id, body) {
  try {
    const updated = await api("/api/groups/" + enc(id), {
      method: "PATCH",
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    });
    replaceGroup(updated);
    await loadKeyStatus();
  } catch (err) {
    if (err instanceof AuthError) return;
    toast(err.message);
    await loadGroups();
  }
}

async function renameGroup(id) {
  const g = groupById(id);
  if (!g) return;
  const name = window.prompt("Rename group", g.name || "");
  if (name === null || !name.trim()) return;
  await patchGroup(id, { name: name.trim().slice(0, 120) });
}

async function deleteGroup(id) {
  const g = groupById(id);
  if (!g) return;
  if (!window.confirm('Delete group "' + g.name + '"? Members will stop sharing keys.')) return;
  try {
    await api("/api/groups/" + enc(id), { method: "DELETE" });
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
    return;
  }
  state.groups = state.groups.filter((x) => x.id !== id);
  renderGroups();
  await loadKeyStatus();
  toast("Group deleted");
}

async function addGroupMember(id, identifier) {
  if (!identifier) return;
  try {
    const updated = await api("/api/groups/" + enc(id) + "/members", {
      method: "POST",
      headers: JSON_HEADERS,
      body: JSON.stringify({ identifier }),
    });
    replaceGroup(updated);
    toast("Member added");
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
  }
}

async function removeGroupMember(group, member) {
  if (!window.confirm('Remove "' + member.username + '" from ' + group.name + "?")) return;
  try {
    await api("/api/groups/" + enc(group.id) + "/members/" + enc(member.user_id), {
      method: "DELETE",
    });
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
    return;
  }
  await loadGroups();
  await loadKeyStatus();
  toast("Member removed");
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
  if (settingsPage.hidden) input.focus({ preventScroll: true });
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

/* File-type glyph for an attachment chip, from MIME then extension. */
function fileGlyph(name, mime) {
  const m = (mime || "").toLowerCase();
  if (m.startsWith("image/")) return "🖼️";
  if (m.startsWith("audio/")) return "🎵";
  if (m.startsWith("video/")) return "🎬";
  if (m === "application/pdf") return "📕";
  const ext = (name || "").toLowerCase().split(".").pop();
  switch (ext) {
    case "png": case "jpg": case "jpeg": case "gif": case "webp": case "bmp":
      case "tif": case "tiff": case "svg": return "🖼️";
    case "mp3": case "wav": case "m4a": case "ogg": case "flac": case "opus": case "aac":
      return "🎵";
    case "mp4": case "mov": case "webm": case "mkv": case "avi": return "🎬";
    case "pdf": return "📕";
    case "doc": case "docx": case "odt": case "rtf": case "txt": case "md": return "📄";
    case "xls": case "xlsx": case "ods": case "csv": case "tsv": return "📊";
    case "ppt": case "pptx": case "odp": return "📈";
    case "zip": case "rar": case "7z": case "tar": case "gz": return "🗜️";
    case "json": case "xml": case "yaml": case "yml": case "toml":
      case "go": case "py": case "js": case "ts": case "java": case "rs": case "c":
      case "cpp": case "h": case "sh": case "sql": case "html": case "css":
      return "🧩";
    default: return "📎";
  }
}

/* A read-only chip for a file attached to a sent message (metadata only). */
function attachmentChip(a) {
  const chip = el("span", "attachment-chip");
  chip.title = (a.mime ? a.mime + " · " : "") + (a.name || "file");
  const icon = el("span", "attachment-chip-icon");
  icon.textContent = fileGlyph(a.name, a.mime);
  const name = el("span", "attachment-chip-name");
  name.textContent = a.name || "file";
  chip.append(icon, name);
  const size = fmtBytes(a.size);
  if (size) {
    const s = el("span", "attachment-chip-size");
    s.textContent = size;
    chip.appendChild(s);
  }
  return chip;
}

function buildUserMessage(m) {
  const wrap = el("div", "msg msg-user");
  const bubble = el("div", "msg-bubble");
  bubble.textContent = m.content;
  const atts = Array.isArray(m.attachments) ? m.attachments : [];
  if (atts.length > 0) {
    const row = el("div", "msg-attachments");
    for (const a of atts) row.appendChild(attachmentChip(a));
    bubble.appendChild(row);
  }
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
  // Speak/stop: server TTS when configured, else the on-device Web Speech API.
  if (speechReady("tts") || window.speechSynthesis) {
    const speakId = m.id || m.role + ":" + (m.content || "").slice(0, 40);
    actions.appendChild(
      actionButton(currentSpeakId === speakId ? "Stop" : "Speak", (btn) => speakMessage(m, btn))
    );
  }
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
  if (Array.isArray(m.tools) && m.tools.length > 0) {
    bubble.appendChild(buildToolsBlock(m.tools));
  }
  const hasImages = Array.isArray(m.images) && m.images.length > 0;
  if (hasImages) bubble.appendChild(buildImagesBlock(m.images));
  const hasText = typeof m.content === "string" && m.content.trim() !== "";
  if (hasText) {
    const content = el("div", "msg-content markdown");
    content.innerHTML = renderMarkdown(m.content);
    enhanceMarkdown(content);
    bubble.appendChild(content);
  }
  // An image-only message has no cost/model footer (and no Retry, which would
  // replace the image with a text reply).
  if (hasText || !hasImages) bubble.appendChild(buildAssistantFooter(m, isLast));
  wrap.appendChild(bubble);
  return wrap;
}

/* Render generated images (feature 3.1) referenced by an assistant message.
   Bytes are served by GET /api/images/{id}; clicking opens the full size. */
function buildImagesBlock(images) {
  const wrap = el("div", "msg-images");
  for (const img of images) {
    if (!img || !img.id) continue;
    const link = document.createElement("a");
    link.href = "/api/images/" + enc(img.id);
    link.target = "_blank";
    link.rel = "noopener";
    const pic = document.createElement("img");
    pic.className = "msg-image";
    pic.src = "/api/images/" + enc(img.id);
    pic.alt = img.prompt || "Generated image";
    pic.loading = "lazy";
    link.appendChild(pic);
    wrap.appendChild(link);
  }
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
  updateAttachBtn();
  updateMicBtn();
  if (!on) input.focus({ preventScroll: true });
}

function updateSendBtn() {
  sendBtn.disabled = state.streaming || isUploading() || input.value.trim() === "";
}

function newStreamUI() {
  return {
    bubble: null,
    contentEl: null,
    thinkingEl: null,
    thinkingBody: null,
    toolsEl: null,
    tools: [],
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

/* ============================== Tool activity (Phase 2.4, collapsible 4.1) ============================== */

function toolIcon(status) {
  if (status === "done") return "🔧";
  if (status === "error") return "⚠️";
  return "⏳";
}

/* Pretty-print a tool's JSON arguments; fall back to the raw string. */
function fmtToolArgs(args) {
  if (!args) return "";
  const s = String(args);
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

/* One labelled, monospace block (Input / Output / Error). */
function toolSection(label, text, isError) {
  const sec = el("div", "msg-tool-section");
  const head = el("div", "msg-tool-label");
  head.textContent = label;
  const pre = el("pre", "msg-tool-pre" + (isError ? " msg-tool-pre-error" : ""));
  pre.textContent = text; // textContent → never inject markup (XSS-safe)
  sec.append(head, pre);
  return sec;
}

/* One tool call as a collapsible <details> with Input/Output sections. Errors
   auto-expand so a failure is visible without a tap. */
function buildToolItem(t) {
  const item = el("details", "msg-tool");
  if (t.status === "error") item.open = true;
  const summary = document.createElement("summary");
  summary.className = "msg-tool-summary";
  const dur = t.duration_ms ? " · " + fmtDuration(t.duration_ms) : "";
  summary.textContent =
    toolIcon(t.status) +
    " " +
    (t.name || "tool") +
    " · " +
    (t.status || "") +
    dur +
    (t.summary ? " · " + t.summary : "");
  item.appendChild(summary);

  const body = el("div", "msg-tool-body");
  const argsText = fmtToolArgs(t.args);
  if (argsText) body.appendChild(toolSection("Input", argsText));
  if (t.output) body.appendChild(toolSection("Output", t.output));
  if (t.error) body.appendChild(toolSection("Error", t.error, true));
  if (t.truncated) {
    const note = el("p", "msg-tool-note");
    note.textContent = "Truncated for display.";
    body.appendChild(note);
  }
  if (body.childElementCount > 0) item.appendChild(body);
  return item;
}

/* Collapsible wrapper for a message's tool calls. Shows a count/status line and
   auto-expands when any call errored. */
function buildToolsBlock(tools) {
  const wrap = el("details", "msg-tools");
  const done = tools.filter((t) => t.status === "done").length;
  const errs = tools.filter((t) => t.status === "error").length;
  const running = tools.filter((t) => t.status === "running").length;
  const parts = [];
  if (done) parts.push(done + " done");
  if (errs) parts.push(errs + " error" + (errs === 1 ? "" : "s"));
  if (running) parts.push(running + " running");
  const summary = document.createElement("summary");
  summary.className = "msg-tools-summary";
  summary.textContent =
    "🔧 " + tools.length + " tool" + (tools.length === 1 ? "" : "s") +
    (parts.length ? " · " + parts.join(", ") : "");
  wrap.appendChild(summary);

  const body = el("div", "msg-tools-body");
  for (const t of tools) body.appendChild(buildToolItem(t));
  wrap.appendChild(body);
  if (errs > 0) wrap.open = true;
  return wrap;
}

/* Upsert a tool activity entry from an SSE "tool" event and re-render. */
function updateToolActivity(ui, payload) {
  if (!ui.tools) ui.tools = [];
  const name = payload.name || "tool";
  const status = payload.status || "running";
  const entry = {
    name,
    status,
    summary: payload.summary || "",
    args: payload.args || "",
    output: payload.output || "",
    error: payload.error || "",
    duration_ms: payload.duration_ms || 0,
    truncated: !!payload.truncated,
  };
  if (status === "running") {
    ui.tools.push(entry);
  } else {
    let updated = false;
    for (let i = ui.tools.length - 1; i >= 0; i--) {
      if (ui.tools[i].name === name && ui.tools[i].status === "running") {
        ui.tools[i] = entry;
        updated = true;
        break;
      }
    }
    if (!updated) ui.tools.push(entry);
  }
  renderStreamTools(ui);
}

function renderStreamTools(ui) {
  if (!ui.tools || ui.tools.length === 0) return;
  ensureStreamBubble(ui);
  if (!ui.toolsEl) {
    ui.toolsEl = el("div", "msg-tools-slot");
    ui.bubble.insertBefore(ui.toolsEl, ui.bubble.firstChild);
  }
  ui.toolsEl.innerHTML = "";
  ui.toolsEl.appendChild(buildToolsBlock(ui.tools));
  scrollToBottom();
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
    case "tool":
      updateToolActivity(ui, payload);
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
    // Tool activity is persisted server-side (2.10) using the same
    // running→done collapse, so this optimistic copy converges with the
    // refresh below without a flicker.
    tools: ui.tools && ui.tools.length ? ui.tools.slice() : null,
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
  const attachments = state.pendingAttachments.filter((a) => a && a.id);
  const userMsg = {
    role: "user",
    content: text,
    cost_micros: null,
    attachments: attachments.map((a) => ({ id: a.id, name: a.name, mime: a.mime, size: a.size })),
  };
  state.currentMessages.push(userMsg);
  messagesEl.appendChild(buildUserMessage(userMsg));
  scrollToBottom();

  const snapshot = state.pendingAttachments.slice();
  clearPendingAttachments();

  const body = { content: text };
  if (attachments.length > 0) body.attachment_ids = attachments.map((a) => a.id);

  await streamAssistant({
    path: "/api/conversations/" + enc(conv.id) + "/messages",
    convId: conv.id,
    body,
    onFatal: () => {
      rollbackOptimisticUserMessage(text);
      // Restore the chips so the user can retry without re-uploading.
      state.pendingAttachments = snapshot;
      renderAttachments();
    },
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

/* ============================== Attachments (2.3) ============================== */

const MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024;
const MAX_ATTACHMENTS = 20;

function fmtBytes(n) {
  if (typeof n !== "number" || n < 0) return "";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(n < 10 * 1024 ? 1 : 0) + " KB";
  return (n / (1024 * 1024)).toFixed(1) + " MB";
}

function isUploading() {
  return state.pendingAttachments.some((a) => a && a.uploading);
}

function updateAttachBtn() {
  attachBtn.disabled =
    state.streaming ||
    isUploading() ||
    state.pendingAttachments.length >= MAX_ATTACHMENTS;
}

/* POST multipart/form-data to /api/attachments; returns the stored metadata. */
async function uploadAttachment(file) {
  const fd = new FormData();
  fd.append("file", file, file.name);
  const res = await fetch("/api/attachments", { method: "POST", body: fd });
  if (res.status === 401) {
    handleUnauthorized();
    throw new AuthError("unauthorized");
  }
  if (!res.ok) {
    throw new ApiError((await readError(res)) || "Upload failed (" + res.status + ")", res.status);
  }
  return res.json();
}

function renderAttachments() {
  const atts = state.pendingAttachments;
  attachmentsBar.innerHTML = "";
  attachmentsBar.hidden = atts.length === 0;
  for (const a of atts) {
    const chip = el("span", "attachment-chip" + (a.uploading ? " uploading" : ""));
    const name = el("span", "attachment-chip-name");
    name.textContent = a.name;
    name.title = a.name;
    chip.appendChild(name);
    const meta = el("span", "attachment-chip-size");
    if (a.uploading) {
      meta.textContent = "uploading…";
      chip.appendChild(meta);
    } else {
      meta.textContent = fmtBytes(a.size);
      chip.appendChild(meta);
      const remove = el("button", "attachment-remove");
      remove.type = "button";
      remove.setAttribute("aria-label", "Remove " + a.name);
      remove.textContent = "✕";
      remove.addEventListener("click", () => removeAttachment(a));
      chip.appendChild(remove);
    }
    attachmentsBar.appendChild(chip);
  }
  updateSendBtn();
  updateAttachBtn();
}

/* Removing a pending chip deletes the server copy to free storage. Attachments
   that were already sent are never removed here. */
function removeAttachment(a) {
  const i = state.pendingAttachments.indexOf(a);
  if (i !== -1) state.pendingAttachments.splice(i, 1);
  renderAttachments();
  if (a.id) {
    fetch("/api/attachments/" + enc(a.id), { method: "DELETE" }).catch(() => {});
  }
}

function clearPendingAttachments() {
  state.pendingAttachments = [];
  renderAttachments();
}

async function onFilesSelected() {
  const files = Array.from(fileInput.files || []);
  fileInput.value = "";
  if (files.length === 0 || state.streaming) return;

  const room = MAX_ATTACHMENTS - state.pendingAttachments.length;
  if (room <= 0) {
    toast("At most " + MAX_ATTACHMENTS + " files per message");
    return;
  }
  const chosen = files.slice(0, room);
  if (files.length > chosen.length) {
    toast("Only the first " + MAX_ATTACHMENTS + " files were added");
  }

  for (const file of chosen) {
    if (file.size > MAX_ATTACHMENT_BYTES) {
      toast(file.name + " is larger than 10 MB");
      continue;
    }
    const placeholder = { id: null, name: file.name, size: file.size, uploading: true };
    state.pendingAttachments.push(placeholder);
    renderAttachments();
    try {
      const meta = await uploadAttachment(file);
      const i = state.pendingAttachments.indexOf(placeholder);
      if (i !== -1) state.pendingAttachments[i] = meta;
    } catch (err) {
      const i = state.pendingAttachments.indexOf(placeholder);
      if (i !== -1) state.pendingAttachments.splice(i, 1);
      if (!(err instanceof AuthError)) toast(err.message);
    }
    renderAttachments();
  }
}

/* ============================== Voice: STT + TTS (3.5 / 3.6) ============================== */

const MAX_TTS_CHARS = 4000;
const ttsCache = new Map(); // message id -> Blob
let mediaRecorder = null;
let recordedChunks = [];
let micState = "idle"; // idle | recording | transcribing
let currentAudio = null; // <audio> currently playing
let currentSpeakBtn = null; // button to reset when playback ends
let currentSpeakId = null; // message id currently being spoken

/* speechReady reports whether the server has the feature configured + enabled. */
function speechReady(kind) {
  return kind === "stt"
    ? !!(state.speech.stt_available && state.speech.stt_enabled)
    : !!(state.speech.tts_available && state.speech.tts_enabled);
}

function canRecord() {
  return !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia && window.MediaRecorder);
}

/* Load availability for the composer buttons (any signed-in user). */
async function loadSpeechStatus() {
  try {
    const s = await api("/api/speech/status");
    state.speech = {
      stt_available: !!(s && s.stt_available),
      stt_enabled: !!(s && s.stt_enabled),
      tts_available: !!(s && s.tts_available),
      tts_enabled: !!(s && s.tts_enabled),
      tts_voice: (s && s.tts_voice) || "",
    };
  } catch {
    /* leave the defaults (off) */
  }
  updateMicBtn();
  // Re-render so the per-message speak buttons reflect availability.
  if (state.currentMessages.length) renderMessages();
}

function updateMicBtn() {
  if (!micBtn) return;
  micBtn.hidden = !speechReady("stt");
  micBtn.disabled = state.streaming || micState === "transcribing";
}

function setMicState(next) {
  micState = next;
  if (!micBtn) return;
  micBtn.classList.toggle("recording", next === "recording");
  micBtn.classList.toggle("transcribing", next === "transcribing");
  const label =
    next === "recording"
      ? "Stop recording"
      : next === "transcribing"
        ? "Transcribing…"
        : "Record voice message";
  micBtn.setAttribute("aria-label", label);
  micBtn.title = label;
  micBtn.disabled = state.streaming || next === "transcribing";
}

function toggleMic() {
  if (!speechReady("stt") || state.streaming) return;
  if (micState === "recording") {
    stopRecording();
    return;
  }
  if (micState === "transcribing") return;
  startRecording();
}

async function startRecording() {
  // No MediaRecorder (or a locked-down browser): fall back to a file picker.
  if (!canRecord()) {
    audioInput.click();
    return;
  }
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: true });
  } catch {
    toast("Microphone permission denied");
    return;
  }
  recordedChunks = [];
  try {
    mediaRecorder = new MediaRecorder(stream);
  } catch {
    stream.getTracks().forEach((t) => t.stop());
    audioInput.click();
    return;
  }
  mediaRecorder.addEventListener("dataavailable", (e) => {
    if (e.data && e.data.size > 0) recordedChunks.push(e.data);
  });
  mediaRecorder.addEventListener("stop", () => {
    stream.getTracks().forEach((t) => t.stop());
    const type = mediaRecorder && mediaRecorder.mimeType ? mediaRecorder.mimeType : "audio/webm";
    const blob = new Blob(recordedChunks, { type });
    recordedChunks = [];
    if (blob.size === 0) {
      setMicState("idle");
      return;
    }
    transcribeBlob(blob);
  });
  mediaRecorder.start();
  setMicState("recording");
}

function stopRecording() {
  if (mediaRecorder && mediaRecorder.state !== "inactive") {
    setMicState("transcribing");
    mediaRecorder.stop();
  } else {
    setMicState("idle");
  }
}

function audioExtension(type) {
  const t = String(type || "").toLowerCase();
  if (t.includes("mp4") || t.includes("aac") || t.includes("m4a")) return "m4a";
  if (t.includes("ogg")) return "ogg";
  if (t.includes("wav")) return "wav";
  if (t.includes("mpeg") || t.includes("mp3")) return "mp3";
  return "webm";
}

async function transcribeBlob(blob, name) {
  if (!speechReady("stt")) {
    setMicState("idle");
    return;
  }
  setMicState("transcribing");
  try {
    const fd = new FormData();
    fd.append("audio", blob, name || "recording." + audioExtension(blob.type));
    const res = await fetch("/api/stt", { method: "POST", body: fd });
    if (res.status === 401) {
      handleUnauthorized();
      throw new AuthError("unauthorized");
    }
    if (!res.ok) {
      throw new ApiError(
        (await readError(res)) || "Transcription failed (" + res.status + ")",
        res.status
      );
    }
    const data = await res.json();
    insertTranscript(data && data.text ? data.text : "");
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
  } finally {
    setMicState("idle");
  }
}

/* Append the transcript to the composer (never clobber what is typed). */
function insertTranscript(text) {
  text = String(text || "").trim();
  if (!text) return;
  const cur = input.value;
  const sep = cur && !/\s$/.test(cur) ? " " : "";
  input.value = cur + sep + text;
  autosizeInput();
  updateSendBtn();
  input.focus();
}

function onAudioFileSelected() {
  const file = audioInput.files && audioInput.files[0];
  audioInput.value = "";
  if (!file) return;
  transcribeBlob(file, file.name);
}

/* Strip markdown/code so Piper reads prose, not syntax. */
function plainTextForSpeech(md) {
  let s = String(md || "");
  s = s.replace(/```[\s\S]*?```/g, " ");
  s = s.replace(/~~~[\s\S]*?~~~/g, " ");
  s = s.replace(/`([^`]*)`/g, "$1");
  s = s.replace(/!\[[^\]]*\]\([^)]*\)/g, " ");
  s = s.replace(/\[([^\]]*)\]\([^)]*\)/g, "$1");
  s = s.replace(/^\s{0,3}#{1,6}\s+/gm, "");
  s = s.replace(/^\s{0,3}>\s?/gm, "");
  s = s.replace(/^\s*[-*+]\s+/gm, "");
  s = s.replace(/^\s*\d+\.\s+/gm, "");
  s = s.replace(/^\s*\|.*\|\s*$/gm, (line) => line.replace(/\|/g, " "));
  s = s.replace(/[*_~]{1,3}/g, "");
  s = s.replace(/[ \t]+/g, " ");
  s = s.replace(/\n{3,}/g, "\n\n");
  return s.trim().slice(0, MAX_TTS_CHARS);
}

function stopSpeaking() {
  if (currentAudio) {
    try {
      currentAudio.pause();
    } catch {
      /* ignore */
    }
    currentAudio = null;
  }
  if (window.speechSynthesis) {
    try {
      window.speechSynthesis.cancel();
    } catch {
      /* ignore */
    }
  }
  if (currentSpeakBtn) currentSpeakBtn.textContent = "Speak";
  currentSpeakBtn = null;
  currentSpeakId = null;
}

/* On-device fallback (Web Speech API) when no server TTS is configured. */
function speakWithBrowser(text) {
  if (!window.speechSynthesis) {
    toast("Text-to-speech is not available");
    return;
  }
  const utter = new SpeechSynthesisUtterance(text);
  utter.onend = () => stopSpeaking();
  utter.onerror = () => stopSpeaking();
  window.speechSynthesis.speak(utter);
}

async function speakMessage(m, btn) {
  const id = m.id || m.role + ":" + (m.content || "").slice(0, 40);
  if (currentSpeakId === id) {
    stopSpeaking();
    return;
  }
  stopSpeaking();
  const text = plainTextForSpeech(m.content);
  if (!text) {
    toast("Nothing to read aloud");
    return;
  }
  if (!speechReady("tts")) {
    if (window.speechSynthesis) {
      currentSpeakId = id;
      currentSpeakBtn = btn;
      btn.textContent = "Stop";
      speakWithBrowser(text);
    } else {
      toast("Text-to-speech is not available");
    }
    return;
  }
  btn.textContent = "…";
  try {
    let blob = ttsCache.get(id);
    if (!blob) {
      const res = await fetch("/api/tts", {
        method: "POST",
        headers: JSON_HEADERS,
        body: JSON.stringify({ text }),
      });
      if (res.status === 401) {
        handleUnauthorized();
        throw new AuthError("unauthorized");
      }
      if (!res.ok) {
        throw new ApiError((await readError(res)) || "Speech failed (" + res.status + ")", res.status);
      }
      blob = await res.blob();
      ttsCache.set(id, blob);
    }
    playSpeechBlob(id, blob, btn);
  } catch (err) {
    btn.textContent = "Speak";
    if (!(err instanceof AuthError)) toast(err.message);
  }
}

function playSpeechBlob(id, blob, btn) {
  const url = URL.createObjectURL(blob);
  const audio = new Audio(url);
  currentAudio = audio;
  currentSpeakId = id;
  currentSpeakBtn = btn;
  btn.textContent = "Stop";
  const done = () => {
    URL.revokeObjectURL(url);
    if (currentAudio === audio) {
      currentAudio = null;
      currentSpeakId = null;
      if (currentSpeakBtn === btn) {
        btn.textContent = "Speak";
        currentSpeakBtn = null;
      }
    }
  };
  audio.addEventListener("ended", done);
  audio.addEventListener("error", () => {
    done();
    toast("Could not play audio");
  });
  audio.play().catch(() => {
    done();
    toast("Could not play audio");
  });
}

/* ============================== Image generation (3.1) ============================== */

/* Show/hide the inline image-prompt bar; prefill it from the composer text. */
function setImageBar(open) {
  if (!imageBar) return;
  imageBar.hidden = !open;
  if (imageBtn) imageBtn.classList.toggle("active", open);
  if (open && imagePrompt) {
    if (!imagePrompt.value && input.value.trim()) imagePrompt.value = input.value.trim();
    imagePrompt.focus({ preventScroll: true });
  }
}

/* ===== Prompt enhancement + preview (feature 4.3) ===== */
let imageGenConv = null;

/* Open the preview modal: ensure a conversation, enhance the brief, and let the
   user edit the resulting prompt before generating. */
async function openImageGen(brief) {
  brief = (brief || "").trim();
  if (!brief) {
    toast("Enter an image prompt");
    return;
  }
  if (state.streaming) {
    toast("Wait for the current reply to finish");
    return;
  }
  try {
    imageGenConv = await ensureConversation();
  } catch (err) {
    if (!(err instanceof AuthError)) toast(err.message);
    return;
  }
  setImageBar(false);
  if (imageModalPrompt) imageModalPrompt.value = brief;
  setFormError(imageModalError, "");
  openModal(imageModal);
  await enhanceImagePrompt(brief);
}

/* Ask the server to rewrite a brief into a rich diffusion prompt (4.3). */
async function enhanceImagePrompt(brief) {
  if (!imageModalPrompt) return;
  const b = (brief || imageModalPrompt.value || "").trim();
  if (!b) {
    setFormError(imageModalError, "Enter an image prompt.");
    return;
  }
  if (imageModalNote) imageModalNote.textContent = "Enhancing the prompt for the image model…";
  setFormError(imageModalError, "");
  if (imageModalGenerate) imageModalGenerate.disabled = true;
  if (imageModalReenhance) imageModalReenhance.disabled = true;
  try {
    const res = await api("/api/images/enhance", {
      method: "POST",
      headers: JSON_HEADERS,
      body: JSON.stringify({
        brief: b,
        conversation_id: imageGenConv ? imageGenConv.id : "",
      }),
    });
    if (res && res.prompt) imageModalPrompt.value = res.prompt;
    if (imageModalNote) {
      imageModalNote.textContent =
        res && res.enhanced
          ? "Prompt enhanced for the image model — edit it if you like, then generate."
          : "Using a basic prompt (enhancer unavailable) — edit it if you like, then generate.";
    }
  } catch (err) {
    if (err instanceof AuthError) return;
    setFormError(imageModalError, err.message);
    if (imageModalNote) {
      imageModalNote.textContent =
        "Could not enhance the prompt — edit it and generate anyway.";
    }
  } finally {
    if (imageModalGenerate) imageModalGenerate.disabled = false;
    if (imageModalReenhance) imageModalReenhance.disabled = false;
  }
}

/* Generate an image into the open conversation. The server persists the prompt
   and the resulting image as messages, so a refresh renders them. */
async function runImageGeneration(prompt) {
  prompt = (prompt || "").trim();
  if (!prompt) {
    toast("Enter an image prompt");
    return;
  }
  if (state.streaming) {
    toast("Wait for the current reply to finish");
    return;
  }

  let conv = imageGenConv;
  if (!conv) {
    try {
      conv = await ensureConversation();
    } catch (err) {
      if (!(err instanceof AuthError)) toast(err.message);
      return;
    }
  }
  imageGenConv = null;

  setImageBar(false);
  if (imageGenerate) imageGenerate.disabled = true;
  // Optimistic placeholder bubble (replaced by the persisted messages below).
  const wrap = el("div", "msg msg-assistant");
  const bubble = el("div", "msg-bubble");
  const note = el("div", "msg-content image-generating");
  note.textContent = "🎨 Generating image…";
  bubble.appendChild(note);
  wrap.appendChild(bubble);
  messagesEl.appendChild(wrap);
  scrollToBottom();

  try {
    await api("/api/images", {
      method: "POST",
      headers: JSON_HEADERS,
      body: JSON.stringify({ prompt: prompt, conversation_id: conv.id }),
    });
    if (state.current && state.current.id === conv.id) {
      await refreshCurrentConversation();
    }
  } catch (err) {
    wrap.remove();
    if (!(err instanceof AuthError)) toast(err.message || "Image generation failed");
  } finally {
    if (imageGenerate) imageGenerate.disabled = false;
    if (imagePrompt) imagePrompt.value = "";
  }
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
  /* Login: identifier (username or email) + password. */
  loginForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    setFormError(loginError, "");
    loginBtn.disabled = true;
    try {
      const res = await fetch("/api/auth/login", {
        method: "POST",
        headers: JSON_HEADERS,
        body: JSON.stringify({
          identifier: loginIdentifier.value.trim(),
          password: loginPassword.value,
        }),
      });
      if (res.ok) {
        loginPassword.value = "";
        await afterAuth();
      } else if (res.status === 403) {
        // §9.3: pending approval or disabled account.
        const message = await readError(res);
        if (/approval|pending/i.test(message)) {
          setFormError(loginError, "Your account is awaiting admin approval.");
        } else if (/disabled/i.test(message)) {
          setFormError(loginError, "This account has been disabled.");
        } else {
          setFormError(loginError, message || "Access denied (403).");
        }
      } else if (res.status === 401) {
        setFormError(loginError, "Incorrect username/email or password.");
      } else {
        setFormError(loginError, (await readError(res)) || "Login failed (" + res.status + ").");
      }
    } catch {
      setFormError(loginError, "Cannot reach the server.");
    } finally {
      loginBtn.disabled = false;
    }
  });

  /* First-run admin creation. */
  setupForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    setFormError(setupError, "");
    if (setupPassword.value !== setupConfirm.value) {
      setFormError(setupError, "Passwords do not match.");
      return;
    }
    if (setupPassword.value.length < 8) {
      setFormError(setupError, "Password must be at least 8 characters.");
      return;
    }
    setupBtn.disabled = true;
    try {
      const res = await fetch("/api/auth/setup", {
        method: "POST",
        headers: JSON_HEADERS,
        body: JSON.stringify({
          username: setupUsername.value.trim(),
          email: setupEmail.value.trim(),
          password: setupPassword.value,
        }),
      });
      if (res.ok) {
        setupForm.reset();
        await afterAuth();
      } else if (res.status === 409) {
        setFormError(
          setupError,
          (await readError(res)) || "That username or email is already taken."
        );
      } else if (res.status === 403) {
        setFormError(setupError, "Setup is no longer available — an account already exists.");
      } else {
        setFormError(setupError, (await readError(res)) || "Setup failed (" + res.status + ").");
      }
    } catch {
      setFormError(setupError, "Cannot reach the server.");
    } finally {
      setupBtn.disabled = false;
    }
  });

  /* Self-registration. */
  registerForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    setFormError(registerError, "");
    if (registerPassword.value !== registerConfirm.value) {
      setFormError(registerError, "Passwords do not match.");
      return;
    }
    if (registerPassword.value.length < 8) {
      setFormError(registerError, "Password must be at least 8 characters.");
      return;
    }
    registerBtn.disabled = true;
    try {
      const res = await fetch("/api/auth/register", {
        method: "POST",
        headers: JSON_HEADERS,
        body: JSON.stringify({
          username: registerUsername.value.trim(),
          email: registerEmail.value.trim(),
          password: registerPassword.value,
        }),
      });
      if (res.status === 202) {
        // Self-registration is now pending admin approval; no session is set.
        let message = "Your account is awaiting admin approval.";
        try {
          const j = await res.json();
          if (j && j.message) message = j.message;
        } catch {
          /* keep default */
        }
        registerForm.reset();
        showRegisterPending(message);
      } else if (res.ok) {
        registerForm.reset();
        await afterAuth();
      } else if (res.status === 403) {
        setFormError(registerError, "Registration is currently closed.");
      } else if (res.status === 409) {
        setFormError(
          registerError,
          (await readError(res)) || "That username or email is already taken."
        );
      } else {
        setFormError(
          registerError,
          (await readError(res)) || "Registration failed (" + res.status + ")."
        );
      }
    } catch {
      setFormError(registerError, "Cannot reach the server.");
    } finally {
      registerBtn.disabled = false;
    }
  });

  showRegisterBtn.addEventListener("click", showRegister);
  showLoginBtn.addEventListener("click", () => showLogin());

  logoutBtn.addEventListener("click", doLogout);
  accountLogout.addEventListener("click", doLogout);

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
    if (sidebar.classList.contains("open")) {
      closeSidebar();
      return;
    }
    if (isSettingsRoute()) leaveSettings();
  });

  /* Settings page (route #/settings). */
  settingsBtn.addEventListener("click", navigateToSettings);
  settingsBack.addEventListener("click", leaveSettings);
  settingsPage.querySelectorAll('input[name="theme"]').forEach((r) => {
    r.addEventListener("change", () => {
      if (!r.checked) return;
      settings.theme = r.value;
      lsSet("myogchat.theme", r.value);
      applyTheme();
    });
  });
  settingsPage.querySelectorAll('input[name="thinking"]').forEach((r) => {
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

  /* Admin: users panel (contract §9.6). */
  addUserForm.addEventListener("submit", (e) => {
    e.preventDefault();
    addUser();
  });
  settingAllowRegistration.addEventListener("change", saveRegistrationSetting);

  /* OpenCode Go key (BYOK, §2.1). */
  keyForm.addEventListener("submit", saveKey);
  keyRemove.addEventListener("click", removeKey);
  settingShareServerKey.addEventListener("change", saveShareServerKey);

  /* Groups & shared keys (§2.2). */
  groupForm.addEventListener("submit", createGroup);

  /* Calendar (feature 3.3). */
  if (calendarKind) calendarKind.addEventListener("change", updateCalendarFieldVisibility);
  if (calendarProvider) calendarProvider.addEventListener("change", applyCalendarProviderPreset);
  if (calendarSave) calendarSave.addEventListener("click", saveCalendar);
  if (calendarDisconnect) calendarDisconnect.addEventListener("click", disconnectCalendar);

  /* Integrations: tools toggle + Linkwarden (admin, Phase 2.4 / 2.8). */
  settingToolsEnabled.addEventListener("change", saveToolsEnabled);
  linkwardenForm.addEventListener("submit", saveLinkwarden);
  linkwardenClear.addEventListener("click", clearLinkwarden);

  /* Image generation settings block (admin, feature 3.1). */
  if (imagegenSave) imagegenSave.addEventListener("click", saveImagegen);
  if (imagegenClear) imagegenClear.addEventListener("click", clearImagegen);

  /* Web search block (self-contained; safe to relocate). */
  if (searchSave) searchSave.addEventListener("click", saveSearch);
  if (searchClearKey) searchClearKey.addEventListener("click", clearBraveKey);

  /* Maps block (self-contained, 3.4; safe to relocate). */
  if (mapsSave) mapsSave.addEventListener("click", saveMaps);

  /* ==== BEGIN per-user web search (2.12) — self-contained, safe to relocate ==== */
  if (mySerperForm) mySerperForm.addEventListener("submit", (e) => {
    e.preventDefault();
    saveMySerper();
  });
  if (mySerperClear) mySerperClear.addEventListener("click", clearMySerper);
  /* ==== END per-user web search ==== */

  /* ==== BEGIN code-sandbox tools (2.6/2.7) — self-contained, safe to relocate ==== */
  if (settingSandboxEnabled) settingSandboxEnabled.addEventListener("change", saveSandboxEnabled);
  /* ==== END code-sandbox tools ==== */

  /* Image OCR toggle (admin, feature 2.5). */
  settingOcrEnabled.addEventListener("change", saveOcrEnabled);

  /* ===== Usage & quotas (2.11) — self-contained, safe to relocate ===== */
  if (usageRange) {
    usageRange.addEventListener("change", () => {
      loadUsage().catch(() => {});
      if (state.user && state.user.is_admin) loadAllUsersUsage().catch(() => {});
    });
  }
  if (usageSave) usageSave.addEventListener("click", saveUsageLimits);
  /* ===== end Usage & quotas (2.11) ===== */

  /* Voice / speech (features 3.5 / 3.6). */
  if (voiceSave) voiceSave.addEventListener("click", saveVoiceSettings);

  /* ==== BEGIN email (read-only IMAP, feature 3.2) — self-contained, safe to relocate ==== */
  if (emailProvider) emailProvider.addEventListener("change", updateEmailCustomFields);
  if (emailForm) emailForm.addEventListener("submit", saveMail);
  if (emailTest) emailTest.addEventListener("click", testMail);
  if (emailDisconnect) emailDisconnect.addEventListener("click", disconnectMail);
  if (settingEmailEnabled) settingEmailEnabled.addEventListener("change", toggleEmailEnabled);
  /* ==== END email ==== */

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
    if (state.streaming || isUploading()) return;
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

  /* Attachments: paperclip opens the picker, chips render above the composer. */
  attachBtn.addEventListener("click", () => {
    if (!attachBtn.disabled) fileInput.click();
  });
  fileInput.addEventListener("change", onFilesSelected);

  /* Voice: mic toggles MediaRecorder; the hidden input is the no-MediaRecorder
     fallback (and accepts an uploaded audio file). */
  if (micBtn) micBtn.addEventListener("click", toggleMic);
  if (audioInput) audioInput.addEventListener("change", onAudioFileSelected);

  /* Image generation (feature 3.1/4.3): the image button toggles the prompt bar;
     Generate opens the enhance + preview modal. */
  if (imageBtn) imageBtn.addEventListener("click", () => setImageBar(imageBar.hidden));
  if (imageCancel) imageCancel.addEventListener("click", () => setImageBar(false));
  if (imageGenerate) imageGenerate.addEventListener("click", () => openImageGen(imagePrompt.value));
  if (imagePrompt) {
    imagePrompt.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.isComposing) {
        e.preventDefault();
        openImageGen(imagePrompt.value);
      }
    });
  }
  if (imageModalClose) imageModalClose.addEventListener("click", () => closeModal(imageModal));
  if (imageModalReenhance) {
    imageModalReenhance.addEventListener("click", () => enhanceImagePrompt(imageModalPrompt.value));
  }
  if (imageModalGenerate) {
    imageModalGenerate.addEventListener("click", () => {
      const p = imageModalPrompt.value.trim();
      if (!p) {
        setFormError(imageModalError, "Enter an image prompt.");
        return;
      }
      closeModal(imageModal);
      runImageGeneration(p);
    });
  }
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

/* ============================== Visual viewport (mobile keyboard) ============================== */

// Pin the app to the *visual* viewport so the composer stays above the
// on-screen keyboard. Android Chrome honours
// interactive-widget=resizes-content (the layout viewport shrinks); this also
// covers iOS Safari, which does not support that hint.
function syncViewport() {
  const root = document.documentElement;
  const vv = window.visualViewport;
  if (!vv) {
    root.style.removeProperty("--app-h");
    root.style.removeProperty("--app-top");
    return;
  }
  root.style.setProperty("--app-h", vv.height + "px");
  root.style.setProperty("--app-top", vv.offsetTop + "px");
}

function wireViewport() {
  syncViewport();
  const vv = window.visualViewport;
  if (vv) {
    vv.addEventListener("resize", syncViewport);
    vv.addEventListener("scroll", syncViewport);
  }
  window.addEventListener("resize", syncViewport);
  window.addEventListener("orientationchange", () => setTimeout(syncViewport, 200));
}

/* ============================== Init ============================== */

loadSettings();
applyTheme();
applySidebarCollapsed();
applyEnterHint();
applySettingsToUI();
if (appVersionEl) appVersionEl.textContent = APP_VERSION;
configureMarked();
wireEvents();
wireViewport();
registerSW();
/* Route changes from the URL bar or the browser/Android back button. */
window.addEventListener("hashchange", () => {
  if (!isSettingsRoute()) settingsPushed = false;
  applyRoute();
});
updateSendBtn();
updateAttachBtn();
updateNewChatLabel();
renderUser();
boot();
