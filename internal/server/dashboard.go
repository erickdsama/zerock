package server

// The dashboard is a single self-contained page: inline CSS and JS, no external
// assets, so it works on a fresh server with no CDN reachable and adds nothing
// to the binary but this string.
//
// It authenticates with the same tokens as the API. The token is held in
// sessionStorage rather than localStorage, so closing the tab discards it, and
// it is never sent anywhere except this server's own API.
//
// Every value that comes from the API is inserted with textContent or a created
// text node. Tunnel data includes operator-supplied strings such as token
// labels, agent hostnames and subdomains, so building rows with innerHTML would
// be an injection route.
const dashboardPage = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>zerock</title>
<style>
:root{
  color-scheme:light dark;
  --bg:Canvas; --fg:CanvasText;
  --muted:color-mix(in srgb,CanvasText 55%,Canvas);
  --line:color-mix(in srgb,CanvasText 14%,Canvas);
  --panel:color-mix(in srgb,CanvasText 4%,Canvas);
  --accent:#2f7d5b; --danger:#b3261e; --warn:#8a6100;
}
@media (prefers-color-scheme:dark){
  :root{ --accent:#6ee7a8; --danger:#ff8a80; --warn:#f5c451; }
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);
  font:15px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
header{display:flex;align-items:center;gap:1rem;flex-wrap:wrap;
  padding:1rem 1.5rem;border-bottom:1px solid var(--line)}
header h1{margin:0;font-size:1.05rem;letter-spacing:-.01em}
header .who{color:var(--muted);font-size:.85rem;margin-left:auto}
main{max-width:76rem;margin:0 auto;padding:1.5rem}
nav{display:flex;gap:.25rem;margin-bottom:1.25rem;flex-wrap:wrap}
nav button{background:none;border:1px solid transparent;color:var(--muted);
  padding:.4rem .8rem;border-radius:.4rem;cursor:pointer;font:inherit}
nav button[aria-selected=true]{background:var(--panel);color:var(--fg);border-color:var(--line)}
table{width:100%;border-collapse:collapse;font-size:.88rem}
th{text-align:left;font-weight:500;color:var(--muted);font-size:.75rem;
  text-transform:uppercase;letter-spacing:.05em;padding:.5rem .6rem;
  border-bottom:1px solid var(--line);white-space:nowrap}
td{padding:.6rem;border-bottom:1px solid var(--line);vertical-align:middle}
td.mono,code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.82rem}
.wrap{overflow-x:auto;border:1px solid var(--line);border-radius:.6rem}
.empty{padding:2.5rem 1rem;text-align:center;color:var(--muted)}
button.act{background:none;border:1px solid var(--line);color:var(--fg);
  padding:.25rem .6rem;border-radius:.35rem;cursor:pointer;font:inherit;font-size:.8rem}
button.act:hover{border-color:var(--danger);color:var(--danger)}
button.primary{background:var(--fg);color:var(--bg);border:none;padding:.5rem 1rem;
  border-radius:.4rem;cursor:pointer;font:inherit;font-size:.88rem}
form.card{background:var(--panel);border:1px solid var(--line);border-radius:.6rem;
  padding:1rem;margin-bottom:1.25rem;display:flex;gap:.75rem;align-items:flex-end;flex-wrap:wrap}
label{display:flex;flex-direction:column;gap:.3rem;font-size:.78rem;color:var(--muted)}
input,select{background:var(--bg);color:var(--fg);border:1px solid var(--line);
  border-radius:.35rem;padding:.4rem .55rem;font:inherit;font-size:.88rem}
a{color:var(--accent)}
.pill{display:inline-block;padding:.1rem .45rem;border-radius:.3rem;font-size:.72rem;
  background:var(--panel);border:1px solid var(--line);color:var(--muted)}
.pill.ok{color:var(--accent)} .pill.bad{color:var(--danger)}
#login{max-width:24rem;margin:15vh auto;padding:0 1.5rem}
#login h1{font-size:1.15rem;margin:0 0 .4rem}
#login p{color:var(--muted);margin:0 0 1.25rem;font-size:.9rem}
#login input{width:100%;margin-bottom:.75rem}
#msg{margin:0 0 1rem;font-size:.85rem;min-height:1.2em}
#msg.err{color:var(--danger)} #msg.ok{color:var(--accent)}
.secret{background:var(--panel);border:1px solid var(--warn);border-radius:.5rem;
  padding:.9rem;margin-bottom:1.25rem;font-size:.85rem}
.secret code{display:block;margin:.5rem 0;word-break:break-all;font-size:.9rem}
.hint{color:var(--muted);font-size:.8rem;margin:.35rem 0 0}
</style>

<section id="login" hidden>
  <h1>zerock</h1>
  <p>Paste a token to continue. It is kept for this tab only and sent to this server alone.</p>
  <form id="loginForm">
    <input id="tokenInput" type="password" placeholder="zk_..." autocomplete="off" spellcheck="false" required>
    <button class="primary" type="submit">Sign in</button>
  </form>
  <p id="loginErr" class="hint"></p>
</section>

<div id="app" hidden>
  <header>
    <h1>zerock</h1>
    <span class="who" id="who"></span>
    <button class="act" id="signout">Sign out</button>
  </header>
  <main>
    <nav>
      <button data-tab="tunnels" aria-selected="true">Tunnels</button>
      <button data-tab="reservations" aria-selected="false">Reservations</button>
      <button data-tab="tokens" aria-selected="false" id="tokensTab" hidden>Tokens</button>
    </nav>
    <p id="msg"></p>
    <div id="view"></div>
  </main>
</div>

<script>
(function(){
  "use strict";
  var KEY = "zerock.token";
  var token = sessionStorage.getItem(KEY);
  var me = null, tab = "tunnels", timer = null;
  // Every render bumps this. A loader that finishes after the view has moved on
  // discards its result, so a slow refresh cannot paint the previous tab's table
  // over the current one.
  var generation = 0;
  // A freshly created secret, waiting to be painted. The create handler cannot
  // paint it itself: render() clears the view and reloads asynchronously, so
  // anything inserted synchronously is wiped when the reload lands.
  var pendingSecret = null;

  var el = function(tag, text, cls){
    var n = document.createElement(tag);
    if (text !== undefined && text !== null) n.textContent = String(text);
    if (cls) n.className = cls;
    return n;
  };
  var $ = function(id){ return document.getElementById(id); };

  function api(method, path, body){
    var opts = { method: method, headers: { "Accept":"application/json", "Authorization":"Bearer "+token } };
    if (body){ opts.headers["Content-Type"]="application/json"; opts.body=JSON.stringify(body); }
    return fetch(path, opts).then(function(res){
      if (res.status === 204) return null;
      return res.text().then(function(text){
        var data = null;
        try { data = text ? JSON.parse(text) : null; } catch(e){}
        if (!res.ok){
          var err = new Error((data && data.message) || ("HTTP "+res.status));
          err.status = res.status;
          throw err;
        }
        return data;
      });
    });
  }

  function say(text, kind){
    var m = $("msg");
    m.textContent = text || "";
    m.className = kind || "";
  }

  function isAdmin(){
    return me && me.token && me.token.scopes && me.token.scopes.indexOf("admin") !== -1;
  }

  // --- auth ---

  function showLogin(err){
    $("login").hidden = false;
    $("app").hidden = true;
    $("loginErr").textContent = err || "";
    if (timer) { clearInterval(timer); timer = null; }
  }

  function start(){
    if (!token){ showLogin(); return; }
    api("GET","/api/v1/whoami").then(function(data){
      me = data;
      $("login").hidden = true;
      $("app").hidden = false;
      $("who").textContent = data.token.label + " · " + data.token.scopes.join(", ") + " · " + data.domain;
      $("tokensTab").hidden = !isAdmin();
      render();
      if (timer) clearInterval(timer);
      // Tunnels are live data; the other tabs change only when someone acts.
      timer = setInterval(function(){ if (tab === "tunnels") load(); }, 5000);
      // load() reuses the current generation, so a refresh in flight during a
      // tab switch is discarded rather than rendered.
    }).catch(function(e){
      sessionStorage.removeItem(KEY); token = null;
      showLogin(e.status === 401 ? "That token was not accepted." : e.message);
    });
  }

  $("loginForm").addEventListener("submit", function(ev){
    ev.preventDefault();
    token = $("tokenInput").value.trim();
    if (!token) return;
    sessionStorage.setItem(KEY, token);
    $("tokenInput").value = "";
    start();
  });

  $("signout").addEventListener("click", function(){
    sessionStorage.removeItem(KEY); token = null; me = null;
    showLogin();
  });

  Array.prototype.forEach.call(document.querySelectorAll("nav button"), function(b){
    b.addEventListener("click", function(){
      tab = b.getAttribute("data-tab");
      Array.prototype.forEach.call(document.querySelectorAll("nav button"), function(o){
        o.setAttribute("aria-selected", String(o === b));
      });
      say("");
      pendingSecret = null;
      render();
    });
  });

  // --- rendering ---

  function table(headers, rows, emptyText){
    var wrap = el("div", null, "wrap");
    if (!rows.length){
      wrap.appendChild(el("div", emptyText, "empty"));
      return wrap;
    }
    var t = el("table"), thead = el("thead"), tr = el("tr");
    headers.forEach(function(h){ tr.appendChild(el("th", h)); });
    thead.appendChild(tr); t.appendChild(thead);
    var tbody = el("tbody");
    rows.forEach(function(cells){
      var r = el("tr");
      cells.forEach(function(c){
        var td = el("td");
        if (c instanceof Node) td.appendChild(c);
        else { td.textContent = c === null || c === undefined ? "" : String(c); }
        r.appendChild(td);
      });
      tbody.appendChild(r);
    });
    t.appendChild(tbody);
    wrap.appendChild(t);
    return wrap;
  }

  function bytes(n){
    n = n || 0;
    var units = ["B","KiB","MiB","GiB"], i = 0;
    while (n >= 1024 && i < units.length-1){ n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
  }

  function confirmThen(message, fn){
    return function(){ if (window.confirm(message)) fn(); };
  }

  function render(){
    generation++;
    $("view").textContent = "";
    load();
  }

  function load(){
    var mine = generation;
    var stale = function(){ return mine !== generation; };
    if (tab === "tunnels") return loadTunnels(stale);
    if (tab === "reservations") return loadReservations(stale);
    if (tab === "tokens") return loadTokens(stale);
  }

  function loadTunnels(stale){
    api("GET","/api/v1/tunnels").then(function(data){
      if (stale()) return;
      var rows = (data.tunnels||[]).map(function(t){
        var target = el("span");
        if (t.type === "tcp"){
          target.appendChild(el("code", t.url.replace(/^tcp:\/\//,"")));
        } else {
          var a = el("a", t.url.replace(/^https?:\/\//,""));
          a.href = t.url; a.target = "_blank"; a.rel = "noopener noreferrer";
          target.appendChild(a);
        }
        var kill = el("button","Close","act");
        kill.addEventListener("click", confirmThen(
          "Close "+t.sub+"? The agent is told not to reconnect.",
          function(){
            api("DELETE","/api/v1/tunnels/"+t.id).then(function(){
              say("Closed "+t.sub+".","ok"); render();
            }).catch(function(e){ say(e.message,"err"); });
          }));
        return [target, el("code", t.local_addr), t.token_label, t.agent_host||"-",
                t.uptime, (t.stats.requests||0)+" req",
                bytes(t.stats.bytes_out)+" ↑ "+bytes(t.stats.bytes_in)+" ↓", kill];
      });
      var view = $("view"); view.textContent = "";
      view.appendChild(table(
        ["Public","→ Local","Owner","Agent","Up","Requests","Traffic",""],
        rows, "No tunnels running. Start one with: zerock http 3000"));
    }).catch(fail);
  }

  function loadReservations(stale){
    api("GET","/api/v1/reservations").then(function(data){
      if (stale()) return;
      var domain = data.domain || (me && me.domain) || "";
      var view = $("view"); view.textContent = "";

      var form = el("form", null, "card");
      var subLabel = el("label","Subdomain");
      var sub = el("input"); sub.required = true; sub.placeholder = "api-x";
      sub.pattern = "[a-z0-9]([a-z0-9-]*[a-z0-9])?";
      subLabel.appendChild(sub);
      var noteLabel = el("label","Note");
      var note = el("input"); note.placeholder = "optional";
      noteLabel.appendChild(note);
      var submit = el("button","Reserve","primary");
      form.appendChild(subLabel); form.appendChild(noteLabel); form.appendChild(submit);
      form.addEventListener("submit", function(ev){
        ev.preventDefault();
        api("POST","/api/v1/reservations",{sub:sub.value.trim(), note:note.value.trim()})
          .then(function(){ say("Reserved "+sub.value.trim()+".","ok"); render(); })
          .catch(function(e){ say(e.message,"err"); });
      });
      view.appendChild(form);

      var rows = (data.reservations||[]).map(function(r){
        var release = el("button","Release","act");
        release.addEventListener("click", confirmThen(
          "Release "+r.sub+"? Anyone on this server could then claim it.",
          function(){
            api("DELETE","/api/v1/reservations/"+encodeURIComponent(r.sub)).then(function(){
              say("Released "+r.sub+".","ok"); render();
            }).catch(function(e){ say(e.message,"err"); });
          }));
        return [el("code", r.sub + (domain ? "."+domain : "")), r.token_id,
                (r.created_at||"").slice(0,10), r.note || "-", release];
      });
      view.appendChild(table(["Host","Token","Since","Note",""], rows,
        "No reservations. A reserved subdomain is yours alone, and survives restarts."));
    }).catch(fail);
  }

  function loadTokens(stale){
    api("GET","/api/v1/tokens").then(function(data){
      if (stale()) return;
      var view = $("view"); view.textContent = "";

      if (pendingSecret){
        var box = el("div", null, "secret");
        box.appendChild(el("strong","Copy this now — it is shown only once."));
        box.appendChild(el("code", pendingSecret));
        box.appendChild(el("div","zerock login --server "+location.hostname+" --token "+pendingSecret, "hint"));
        view.appendChild(box);
      }

      var form = el("form", null, "card");
      var labelWrap = el("label","Label");
      var label = el("input"); label.required = true; label.placeholder = "erick laptop";
      labelWrap.appendChild(label);
      var scopeWrap = el("label","Scopes");
      var scopes = el("select");
      [["tunnel","tunnel"],["tunnel,admin","tunnel + admin"]].forEach(function(o){
        var opt = el("option", o[1]); opt.value = o[0]; scopes.appendChild(opt);
      });
      scopeWrap.appendChild(scopes);
      var expWrap = el("label","Expires in");
      var exp = el("input"); exp.placeholder = "720h (blank = never)";
      expWrap.appendChild(exp);
      var maxWrap = el("label","Max tunnels");
      var max = el("input"); max.type="number"; max.min="0"; max.value="0"; max.style.width="6rem";
      maxWrap.appendChild(max);
      var create = el("button","Create","primary");
      [labelWrap,scopeWrap,expWrap,maxWrap,create].forEach(function(n){ form.appendChild(n); });

      form.addEventListener("submit", function(ev){
        ev.preventDefault();
        api("POST","/api/v1/tokens",{
          label: label.value.trim(),
          scopes: scopes.value.split(","),
          expires_in: exp.value.trim(),
          max_tunnels: parseInt(max.value,10) || 0
        }).then(function(res){
          // The secret is readable exactly once. loadTokens paints it after the
          // reload this triggers has cleared the view, so it survives.
          pendingSecret = res.secret;
          render();
        }).catch(function(e){ say(e.message,"err"); });
      });
      view.appendChild(form);

      var rows = (data.tokens||[]).map(function(t){
        var status = el("span", t.status, "pill " + (t.status === "active" ? "ok" : "bad"));
        var actions = el("span");
        if (t.id !== me.token.id){
          if (t.status === "active"){
            var revoke = el("button","Revoke","act");
            revoke.addEventListener("click", confirmThen(
              "Revoke "+t.label+"? Its live tunnels close immediately.",
              function(){
                api("POST","/api/v1/tokens/"+t.id+"/revoke").then(function(){
                  say("Revoked "+t.label+".","ok"); render();
                }).catch(function(e){ say(e.message,"err"); });
              }));
            actions.appendChild(revoke);
          }
          var del = el("button","Delete","act");
          del.style.marginLeft = ".35rem";
          del.addEventListener("click", confirmThen(
            "Delete "+t.label+"? Its tunnels close and its reserved subdomains are freed.",
            function(){
              api("DELETE","/api/v1/tokens/"+t.id).then(function(){
                say("Deleted "+t.label+".","ok"); render();
              }).catch(function(e){ say(e.message,"err"); });
            }));
          actions.appendChild(del);
        } else {
          actions.appendChild(el("span","this session","pill"));
        }
        return [el("code", t.id), t.label, t.scopes.join(","), status,
                t.max_tunnels ? t.active_tunnels+"/"+t.max_tunnels : String(t.active_tunnels),
                t.expires_at ? t.expires_at.slice(0,10) : "never",
                t.last_used_at ? t.last_used_at.slice(0,10) : "never", actions];
      });
      view.appendChild(table(
        ["ID","Label","Scopes","Status","Live","Expires","Last used",""],
        rows, "No tokens."));
    }).catch(fail);
  }

  function fail(e){
    if (e.status === 401){
      sessionStorage.removeItem(KEY); token = null;
      showLogin("Your token is no longer valid.");
      return;
    }
    say(e.message, "err");
  }

  start();
})();
</script>
</html>
`
