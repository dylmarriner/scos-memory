package main

import (
	"io"
	"net/http"
)

// portal serves the customer-facing single page app: signup/login when
// signed out, and a plan/usage/keys/settings dashboard once /api/me
// resolves. All state is fetched client-side; this handler is static HTML.
func portal(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, portalHTML)
}

const portalHTML = `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>SCOS Cloud</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@500;600;700&family=IBM+Plex+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
:root{
  --bg:#05060a; --card:rgba(13,17,28,.6); --border:rgba(255,255,255,.08); --text:#eef4fa; --muted:#7c8aa0;
  --accent:#00f0ff; --accent-dark:#00b8cc; --accent-soft:rgba(0,240,255,.1);
  --violet:#a78bfa; --danger:#ff4d6d; --danger-soft:rgba(255,77,109,.12); --radius:.8rem;
  --shadow:0 1px 2px rgba(0,0,0,.3),0 8px 24px -12px rgba(0,0,0,.55);
}
*{box-sizing:border-box}
body{font:15px/1.5 'IBM Plex Mono',ui-monospace,monospace;margin:0;background:var(--bg);color:var(--text)}
main{max-width:1040px;margin:0 auto;padding:0 24px 48px;position:relative;z-index:1}
h1,h2,h3{font-family:'Space Grotesk',sans-serif;font-weight:650;letter-spacing:-.01em}
a{color:var(--accent)}
.brand{display:flex;align-items:center;gap:10px;padding:28px 0 8px}
.brand .mark{width:32px;height:32px;border-radius:50%;flex:none;background:conic-gradient(from 210deg,var(--accent),#7c3aed,var(--accent));box-shadow:0 0 18px -2px rgba(0,240,255,.6),0 0 0 1px rgba(255,255,255,.08)}
.brand span{font-family:'Space Grotesk',sans-serif;font-weight:700;font-size:17px;letter-spacing:.02em}
.brand span em{color:var(--accent);font-style:normal}
section{background:var(--card);backdrop-filter:blur(14px);border:1px solid var(--border);border-radius:var(--radius);padding:28px;margin:16px 0;box-shadow:var(--shadow)}
.field{display:block;margin:14px 0;text-align:left}
.field>span{display:block;font-size:12px;font-weight:600;color:var(--muted);margin-bottom:6px;text-transform:uppercase;letter-spacing:.05em}
input,select{font:inherit;padding:11px 13px;border:1px solid rgba(255,255,255,.12);border-radius:.4rem;width:100%;background:#05070d;color:var(--text);transition:border-color .12s,box-shadow .12s}
input:focus,select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-soft)}
button{font:inherit;font-weight:600;padding:11px 16px;border:0;border-radius:.4rem;cursor:pointer;background:var(--accent-dark);color:#05070d;transition:background .12s,transform .05s}
button:hover{background:var(--accent)}
button:active{transform:translateY(1px)}
button.secondary{background:#141a2b;color:var(--text);border:1px solid var(--border)}
button.secondary:hover{background:#1c2438}
button.danger{background:var(--danger-soft);color:var(--danger);border:1px solid rgba(255,77,109,.3)}
button.danger:hover{background:rgba(255,77,109,.2)}
button.block{width:100%;padding:12px 16px}
button.small{width:auto;padding:7px 12px;font-size:13px}
pre{white-space:pre-wrap;background:#04060b;border:1px solid var(--border);padding:14px 16px;border-radius:.5rem;font-size:13px;color:#9fe8b0;margin:12px 0 0}
pre:empty{display:none}
.row{display:flex;gap:16px}
.row>div,.row>.field{flex:1}
.tabs{display:flex;gap:6px;margin-bottom:20px;background:rgba(13,17,28,.5);padding:4px;border-radius:.6rem;width:fit-content;border:1px solid var(--border)}
.tabs button{width:auto;background:transparent;color:var(--muted);padding:8px 16px;border-radius:.4rem;font-size:13px;font-family:'Space Grotesk',sans-serif;font-weight:600;letter-spacing:.04em;text-transform:uppercase;font-size:11px}
.tabs button.active{background:rgba(0,240,255,.1);color:var(--accent);box-shadow:0 0 0 1px rgba(0,240,255,.25) inset}
table{width:100%;border-collapse:collapse}
td,th{text-align:left;padding:10px 8px;border-bottom:1px solid var(--border);font-size:14px}
th{color:var(--muted);font-weight:600;font-size:11px;text-transform:uppercase;letter-spacing:.06em}
tr:last-child td{border-bottom:0}
.hidden{display:none}
.plan-card{border:1px solid var(--border);border-radius:.6rem;padding:18px;margin:8px 0;background:rgba(13,17,28,.4)}
.badge{display:inline-block;background:var(--accent-soft);color:var(--accent);border-radius:999px;padding:3px 11px;font-size:.8em;font-weight:600}
.muted{color:var(--muted);font-size:14px}
.auth-wrap{max-width:400px;margin:6vh auto 0}
.auth-wrap section{padding:32px;border:1px solid rgba(0,240,255,.22);box-shadow:0 0 40px -18px rgba(0,240,255,.35)}
.auth-wrap h1{text-align:center;font-size:22px;margin:4px 0 2px;text-transform:uppercase;letter-spacing:.03em;color:#fff}
.auth-wrap p.tagline{text-align:center;color:var(--muted);margin:0 0 22px;font-size:13px;letter-spacing:.02em}
.auth-wrap .tabs{width:100%;margin-bottom:22px}
.auth-wrap .tabs button{flex:1}
.pricing-wrap{max-width:820px;margin:36px auto 0}
.pricing-wrap h2{text-align:center;font-size:20px;text-transform:uppercase;letter-spacing:.03em;color:var(--violet)}
.pricing-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:14px}
.dash-head{display:flex;align-items:center;justify-content:space-between;margin-bottom:4px}
.graph-wrap{position:relative}
.graph-svg{width:100%;height:460px;background:radial-gradient(circle at 50% 40%,rgba(124,58,237,.08),transparent 65%),#04060b;border-radius:.6rem;cursor:grab;touch-action:none}
.graph-svg.panning{cursor:grabbing}
.graph-edge{stroke:#3fd9e8;stroke-width:1.1;opacity:.5;transition:opacity .12s ease,stroke .12s ease}
.graph-edge.mentions{stroke-dasharray:4 6}
.graph-edge.dim{opacity:.05}
.graph-edge.active{stroke:var(--violet);stroke-width:1.6;opacity:.95}
.graph-node circle{stroke-width:1.4;transition:opacity .12s ease,r .12s ease}
.graph-node.fact circle{fill:#0a2230;stroke:var(--accent)}
.graph-node.entity circle{fill:#1c1230;stroke:var(--violet)}
.graph-node.dim{opacity:.15}
.graph-node:hover circle{filter:brightness(1.3)}
.graph-label{fill:var(--muted);font-size:8.5px;text-anchor:middle;user-select:none}
.graph-tooltip{position:absolute;display:none;background:#05070d;color:var(--text);border:1px solid var(--border);border-radius:.4rem;padding:6px 10px;font-size:12px;pointer-events:none;max-width:220px;z-index:5}
.graph-tooltip span{color:var(--muted)}
.graph-legend{display:flex;flex-wrap:wrap;align-items:center;gap:.9rem;margin-top:.5rem;font-size:.74rem;color:var(--muted)}
.graph-legend span{display:inline-flex;align-items:center;gap:.35rem}
.graph-dot{width:9px;height:9px;border-radius:50%;display:inline-block}
.graph-dot.fact{background:#0a2230;border:1.5px solid var(--accent)}
.graph-dot.entity{background:#1c1230;border:1.5px solid var(--violet)}
</style></head><body><main>

<div id="signed-out">
  <div class="brand" style="justify-content:center"><div class="mark"></div><span>SCOS <em>// CLOUD</em></span></div>
  <div class="auth-wrap">
    <section id="auth-section">
      <h1>Welcome</h1>
      <p class="tagline">Persistent, private memory for your AI agents.</p>
      <div class="tabs">
        <button id="tab-signup" class="active" onclick="showAuth('signup')">Create account</button>
        <button id="tab-login" onclick="showAuth('login')">Sign in</button>
      </div>
      <div id="signup-form">
        <label class="field"><span>Full name</span><input id="su_n" autocomplete="name" placeholder="Ada Lovelace"></label>
        <label class="field"><span>Email</span><input id="su_e" type="email" autocomplete="email" placeholder="you@example.com"></label>
        <label class="field"><span>Password</span><input id="su_p" type="password" autocomplete="new-password" placeholder="At least 12 characters"></label>
        <button class="block" onclick="signup()">Create account</button>
      </div>
      <div id="login-form" class="hidden">
        <label class="field"><span>Email</span><input id="li_e" type="email" autocomplete="email" placeholder="you@example.com"></label>
        <label class="field"><span>Password</span><input id="li_p" type="password" autocomplete="current-password" placeholder="Your password"></label>
        <button class="block" onclick="login()">Sign in</button>
      </div>
      <pre id="auth_out"></pre>
    </section>
  </div>

  <div class="pricing-wrap">
    <h2>Plans</h2>
    <div id="pricing" class="pricing-grid"></div>
  </div>
</div>

<section id="dashboard" class="hidden">
  <div class="brand"><div class="mark"></div><span>SCOS <em>// CLOUD</em></span></div>
  <div class="dash-head">
    <h2 id="welcome"></h2>
    <button class="danger small" onclick="logout()">Sign out</button>
  </div>
  <div class="tabs">
    <button id="dtab-overview" class="active" onclick="showDash('overview')">Overview</button>
    <button id="dtab-memory" onclick="showDash('memory')">Memory</button>
    <button id="dtab-code" onclick="showDash('code')">Code index</button>
    <button id="dtab-graph" onclick="showDash('graph')">Graphify</button>
    <button id="dtab-okf" onclick="showDash('okf')">OKF</button>
    <button id="dtab-keys" onclick="showDash('keys')">API keys</button>
    <button id="dtab-vault" onclick="showDash('vault')">Vault</button>
    <button id="dtab-settings" onclick="showDash('settings')">Settings</button>
  </div>

  <div id="dash-overview">
    <p>Current plan: <span class="badge" id="d_plan">-</span> <span class="muted" id="d_status"></span></p>
    <table><tbody>
      <tr><td>API requests (all time)</td><td id="u_requests">-</td></tr>
      <tr><td>Memory reads</td><td id="u_reads">-</td></tr>
      <tr><td>Memory writes</td><td id="u_writes">-</td></tr>
      <tr><td>Bytes stored</td><td id="u_bytes">-</td></tr>
    </tbody></table>
  </div>

  <div id="dash-memory" class="hidden">
    <p class="muted">Every fact and entity your agents have stored, and how they connect. Only visible to you.</p>
    <div id="memory-graph"></div>
    <pre id="memory_out"></pre>
    <table><thead><tr><th>Content</th><th>Network</th><th>Recorded</th></tr></thead><tbody id="memory-facts-body"></tbody></table>
  </div>

  <div id="dash-code" class="hidden">
    <p class="muted">Repositories code-intelligence has indexed for you, and the status of each indexing run. Only your own repos.</p>
    <h3 style="font-size:14px;margin:18px 0 4px">Repositories</h3>
    <table><thead><tr><th>Path</th><th>Branch</th><th>Commit</th><th>Languages</th><th>Generation</th></tr></thead><tbody id="code-repos-body"></tbody></table>
    <h3 style="font-size:14px;margin:18px 0 4px">Indexing status</h3>
    <table><thead><tr><th>Repository</th><th>Status</th><th>Files</th><th>Symbols</th><th>Started</th><th>Completed</th></tr></thead><tbody id="code-status-body"></tbody></table>
  </div>

  <div id="dash-graph" class="hidden">
    <p class="muted">Ask graphify about a code relationship, or ask it to explain a specific node. It answers live queries — there's no static graph to browse.</p>
    <div class="row">
      <label class="field"><span>Ask a question (query)</span><input id="graphify_q" placeholder="e.g. what calls handleLogin?"></label>
      <label class="field"><span>Repo path (optional)</span><input id="graphify_repo" placeholder="e.g. services/cloud-control-plane"></label>
    </div>
    <button onclick="graphifyAsk()">Ask</button>
    <div class="row" style="margin-top:14px">
      <label class="field"><span>Explain a node</span><input id="graphify_node" placeholder="e.g. handleLogin"></label>
    </div>
    <button class="secondary" onclick="graphifyExplainNode()">Explain</button>
    <pre id="graphify_out"></pre>
  </div>

  <div id="dash-okf" class="hidden">
    <p class="muted">OKF concepts synced across the whole platform. Shared knowledge, not private to your account.</p>
    <table><thead><tr><th>Path</th><th>Title</th><th>Updated</th></tr></thead><tbody id="okf-docs-body"></tbody></table>
  </div>

  <div id="dash-keys" class="hidden">
    <p class="muted">API keys let your agents authenticate to your shared memory over MCP.</p>
    <div class="row">
      <label class="field"><span>Key name</span><input id="k" placeholder="e.g. laptop-agent"></label>
    </div>
    <button onclick="createKey()">Create API key</button>
    <pre id="key_out"></pre>
    <table><thead><tr><th>Name</th><th>Prefix</th><th>Created</th><th></th></tr></thead><tbody id="keys_body"></tbody></table>
  </div>

  <div id="dash-vault" class="hidden">
    <p class="muted">Keys and passwords stored here are encrypted and available to any of your agents.</p>
    <div class="row">
      <label class="field"><span>Name</span><input id="v_name" placeholder="e.g. stripe-api-key"></label>
      <label class="field"><span>Value</span><input id="v_value" type="password" placeholder="Value"></label>
    </div>
    <button onclick="createCredential()">Save credential</button>
    <pre id="vault_out"></pre>
    <table><thead><tr><th>Name</th><th>Created</th><th>Updated</th><th></th></tr></thead><tbody id="vault_body"></tbody></table>
  </div>

  <div id="dash-settings" class="hidden">
    <label class="field"><span>Display name</span><input id="set_name"></label>
    <button onclick="saveSettings()">Save</button>
    <pre id="settings_out"></pre>
    <div id="creator_link" class="hidden"><p class="muted">You have the Creator role. <a href="/admin">Open the Creator Console →</a></p></div>
  </div>
</section>

<script>
async function req(u,opts){
  let r=await fetch(u,Object.assign({headers:{'content-type':'application/json'}},opts||{}));
  let t=await r.text();
  let x={};
  try{x=JSON.parse(t)}catch(e){}
  if(!r.ok) throw Error(x.error || t || (r.status+' '+r.statusText));
  return x;
}
function post(u,b){return req(u,{method:'POST',body:JSON.stringify(b)})}
function patch(u,b){return req(u,{method:'PATCH',body:JSON.stringify(b)})}
function put(u,b){return req(u,{method:'PUT',body:JSON.stringify(b)})}
function del(u){return req(u,{method:'DELETE'})}

function showAuth(which){
  document.getElementById('tab-signup').classList.toggle('active',which==='signup');
  document.getElementById('tab-login').classList.toggle('active',which==='login');
  document.getElementById('signup-form').classList.toggle('hidden',which!=='signup');
  document.getElementById('login-form').classList.toggle('hidden',which!=='login');
}
async function signup(){
  try{await post('/api/auth/signup',{display_name:su_n.value,email:su_e.value,password:su_p.value});await boot()}
  catch(e){auth_out.textContent=e.message}
}
async function login(){
  try{await post('/api/auth/login',{email:li_e.value,password:li_p.value});await boot()}
  catch(e){auth_out.textContent=e.message}
}
async function logout(){await post('/api/auth/logout',{});location.reload()}

function showDash(which){
  for(const t of ['overview','memory','code','graph','okf','keys','vault','settings']){
    document.getElementById('dtab-'+t).classList.toggle('active',t===which);
    document.getElementById('dash-'+t).classList.toggle('hidden',t!==which);
  }
  if(which==='memory')loadMemoryTab();
  if(which==='code')loadCodeTab();
  if(which==='okf')loadOkfTab();
}

function escapeHtml(s){
  return String(s==null?'':s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

// ── minimal force-directed graph (read-only) ──────────────────────────────
function forceLayout(nodes,edges,width,height,iterations){
  const index=new Map(nodes.map((n,i)=>[n.id,i]));
  const pos=nodes.map(()=>({x:width/2+(Math.random()-0.5)*width*0.8,y:height/2+(Math.random()-0.5)*height*0.8}));
  const links=edges.map(e=>({s:index.get(e.source),t:index.get(e.target)})).filter(l=>l.s!==undefined&&l.t!==undefined&&l.s!==l.t);
  const k=Math.sqrt((width*height)/Math.max(nodes.length,1));
  let temperature=Math.max(width,height)/8;
  const cooling=temperature/iterations;
  for(let iter=0;iter<iterations;iter++){
    const disp=pos.map(()=>({x:0,y:0}));
    for(let i=0;i<pos.length;i++)for(let j=i+1;j<pos.length;j++){
      const dx=pos[i].x-pos[j].x,dy=pos[i].y-pos[j].y;
      const d=Math.sqrt(dx*dx+dy*dy)||0.01;
      const force=(k*k)/d;
      const ux=dx/d,uy=dy/d;
      disp[i].x+=ux*force;disp[i].y+=uy*force;
      disp[j].x-=ux*force;disp[j].y-=uy*force;
    }
    for(const l of links){
      const a=pos[l.s],b=pos[l.t];
      const dx=a.x-b.x,dy=a.y-b.y;
      const d=Math.sqrt(dx*dx+dy*dy)||0.01;
      const force=(d*d)/k;
      const ux=dx/d,uy=dy/d;
      disp[l.s].x-=ux*force;disp[l.s].y-=uy*force;
      disp[l.t].x+=ux*force;disp[l.t].y+=uy*force;
    }
    for(let i=0;i<pos.length;i++){disp[i].x+=(width/2-pos[i].x)*0.01;disp[i].y+=(height/2-pos[i].y)*0.01}
    for(let i=0;i<pos.length;i++){
      const dx=disp[i].x,dy=disp[i].y;
      const d=Math.sqrt(dx*dx+dy*dy)||0.01;
      const capped=Math.min(d,temperature);
      pos[i].x=Math.max(24,Math.min(width-24,pos[i].x+(dx/d)*capped));
      pos[i].y=Math.max(24,Math.min(height-24,pos[i].y+(dy/d)*capped));
    }
    temperature=Math.max(0.5,temperature-cooling);
  }
  return pos;
}
const SVG_NS='http://www.w3.org/2000/svg', GRAPH_MAX_NODES=220;
function renderGraph(container,data){
  container.innerHTML='';
  const wrap=document.createElement('div');
  wrap.className='graph-wrap';
  const allNodes=(data.nodes||[]).filter(n=>n&&n.id!=null);
  const allEdges=(data.edges||[]).filter(e=>e&&e.source!=null&&e.target!=null);
  if(allNodes.length===0){
    const p=document.createElement('p');
    p.className='muted';
    p.textContent='No linked memories yet.';
    container.appendChild(p);
    return;
  }
  let nodes=allNodes,edges=allEdges,truncated=false;
  const rawDegree=new Map();
  for(const e of allEdges){rawDegree.set(e.source,(rawDegree.get(e.source)||0)+1);rawDegree.set(e.target,(rawDegree.get(e.target)||0)+1)}
  if(allNodes.length>GRAPH_MAX_NODES){
    nodes=[...allNodes].sort((a,b)=>(rawDegree.get(b.id)||0)-(rawDegree.get(a.id)||0)).slice(0,GRAPH_MAX_NODES);
    const keep=new Set(nodes.map(n=>n.id));
    edges=allEdges.filter(e=>keep.has(e.source)&&keep.has(e.target));
    truncated=true;
  }
  const width=780,height=460;
  const degree=new Map();
  for(const e of edges){degree.set(e.source,(degree.get(e.source)||0)+1);degree.set(e.target,(degree.get(e.target)||0)+1)}
  const pos=forceLayout(nodes,edges,width,height,Math.min(220,4000/Math.max(nodes.length,1)+60));
  const index=new Map(nodes.map((n,i)=>[n.id,i]));
  const svg=document.createElementNS(SVG_NS,'svg');
  svg.setAttribute('viewBox','0 0 '+width+' '+height);
  svg.setAttribute('class','graph-svg');
  const g=document.createElementNS(SVG_NS,'g');
  svg.appendChild(g);
  const edgeGroup=document.createElementNS(SVG_NS,'g'), nodeGroup=document.createElementNS(SVG_NS,'g');
  g.append(edgeGroup,nodeGroup);
  const edgeEls=[];
  for(const e of edges){
    const si=index.get(e.source),ti=index.get(e.target);
    if(si===undefined||ti===undefined)continue;
    const line=document.createElementNS(SVG_NS,'line');
    line.setAttribute('class','graph-edge'+(e.type==='mentions'?' mentions':''));
    line.setAttribute('x1',pos[si].x);line.setAttribute('y1',pos[si].y);
    line.setAttribute('x2',pos[ti].x);line.setAttribute('y2',pos[ti].y);
    line.dataset.s=si;line.dataset.t=ti;
    const title=document.createElementNS(SVG_NS,'title');
    title.textContent=e.type||'related';
    line.appendChild(title);
    edgeGroup.appendChild(line);
    edgeEls.push(line);
  }
  const neighbors=nodes.map(()=>new Set());
  edges.forEach(e=>{
    const si=index.get(e.source),ti=index.get(e.target);
    if(si===undefined||ti===undefined)return;
    neighbors[si].add(ti);neighbors[ti].add(si);
  });
  const tooltip=document.createElement('div');
  tooltip.className='graph-tooltip';
  function showTooltip(ev,n,isEntity){
    tooltip.style.display='block';
    tooltip.innerHTML='<strong>'+escapeHtml(n.label||n.id)+'</strong><br><span>'+(isEntity?'entity':(n.network?'fact &middot; '+escapeHtml(n.network):'fact'))+'</span>';
    const rect=wrap.getBoundingClientRect();
    tooltip.style.left=Math.min(rect.width-190,Math.max(0,ev.clientX-rect.left+14))+'px';
    tooltip.style.top=Math.max(0,ev.clientY-rect.top-8)+'px';
  }
  function hideTooltip(){tooltip.style.display='none'}
  const nodeEls=[];
  nodes.forEach((n,i)=>{
    const p=pos[i];
    const isEntity=n.type==='entity';
    const r=Math.min(17,5+Math.sqrt(degree.get(n.id)||0)*2.3);
    const nodeEl=document.createElementNS(SVG_NS,'g');
    nodeEl.setAttribute('class','graph-node'+(isEntity?' entity':' fact'));
    nodeEl.setAttribute('transform','translate('+p.x+','+p.y+')');
    const circle=document.createElementNS(SVG_NS,'circle');
    circle.setAttribute('r',r);
    nodeEl.appendChild(circle);
    if(r>8){
      const label=document.createElementNS(SVG_NS,'text');
      label.setAttribute('class','graph-label');
      label.setAttribute('y',r+11);
      label.textContent=String(n.label||n.id||'').slice(0,22);
      nodeEl.appendChild(label);
    }
    nodeEl.addEventListener('mouseenter',ev=>{highlightNode(i);showTooltip(ev,n,isEntity)});
    nodeEl.addEventListener('mousemove',ev=>showTooltip(ev,n,isEntity));
    nodeEl.addEventListener('mouseleave',()=>{clearHighlight();hideTooltip()});
    nodeGroup.appendChild(nodeEl);
    nodeEls.push(nodeEl);
  });
  function highlightNode(i){
    const keep=new Set([i,...neighbors[i]]);
    nodeEls.forEach((el,j)=>el.classList.toggle('dim',!keep.has(j)));
    edgeEls.forEach(el=>{
      const s=+el.dataset.s,t=+el.dataset.t;
      el.classList.toggle('dim',!(keep.has(s)&&keep.has(t)));
      el.classList.toggle('active',s===i||t===i);
    });
  }
  function clearHighlight(){
    nodeEls.forEach(el=>el.classList.remove('dim'));
    edgeEls.forEach(el=>el.classList.remove('dim','active'));
  }
  let scale=1,tx=0,ty=0,dragging=false,lastX=0,lastY=0;
  function applyTransform(){g.setAttribute('transform','translate('+tx+','+ty+') scale('+scale+')')}
  svg.addEventListener('wheel',ev=>{ev.preventDefault();scale=Math.min(4,Math.max(0.35,scale*(ev.deltaY<0?1.12:0.89)));applyTransform()},{passive:false});
  svg.addEventListener('pointerdown',ev=>{dragging=true;lastX=ev.clientX;lastY=ev.clientY;svg.setPointerCapture(ev.pointerId);svg.classList.add('panning')});
  svg.addEventListener('pointermove',ev=>{if(!dragging)return;tx+=ev.clientX-lastX;ty+=ev.clientY-lastY;lastX=ev.clientX;lastY=ev.clientY;applyTransform()});
  svg.addEventListener('pointerup',()=>{dragging=false;svg.classList.remove('panning')});
  svg.addEventListener('pointerleave',()=>{dragging=false;svg.classList.remove('panning')});
  wrap.append(svg,tooltip);
  const legend=document.createElement('div');
  legend.className='graph-legend';
  legend.innerHTML='<span><i class="graph-dot fact"></i>fact</span><span><i class="graph-dot entity"></i>entity</span>'+
    '<span>'+nodes.length+' node'+(nodes.length===1?'':'s')+' &middot; '+edges.length+' link'+(edges.length===1?'':'s')+'</span>'+
    '<span>scroll to zoom &middot; drag to pan</span>';
  wrap.appendChild(legend);
  if(truncated){
    const note=document.createElement('p');
    note.className='muted';
    note.textContent='showing the '+nodes.length+' most-connected of '+allNodes.length+' nodes';
    wrap.appendChild(note);
  }
  container.appendChild(wrap);
}

async function loadMemoryTab(){
  try{
    const graph=await req('/api/memory/graph');
    renderGraph(document.getElementById('memory-graph'),graph);
  }catch(e){memory_out.textContent='Memory graph unavailable: '+e.message}
  try{
    const facts=await req('/api/memory/facts');
    document.getElementById('memory-facts-body').innerHTML=(facts.entries||[]).map(f=>
      '<tr><td>'+escapeHtml(f.content).slice(0,140)+'</td><td>'+escapeHtml(f.network)+'</td><td>'+escapeHtml(f.recorded_at)+'</td></tr>'
    ).join('')||'<tr><td colspan="3">No facts recorded yet.</td></tr>';
  }catch(e){memory_out.textContent=(memory_out.textContent?memory_out.textContent+'\n':'')+'Facts unavailable: '+e.message}
}

async function loadCodeTab(){
  try{
    const repos=await req('/api/code/repos');
    document.getElementById('code-repos-body').innerHTML=(repos||[]).map(r=>
      '<tr><td>'+escapeHtml(r.canonical_path)+'</td><td>'+escapeHtml(r.branch_or_worktree)+'</td><td>'+escapeHtml((r.commit_id||'').slice(0,10))+'</td><td>'+escapeHtml((r.language_set||[]).join(', '))+'</td><td>'+escapeHtml(r.index_generation)+'</td></tr>'
    ).join('')||'<tr><td colspan="5">No repositories indexed yet.</td></tr>';
    const status=await req('/api/code/index-status');
    document.getElementById('code-status-body').innerHTML=(status||[]).map(g=>
      '<tr><td>'+escapeHtml(g.repository_id)+'</td><td>'+escapeHtml(g.status)+'</td><td>'+escapeHtml(g.file_count)+'</td><td>'+escapeHtml(g.symbol_count)+'</td><td>'+escapeHtml(g.started_at)+'</td><td>'+escapeHtml(g.completed_at||'—')+'</td></tr>'
    ).join('')||'<tr><td colspan="6">No indexing runs yet.</td></tr>';
  }catch(e){document.getElementById('code-repos-body').innerHTML='<tr><td colspan="5">Code index unavailable: '+escapeHtml(e.message)+'</td></tr>'}
}

async function loadOkfTab(){
  try{
    const docs=await req('/api/okf/documents');
    document.getElementById('okf-docs-body').innerHTML=(docs||[]).map(d=>
      '<tr><td>'+escapeHtml(d.path)+'</td><td>'+escapeHtml(d.title)+'</td><td>'+escapeHtml(d.updated_at)+'</td></tr>'
    ).join('')||'<tr><td colspan="3">No OKF documents synced yet.</td></tr>';
  }catch(e){document.getElementById('okf-docs-body').innerHTML='<tr><td colspan="3">OKF unavailable: '+escapeHtml(e.message)+'</td></tr>'}
}

async function graphifyAsk(){
  try{
    const body={q:graphify_q.value};
    if(graphify_repo.value)body.repo_path=graphify_repo.value;
    const x=await post('/api/graphify/query',body);
    graphify_out.textContent=JSON.stringify(x,null,2);
  }catch(e){graphify_out.textContent=e.message}
}
async function graphifyExplainNode(){
  try{
    const x=await post('/api/graphify/explain',{node:graphify_node.value});
    graphify_out.textContent=JSON.stringify(x,null,2);
  }catch(e){graphify_out.textContent=e.message}
}

async function loadPricing(){
  const plans=await req('/api/plans');
  pricing.innerHTML=plans.map(p=>'<div class="plan-card"><h3>'+p.name+' — '+p.price+'</h3><ul>'+p.perks.map(x=>'<li>'+x+'</li>').join('')+'</ul></div>').join('');
}

async function loadDashboard(me){
  document.getElementById('signed-out').classList.add('hidden');
  document.getElementById('dashboard').classList.remove('hidden');
  welcome.textContent='Welcome, '+me.DisplayName;
  set_name.value=me.DisplayName;
  creator_link.classList.toggle('hidden',me.PlatformRole!=='creator');
  const [billing,usage,keys]=await Promise.all([req('/api/billing'),req('/api/usage'),req('/api/keys')]);
  let vault=[];
  try{vault=await req('/api/vault')}
  catch(e){vault_out.textContent='Credential vault is temporarily unavailable.'}
  d_plan.textContent=billing.plan;
  d_status.textContent=billing.status;
  u_requests.textContent=usage.api_requests;
  u_reads.textContent=usage.memory_reads;
  u_writes.textContent=usage.memory_writes;
  u_bytes.textContent=usage.bytes_stored;
  keys_body.innerHTML=keys.map(k=>'<tr><td>'+k.name+'</td><td><code>'+k.prefix+'…</code></td><td>'+new Date(k.created_at).toLocaleDateString()+'</td><td><button class="danger" style="width:auto" onclick="revokeKey(\''+k.id+'\')">Revoke</button></td></tr>').join('') || '<tr><td colspan="4">No keys yet.</td></tr>';
  vault_body.innerHTML=vault.map(c=>'<tr><td>'+c.name+'</td><td>'+new Date(c.created_at).toLocaleDateString()+'</td><td>'+new Date(c.updated_at).toLocaleDateString()+'</td><td><button class="secondary" style="width:auto" onclick="rotateCredential(\''+c.id+'\',\''+c.name+'\')">Rotate</button> <button class="danger" style="width:auto" onclick="deleteCredential(\''+c.id+'\')">Delete</button></td></tr>').join('') || '<tr><td colspan="4">No credentials yet.</td></tr>';
}

async function createKey(){
  try{const x=await post('/api/keys',{name:k.value});key_out.textContent='Copy this key now — it will not be shown again:\n'+x.key;k.value='';await loadDashboard(await req('/api/me'))}
  catch(e){key_out.textContent=e.message}
}
async function revokeKey(id){
  try{await post('/api/keys/revoke',{id});await loadDashboard(await req('/api/me'))}
  catch(e){key_out.textContent=e.message}
}
async function createCredential(){
  try{
    if(!v_name.value||!v_value.value){vault_out.textContent='Name and value are required.';return}
    await post('/api/vault',{name:v_name.value,value:v_value.value});
    v_name.value='';v_value.value='';vault_out.textContent='Saved.';
    await loadDashboard(await req('/api/me'));
  }catch(e){vault_out.textContent=e.message}
}
async function rotateCredential(id,name){
  const value=prompt('New value for "'+name+'":');
  if(!value)return;
  try{await put('/api/vault/'+id,{value});vault_out.textContent='Rotated.';await loadDashboard(await req('/api/me'))}
  catch(e){vault_out.textContent=e.message}
}
async function deleteCredential(id){
  if(!confirm('Delete this credential? This cannot be undone.'))return;
  try{await del('/api/vault/'+id);vault_out.textContent='Deleted.';await loadDashboard(await req('/api/me'))}
  catch(e){vault_out.textContent=e.message}
}
async function saveSettings(){
  try{await patch('/api/settings',{display_name:set_name.value});settings_out.textContent='Saved.';welcome.textContent='Welcome, '+set_name.value}
  catch(e){settings_out.textContent=e.message}
}

async function boot(){
  await loadPricing();
  try{
    const me=await req('/api/me');
    await loadDashboard(me);
  }catch(e){/* signed out: show auth + pricing */}
}
boot();
</script>
</main></body></html>`
