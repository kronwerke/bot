// The whitelist app. Inside Discord it signs in through the Embedded App SDK; on the
// website through Discord's OAuth page. Every address is relative: in Discord the page
// lives on discordsays.com, which forwards to kronwerke.com/whitelist.

const $ = (id) => document.getElementById(id);
const params = new URLSearchParams(location.search);
const inDiscord = params.has("frame_id");

let session = "";
let me = null;

// ---- small helpers ----

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

// the bot's messages use **bold** and `code`, as on Discord
function rich(s) {
  return esc(s).replace(/\*\*(.+?)\*\*/g, "<b>$1</b>").replace(/`(.+?)`/g, "<code>$1</code>");
}

function face(name, size) {
  const img = document.createElement("img");
  img.className = "face";
  img.alt = "";
  img.width = size || 36;
  img.height = size || 36;
  img.loading = "lazy";
  img.src = "head/" + encodeURIComponent(name || "-");
  return img;
}

function el(tag, cls, html) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (html != null) e.innerHTML = html;
  return e;
}

let toastTimer = 0;
function toast(msg, kind) {
  const t = $("toast");
  t.innerHTML = rich(msg);
  t.className = "toast " + (kind || "");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, kind === "bad" ? 7000 : 4000);
}

function store(key, value) {
  try {
    if (value === undefined) return sessionStorage.getItem(key) || "";
    if (value === null) sessionStorage.removeItem(key);
    else sessionStorage.setItem(key, value);
  } catch (_) { /* no storage in this context */ }
  return "";
}

async function api(path, body) {
  const opts = { headers: { Authorization: "Bearer " + session } };
  if (body !== undefined) {
    opts.method = "POST";
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch("api/" + path, opts);
  let data = {};
  try { data = await res.json(); } catch (_) { /* not JSON */ }
  if (res.status === 401) {
    session = "";
    store("kw_wl", null);
    await signIn(true);
    throw new Error("again");
  }
  if (!res.ok) throw Object.assign(new Error(data.error || "Das ging nicht."), { shown: true });
  return data;
}

function showOnly(...ids) {
  for (const id of ["loading", "login", "status", "add", "players", "more"]) $(id).hidden = !ids.includes(id);
}

// ---- signing in ----

let sdk = null;

async function signIn(again) {
  showOnly("loading");
  if (inDiscord) {
    $("loading-text").textContent = "Anmelden bei Discord …";
    const cfg = await (await fetch("api/config")).json();
    if (!cfg.ready) {
      $("loading-text").textContent = "Die Whitelist-App ist noch nicht fertig eingerichtet. Bis dahin geht alles mit /whitelist.";
      document.querySelector(".spinner")?.remove();
      return;
    }
    if (!sdk) {
      const { DiscordSDK } = await import("./sdk.js");
      sdk = new DiscordSDK(cfg.client_id);
      await sdk.ready();
    }
    const { code } = await sdk.commands.authorize({
      client_id: cfg.client_id, response_type: "code", state: "", prompt: "none", scope: ["identify"],
    });
    const res = await fetch("api/token", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ code }) });
    const data = await res.json();
    if (!res.ok) throw Object.assign(new Error(data.error || "Die Anmeldung ging nicht."), { shown: true });
    await sdk.commands.authenticate({ access_token: data.access_token });
    session = data.session;
  } else {
    const hash = new URLSearchParams(location.hash.slice(1));
    if (hash.get("s")) {
      session = hash.get("s");
      store("kw_wl", session);
      history.replaceState(null, "", location.pathname + location.search);
    } else if (!again) {
      session = store("kw_wl");
    }
    if (!session) {
      const why = hash.get("fehler");
      if (why) {
        $("login-error").textContent = why === "abgebrochen" ? "Die Anmeldung wurde abgebrochen." : "Die Anmeldung ging nicht. Versuch es noch einmal.";
        $("login-error").hidden = false;
        history.replaceState(null, "", location.pathname + location.search);
      }
      showOnly("login");
      document.getElementById("app").removeAttribute("aria-busy");
      return;
    }
  }
  await load();
}

async function load() {
  try {
    me = await api("me");
  } catch (e) {
    if (e.message === "again") return;
    showOnly("login");
    $("login").querySelector("h2").textContent = "Kein Zugriff";
    $("login").querySelector("p").innerHTML = rich(e.message);
    $("login").querySelector("a").hidden = inDiscord;
    return;
  }
  document.getElementById("app").removeAttribute("aria-busy");
  render();
}

// ---- drawing ----

function render() {
  const who = $("who");
  who.hidden = false;
  who.innerHTML = esc(me.name);
  if (!inDiscord) {
    const out = el("button", "btn link", "Abmelden");
    out.type = "button";
    out.onclick = async () => {
      try { await api("logout", {}); } catch (_) { /* gone anyway */ }
      store("kw_wl", null);
      location.reload();
    };
    who.append(out);
  }
  const shown = ["status"];
  renderStatus();
  if (me.can_give) {
    shown.push("add", "players");
    renderPlayers();
  }
  showOnly(...shown);
  setFull();
}

function used() { return (me.players || []).length; }

function renderStatus() {
  const s = $("status");
  s.innerHTML = "";
  const start = me.start ? new Date(me.start * 1000).toLocaleString("de-DE", { weekday: "long", day: "numeric", month: "long", hour: "2-digit", minute: "2-digit" }) + " Uhr" : "";
  if (me.own) {
    const row = el("div", "me");
    row.append(face(me.own.player, 48));
    row.append(el("div", "me-text", `<span class="me-name">${esc(me.own.player)}</span><span class="me-kind">Eigener Platz, ${esc(me.own.kind)}</span>`));
    s.append(row);
    if (me.slots != null) {
      const full = used() >= me.slots;
      const m = el("div", "meter",
        `<div class="meter-label"><span><b>${used()} von ${me.slots}</b> Plätzen belegt</span><span>${full ? "alle vergeben" : (me.slots - used()) + " frei"}</span></div>` +
        `<div class="bar${full ? " full" : ""}"><i></i></div>`);
      m.querySelector("i").style.width = "0";
      s.append(m);
      requestAnimationFrame(() => { m.querySelector("i").style.width = Math.min(100, me.slots ? used() / me.slots * 100 : 0) + "%"; });
    } else {
      s.append(el("p", "fine", `${used()} Plätze vergeben. Wie viele du insgesamt hast, sagt der Server, sobald er wieder erreichbar ist.`));
    }
    const actions = el("div", "actions");
    const more = el("button", "btn small", me.asked ? "Mehr Plätze angefragt" : "Mehr Plätze anfragen");
    more.type = "button";
    more.disabled = !!me.asked;
    more.onclick = () => { $("more").hidden = false; $("more-note").focus(); $("more").scrollIntoView({ behavior: "smooth", block: "nearest" }); };
    actions.append(more);
    s.append(actions);
  } else if (me.held) {
    const row = el("div", "me");
    row.append(face(me.held.player, 48));
    row.append(el("div", "me-text", `<span class="me-name">${esc(me.held.player)}</span><span class="me-kind">Auf der Whitelist, eingetragen von ${esc(me.held.by)}</span>`));
    s.append(row);
    if (me.team) s.append(el("p", "", "Als Team kannst du hier auch Plätze vergeben."));
  } else if (me.player) {
    s.append(el("p", "", rich(`Du bist noch nicht auf der Whitelist. Dein hinterlegter Name ist **${me.player}**: Ein Streamer kann dich damit eintragen.`)));
  } else {
    s.append(el("p", "", rich("Du bist noch nicht auf der Whitelist. Hinterleg zuerst deinen Minecraft-Namen mit `/link` auf dem Discord, dann kann dich ein Streamer eintragen.")));
  }
  if (start) s.append(el("p", "fine", "Der Server öffnet am " + esc(start) + "."));
  if (!me.verified) s.append(el("p", "fine", "Du bist auf dem Discord noch nicht verifiziert. Das Captcha findest du im Kanal zur Verifizierung."));
}

function renderPlayers() {
  const list = $("list");
  list.innerHTML = "";
  $("count").textContent = me.slots != null ? `${used()} von ${me.slots}` : String(used());
  if (!used()) {
    list.append(el("li", "empty", "Du hast noch niemanden eingetragen."));
    return;
  }
  for (const p of me.players) {
    const li = el("li", "item");
    li.append(face(p.player));
    const since = p.since ? new Date(p.since).toLocaleDateString("de-DE", { day: "numeric", month: "short" }) : "";
    li.append(el("div", "item-text", `<span class="item-name">${esc(p.player)}</span><span class="item-sub">${esc(p.discord_name)}${since ? ", seit " + esc(since) : ""}</span>`));
    const rm = el("button", "btn small danger", "Entfernen");
    rm.type = "button";
    let armed = 0;
    rm.onclick = async () => {
      if (!armed) {
        rm.classList.add("sure");
        rm.textContent = "Wirklich?";
        armed = setTimeout(() => { armed = 0; rm.classList.remove("sure"); rm.textContent = "Entfernen"; }, 3000);
        return;
      }
      clearTimeout(armed);
      rm.disabled = true;
      try {
        const r = await api("remove", { player: p.player });
        me = r.me;
        toast(r.message, "good");
        render();
      } catch (e) {
        rm.disabled = false;
        if (e.shown) toast(e.message, "bad");
      }
    };
    li.append(rm);
    list.append(li);
  }
}

function full() { return me.slots != null && used() >= me.slots && !me.team; }

function setFull() {
  const q = $("q");
  q.disabled = full();
  q.placeholder = full() ? "Alle Plätze belegt" : "Minecraft- oder Discord-Name";
}

// ---- finding and adding ----

let findTimer = 0;
let findSeq = 0;

$("q").addEventListener("input", () => {
  clearTimeout(findTimer);
  const q = $("q").value.trim();
  if (q.length < 2) {
    $("results").innerHTML = "";
    return;
  }
  findTimer = setTimeout(() => find(q), 280);
});

async function find(q) {
  const seq = ++findSeq;
  let data;
  try {
    data = await api("find?q=" + encodeURIComponent(q));
  } catch (e) {
    if (e.shown && seq === findSeq) toast(e.message, "bad");
    return;
  }
  if (seq !== findSeq) return;
  const box = $("results");
  box.innerHTML = "";
  const hits = data.hits || [];
  const linkedHere = hits.some((h) => h.player && h.player.toLowerCase() === q.toLowerCase());
  if (data.account && !linkedHere) {
    const a = el("div", "account");
    a.append(face(data.account.name, 40));
    a.append(el("div", "item-text",
      `<span class="item-name">${esc(data.account.name)}</span>` +
      `<p>Den Minecraft-Account gibt es, aber auf dem Discord hat ihn niemand mit /link hinterlegt. Such die Person mit ihrem Discord-Namen, oder bitte sie, <code>/link ${esc(data.account.name)}</code> zu schreiben.</p>`));
    box.append(a);
  } else if (data.account === null && !hits.length) {
    box.append(el("p", "empty", rich(`Keinen Minecraft-Account **${q}** gefunden und niemanden auf dem Discord mit diesem Namen.`)));
    return;
  }
  if (!hits.length && !data.account) {
    box.append(el("p", "empty", "Niemanden gefunden."));
    return;
  }
  const ul = el("ul", "results");
  for (const h of hits) ul.append(hitRow(h));
  box.append(ul);
}

function hitRow(h) {
  const li = el("li", "item" + (h.state === "ok" || h.state === "nolink" ? "" : " off"));
  li.append(face(h.player || ""));
  const sub = h.player ? `Minecraft: ${esc(h.player)}` : "@" + esc(h.username);
  li.append(el("div", "item-text",
    `<span class="item-name">${esc(h.discord_name)}</span><span class="item-sub">${sub}</span>` +
    (h.state !== "ok" && h.note ? `<span class="item-note">${esc(h.note)}</span>` : "")));
  if (h.state === "ok") {
    const add = el("button", "btn small primary", "Eintragen");
    add.type = "button";
    add.onclick = () => doAdd(add, h.discord_id, "");
    li.append(add);
  } else if (h.state === "nolink") {
    const add = el("button", "btn small", "Name eingeben");
    add.type = "button";
    add.onclick = () => {
      add.remove();
      const row = el("div", "typed");
      const prev = face("");
      prev.width = prev.height = 30;
      const inp = el("input");
      inp.type = "text";
      inp.maxLength = 16;
      inp.placeholder = "Minecraft-Name";
      inp.autocomplete = "off";
      inp.spellcheck = false;
      let t = 0;
      inp.oninput = () => {
        clearTimeout(t);
        t = setTimeout(() => { prev.src = "head/" + encodeURIComponent(inp.value.trim() || "-"); }, 300);
      };
      const go = el("button", "btn small primary", "Eintragen");
      go.type = "button";
      go.onclick = () => doAdd(go, h.discord_id, inp.value.trim());
      inp.onkeydown = (e) => { if (e.key === "Enter") go.click(); };
      row.append(prev, inp, go);
      li.append(row);
      inp.focus();
    };
    li.append(add);
  } else {
    li.append(el("span"));
  }
  return li;
}

async function doAdd(btn, id, player) {
  if (player !== "" && !/^[A-Za-z0-9_]{3,16}$/.test(player)) {
    toast("Ein Minecraft-Name hat 3 bis 16 Zeichen: Buchstaben, Zahlen und Unterstrich.", "bad");
    return;
  }
  btn.disabled = true;
  try {
    const r = await api("add", { discord_id: id, player });
    me = r.me;
    toast(r.message, "good");
    $("q").value = "";
    $("results").innerHTML = "";
    render();
  } catch (e) {
    btn.disabled = false;
    if (e.shown) toast(e.message, "bad");
  }
}

// ---- more slots ----

$("more-cancel").onclick = () => { $("more").hidden = true; };
$("more-send").onclick = async () => {
  const b = $("more-send");
  b.disabled = true;
  try {
    const r = await api("more", { note: $("more-note").value });
    me = r.me;
    $("more-note").value = "";
    toast(r.message, "good");
    render();
  } catch (e) {
    if (e.shown) toast(e.message, "bad");
  } finally {
    b.disabled = false;
  }
};

signIn(false).catch((e) => {
  showOnly("loading");
  document.querySelector(".spinner")?.remove();
  $("loading-text").textContent = e.shown ? e.message : "Die Anmeldung bei Discord ging nicht. Schließ die App und öffne sie noch einmal.";
});
