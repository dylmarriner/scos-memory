package main

import (
	"io"
	"net/http"
)

// adminPortal serves the Creator Console: a platform-overview dashboard,
// searchable/filterable user management with inline plan and role editing,
// a per-tenant usage lookup, the admin audit log, and a creator-only
// settings panel for editing the pricing/perks copy shown on the public
// portal. Access is gated server-side on every /api/admin/* call this page
// makes; the page itself renders for anyone but every action 403s without
// the Creator role.
func adminPortal(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, adminPortalHTML)
}

const adminPortalHTML = `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>SCOS Creator Console</title><style>
body{font:16px system-ui;margin:0;background:#f7f9fc;color:#14213d}
main{max-width:1200px;margin:0 auto;padding:32px}
section{background:#fff;border:1px solid #dfe5ef;border-radius:14px;padding:24px;margin:16px 0}
button{font:inherit;padding:9px 14px;border:0;border-radius:8px;background:#1757e8;color:#fff;cursor:pointer}
button.secondary{background:#eef2ff;color:#1757e8}
input,select,textarea{font:inherit;padding:9px;border:1px solid #cbd5e1;border-radius:8px;box-sizing:border-box}
table{width:100%;border-collapse:collapse}
td,th{text-align:left;padding:10px;border-bottom:1px solid #e2e8f0;vertical-align:middle}
code{font-size:.9em}
.filters{display:flex;gap:10px;margin-bottom:16px;flex-wrap:wrap}
.filters input,.filters select{flex:1;min-width:140px}
.stat-grid{display:flex;gap:16px}
.stat{flex:1;background:#f7f9fc;border-radius:10px;padding:16px;text-align:center}
.stat b{font-size:1.6em;display:block}
.plan-edit{border:1px solid #dfe5ef;border-radius:10px;padding:16px;margin:10px 0}
.plan-edit textarea{width:100%;min-height:90px}
.plan-edit input{width:100%;margin:4px 0}
.msg{font-size:.9em;color:#166534;min-height:1.2em}
.err{color:#b91c1c}
.hidden{display:none}
</style></head><body><main>
<h1>SCOS Creator Console</h1>
<p>Platform administration. This view is only actionable for accounts with the persistent Creator role.</p>

<section>
  <h2>Platform overview</h2>
  <div class="stat-grid" id="stats"><div class="stat">Loading…</div></div>
</section>

<section>
  <h2>Customers</h2>
  <div class="filters">
    <input id="f-q" aria-label="Search name or email" oninput="debouncedLoadUsers()">
    <select id="f-plan" onchange="loadUsers()"><option value="">All plans</option></select>
    <select id="f-role" onchange="loadUsers()">
      <option value="">All roles</option>
      <option value="user">user</option>
      <option value="creator">creator</option>
    </select>
    <button class="secondary" onclick="loadUsers()">Refresh</button>
  </div>
  <table><thead><tr><th>Email</th><th>Plan</th><th>Role</th><th>Status</th><th>Usage (30d)</th><th></th></tr></thead><tbody id="users-body"></tbody></table>
</section>

<section>
  <h2>Tenant usage</h2>
  <p id="usage-hint">Click "Usage" on a customer row to see their last 30 days here.</p>
  <table><thead><tr><th>Date</th><th>API requests</th><th>Memory reads</th><th>Memory writes</th><th>Bytes stored</th></tr></thead><tbody id="usage-body"></tbody></table>
</section>

<section>
  <h2>Creator settings — plans and perks</h2>
  <p>Edit the pricing copy and perk list shown on the public portal for each tier. Changes take effect immediately and are recorded in the audit log.</p>
  <div id="plan-editors"></div>
</section>

<section>
  <h2>Audit log</h2>
  <table><thead><tr><th>When</th><th>Actor</th><th>Action</th><th>Target</th><th>Details</th></tr></thead><tbody id="audit-body"></tbody></table>
</section>

<script>
async function req(u,opts){
  let r=await fetch(u,Object.assign({headers:{'content-type':'application/json'}},opts||{}));
  let x=await r.json().catch(()=>({}));
  if(!r.ok) throw Error(x.error||JSON.stringify(x));
  return x;
}
function post(u,b){return req(u,{method:'POST',body:JSON.stringify(b)})}
function put(u,b){return req(u,{method:'PUT',body:JSON.stringify(b)})}

let allPlans=[];
let debounceTimer=null;
function debouncedLoadUsers(){clearTimeout(debounceTimer);debounceTimer=setTimeout(loadUsers,250)}

async function loadStats(){
  try{
    const s=await req('/api/admin/overview');
    stats.innerHTML='<div class="stat"><b>'+s.users+'</b>Users</div><div class="stat"><b>'+s.tenants+'</b>Tenants</div><div class="stat"><b>'+s.active_api_keys+'</b>Active API keys</div>';
  }catch(e){stats.innerHTML='<div class="stat err">'+e.message+'</div>'}
}

async function loadPlans(){
  allPlans=await req('/api/plans');
  const sel=f_plan;
  const current=sel.value;
  sel.innerHTML='<option value="">All plans</option>'+allPlans.map(p=>'<option value="'+p.id+'">'+p.name+'</option>').join('');
  sel.value=current;
  plan_editors.innerHTML=allPlans.map(p=>
    '<div class="plan-edit" id="pe-'+p.id+'">'+
      '<b>'+p.id+'</b>'+
      '<input id="pe-name-'+p.id+'" value="'+escapeAttr(p.name)+'" aria-label="Name">'+
      '<input id="pe-price-'+p.id+'" value="'+escapeAttr(p.price)+'" aria-label="Price">'+
      '<textarea id="pe-perks-'+p.id+'" aria-label="One perk per line">'+p.perks.join('\n')+'</textarea>'+ 
      '<button onclick="savePlan(\''+p.id+'\')">Save</button>'+
      '<div class="msg" id="pe-msg-'+p.id+'"></div>'+
    '</div>'
  ).join('');
}
function escapeAttr(s){return String(s).replace(/&/g,'&amp;').replace(/"/g,'&quot;').replace(/</g,'&lt;')}

async function savePlan(id){
  const msg=document.getElementById('pe-msg-'+id);
  try{
    const perks=document.getElementById('pe-perks-'+id).value.split('\n').map(s=>s.trim()).filter(Boolean);
    await put('/api/admin/plans',{id:id,name:document.getElementById('pe-name-'+id).value,price:document.getElementById('pe-price-'+id).value,perks:perks});
    msg.textContent='Saved.';
    msg.classList.remove('err');
    await Promise.all([loadPlans(),loadAudit()]);
  }catch(e){msg.textContent=e.message;msg.classList.add('err')}
}

async function loadUsers(){
  const params=new URLSearchParams();
  if(f_q.value) params.set('q',f_q.value);
  if(f_plan.value) params.set('plan',f_plan.value);
  if(f_role.value) params.set('role',f_role.value);
  try{
    const users=await req('/api/admin/users?'+params.toString());
    const planOpts=allPlans.map(p=>'<option value="'+p.id+'">'+p.name+'</option>').join('');
    users_body.innerHTML=users.map(u=>
      '<tr>'+
        '<td>'+u.email+'<br><code>'+u.id+'</code></td>'+
        '<td><select id="plan-'+u.id+'">'+planOpts+'</select></td>'+
        '<td><select id="role-'+u.id+'"><option value="user">user</option><option value="creator">creator</option></select></td>'+
        '<td>'+u.status+'</td>'+
        '<td><button class="secondary" onclick="loadTenantUsage(\''+u.tenant_id+'\')">Usage</button></td>'+
        '<td><button onclick="saveUser(\''+u.id+'\',\''+u.tenant_id+'\')">Save</button><div class="msg" id="umsg-'+u.id+'"></div></td>'+
      '</tr>'
    ).join('') || '<tr><td colspan="6">No customers match this filter.</td></tr>';
    for(const u of users){
      document.getElementById('plan-'+u.id).value=u.plan;
      document.getElementById('role-'+u.id).value=u.platform_role;
    }
  }catch(e){users_body.innerHTML='<tr><td colspan="6" class="err">'+e.message+'</td></tr>'}
}

async function saveUser(userID,tenantID){
  const msg=document.getElementById('umsg-'+userID);
  const plan=document.getElementById('plan-'+userID).value;
  const role=document.getElementById('role-'+userID).value;
  try{
    await post('/api/admin/set-plan',{tenant_id:tenantID,plan:plan});
    await post('/api/admin/set-role',{user_id:userID,role:role});
    msg.textContent='Saved.';
    msg.classList.remove('err');
    await Promise.all([loadStats(),loadAudit()]);
  }catch(e){msg.textContent=e.message;msg.classList.add('err')}
}

async function loadTenantUsage(tenantID){
  usage_hint.textContent='Last 30 days for tenant '+tenantID;
  try{
    const rows=await req('/api/admin/tenant-usage?tenant_id='+encodeURIComponent(tenantID));
    usage_body.innerHTML=rows.map(r=>'<tr><td>'+r.date+'</td><td>'+r.api_requests+'</td><td>'+r.memory_reads+'</td><td>'+r.memory_writes+'</td><td>'+r.bytes_stored+'</td></tr>').join('') || '<tr><td colspan="5">No usage recorded in this window.</td></tr>';
  }catch(e){usage_body.innerHTML='<tr><td colspan="5" class="err">'+e.message+'</td></tr>'}
}

async function loadAudit(){
  try{
    const rows=await req('/api/admin/audit');
    audit_body.innerHTML=rows.map(r=>'<tr><td>'+new Date(r.created_at).toLocaleString()+'</td><td>'+r.actor_email+'</td><td>'+r.action+'</td><td><code>'+r.target+'</code></td><td><code>'+JSON.stringify(r.details)+'</code></td></tr>').join('') || '<tr><td colspan="5">No admin actions recorded yet.</td></tr>';
  }catch(e){audit_body.innerHTML='<tr><td colspan="5" class="err">'+e.message+'</td></tr>'}
}

async function load(){
  await loadStats();
  await loadPlans();
  await loadUsers();
  await loadAudit();
}
load();
</script>
</main></body></html>`
