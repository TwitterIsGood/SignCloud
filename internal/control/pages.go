package control

const manifestTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>items</key><array><dict>
<key>assets</key><array><dict><key>kind</key><string>software-package</string><key>url</key><string>%s</string></dict></array>
<key>metadata</key><dict><key>bundle-identifier</key><string>%s</string><key>bundle-version</key><string>%s</string><key>kind</key><string>software</string><key>title</key><string>%s</string></dict>
</dict></array></dict></plist>`

const installPage = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover"><meta name="color-scheme" content="light dark"><title>{{.Name}} - 安装</title><style>
:root{--bg:#f3f5f7;--card:#fff;--ink:#171a1f;--muted:#727a84;--line:#e3e7ec;--blue:#1769e0;--blue-soft:#eaf2fd;--ok:#1a7f4b;--ok-soft:#e7f6ee;--warn:#805c12;--warn-soft:#fff5df;--bad:#b42318;--bad-soft:#fdecea;--chip:#f1f3f6;--track:#edf0f4;--stage-bg:#f4f6f9;--panel:#fbfcfd;--qr-bg:#fbfcfd;--qr-ink:#727a84;--facts-muted:#7c848e;--note:#747c86;--button-bg:#1769e0;--button-ink:#fff;--secondary-bg:#fff;--secondary-border:#b9cff0;--notice-border:#efdaa7;--error-border:#f3c2bd;--log-bg:#14181f;--log-ink:#d7dde7;--shadow:0 12px 32px rgba(17,24,39,.06);font-family:Inter,-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif;color:var(--ink);background:var(--bg);letter-spacing:0}
*{box-sizing:border-box}html,body{max-width:100%;overflow-x:hidden}body{margin:0;min-height:100vh;min-height:100dvh;display:grid;place-items:center;padding:24px}
.shell{width:min(940px,100%);background:var(--card);border:1px solid var(--line);border-radius:14px;box-shadow:var(--shadow);display:grid;grid-template-columns:minmax(280px,340px) minmax(0,1fr);overflow:hidden}
.qr{min-width:0;padding:38px;border-right:1px solid var(--line);display:flex;flex-direction:column;align-items:center;justify-content:center;background:var(--qr-bg)}
.qr img{display:block;width:250px;max-width:100%;height:auto;image-rendering:pixelated;border-radius:8px}
.qr small{margin-top:16px;color:var(--qr-ink);text-align:center;font-size:13px}
.info{min-width:0;padding:38px 40px 40px}
.eyebrow{color:var(--blue);font-weight:700;font-size:12px;text-transform:uppercase;letter-spacing:.04em}
.info h1{font-size:28px;line-height:1.22;margin:10px 0 12px;overflow-wrap:anywhere}
.meta{display:flex;align-items:center;gap:10px;flex-wrap:wrap;color:var(--muted);margin-bottom:26px;font-size:14px}
.kind{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;background:var(--chip);border-radius:6px;padding:2px 7px}
.pill{display:inline-flex;align-items:center;gap:7px;padding:4px 11px;border-radius:999px;font-size:12px;font-weight:700;background:var(--chip);color:var(--muted)}
.pill::before{content:"";width:7px;height:7px;border-radius:50%;background:currentColor;flex:none}
.pill[data-status=running]{background:var(--blue-soft);color:var(--blue)}
.pill[data-status=running]::before{animation:pulse 1.4s ease-in-out infinite}
.pill[data-status=succeeded]{background:var(--ok-soft);color:var(--ok)}
.pill[data-status=failed]{background:var(--bad-soft);color:var(--bad)}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.35}}
.progress{margin:0 0 22px}
.progress-top{display:flex;align-items:baseline;justify-content:space-between;gap:10px 16px;font-size:13px;color:var(--muted);margin-bottom:9px;flex-wrap:wrap}
.progress-top b{color:var(--ink);font-weight:600}
.track{position:relative;height:8px;border-radius:999px;background:var(--track);overflow:hidden}
.bar{position:relative;height:100%;width:0;border-radius:999px;background:linear-gradient(90deg,#1769e0,#5aa2ff);transition:width .6s ease}
.bar.busy::after{content:"";position:absolute;inset:0;background:linear-gradient(90deg,transparent,rgba(255,255,255,.6),transparent);animation:sheen 1.6s linear infinite}
.bar.failed{background:linear-gradient(90deg,#b42318,#e4635a)}
@keyframes sheen{from{transform:translateX(-100%)}to{transform:translateX(100%)}}
.track.indeterminate .bar{width:38%;animation:slide 1.5s ease-in-out infinite}
@keyframes slide{0%{transform:translateX(-110%)}100%{transform:translateX(300%)}}
.stages{display:flex;flex-wrap:wrap;gap:6px;margin-top:12px}
.stage{font-size:12px;line-height:1.4;color:var(--muted);background:var(--stage-bg);border:1px solid var(--line);padding:4px 10px;border-radius:999px}
.stage.done{color:var(--ok);background:var(--ok-soft);border-color:transparent}
.stage.current{color:var(--blue);background:var(--blue-soft);border-color:transparent;font-weight:700}
.stage.failed{color:var(--bad);background:var(--bad-soft);border-color:transparent;font-weight:700}
.facts{display:grid;grid-template-columns:96px minmax(0,1fr);gap:12px 18px;font-size:14px;margin-bottom:30px}
.facts span{color:var(--facts-muted)}.facts strong{min-width:0;overflow-wrap:anywhere;word-break:break-word;font-weight:600}
.button{display:block;width:100%;text-align:center;text-decoration:none;background:var(--button-bg);color:var(--button-ink);padding:13px 18px;border-radius:9px;font-weight:700;margin-top:10px;font-size:15px}
.button.secondary{background:var(--secondary-bg);color:var(--blue);border:1px solid var(--secondary-border)}
.note{max-width:100%;font-size:12px;color:var(--note);line-height:1.6;margin:20px 0 0;overflow-wrap:anywhere}
.notice{padding:12px 14px;background:var(--warn-soft);color:var(--warn);border:1px solid var(--notice-border);border-radius:9px;font-size:14px}
.error{padding:12px 14px;background:var(--bad-soft);color:var(--bad);border:1px solid var(--error-border);border-radius:9px;font-size:13px;margin-top:12px;white-space:pre-wrap;overflow-wrap:anywhere}
details.panel{margin-top:22px;border:1px solid var(--line);border-radius:10px;background:var(--panel);overflow:hidden}
details.panel>summary{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:13px 15px;cursor:pointer;font-size:14px;font-weight:600;list-style:none;user-select:none}
details.panel>summary::-webkit-details-marker{display:none}
.chev{color:var(--muted);transition:transform .18s ease;font-size:12px}
details[open] .chev{transform:rotate(90deg)}
.detail-body{padding:0 15px 15px}
.detail-body .facts{margin:4px 0 0;gap:10px 16px;font-size:13px}
.section-label{display:flex;align-items:center;justify-content:space-between;gap:10px;margin:16px 0 8px;font-size:12px;font-weight:700;color:var(--muted);text-transform:uppercase;letter-spacing:.04em}
pre.log{margin:0;padding:13px 14px;background:var(--log-bg);color:var(--log-ink);border-radius:9px;font:12px/1.55 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;white-space:pre-wrap;word-break:break-word;max-height:340px;overflow:auto;-webkit-overflow-scrolling:touch}
.empty{color:var(--muted);font-size:12px;font-weight:400;text-transform:none;letter-spacing:0}
@media(max-width:720px){body{display:block;padding:0;background:var(--card)}.shell{width:100%;min-height:100dvh;border:0;border-radius:0;box-shadow:none;grid-template-columns:1fr;align-content:start}.qr{border-right:0;border-bottom:1px solid var(--line);padding:22px}.qr img{width:184px}.qr small{margin-top:12px}.info{padding:24px 20px 40px}.info h1{font-size:24px}.meta{margin-bottom:22px}.facts{grid-template-columns:86px minmax(0,1fr);gap:10px 14px;margin-bottom:24px}.button{padding:14px 18px}pre.log{max-height:44vh}.detail-body{padding:0 12px 12px}.progress-top{display:block;font-size:12px}.progress-top>span{display:block}.progress-top>span+span{margin-top:4px}}
@media(max-width:380px){.qr img{width:156px}.info{padding-inline:16px}.facts{grid-template-columns:1fr;gap:3px}.facts strong{margin-bottom:8px}.detail-body .facts{grid-template-columns:1fr}}
@media(prefers-reduced-motion:reduce){.bar.busy::after,.track.indeterminate .bar,.pill[data-status=running]::before{animation:none}}
@media(prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#161a20;--ink:#e8ecf2;--muted:#98a2b0;--line:#2a313b;--blue:#6aa8ff;--blue-soft:#16253a;--ok:#4ad08b;--ok-soft:#10261b;--warn:#e0b96a;--warn-soft:#2a2113;--bad:#ff8a7a;--bad-soft:#2d1614;--chip:#222a34;--track:#222a34;--stage-bg:#1d232b;--panel:#1a1f26;--qr-bg:#fff;--qr-ink:#5b6470;--facts-muted:#8f99a6;--note:#8b94a1;--button-bg:#2f7de1;--button-ink:#fff;--secondary-bg:#1a2029;--secondary-border:#34507a;--notice-border:#4a3b1c;--error-border:#4a2521;--log-bg:#0c1015;--log-ink:#c9d2de;--shadow:0 12px 32px rgba(0,0,0,.55)}.bar.busy::after{background:linear-gradient(90deg,transparent,rgba(255,255,255,.18),transparent)}}
</style></head><body><main class="shell" id="page" data-status="{{.Status}}" data-status-url="/install/{{.InstallToken}}/status" data-created="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}" data-updated="{{.UpdatedAt.Format "2006-01-02T15:04:05Z07:00"}}">
<section class="qr"><img src="{{.QRURL}}" alt="安装二维码"><small>{{if eq .Status "succeeded"}}使用 iPhone 相机扫描此二维码{{else}}扫码在 iPhone 上查看构建进度{{end}}</small></section>
<section class="info"><div class="eyebrow">SignCloud iOS Distribution</div><h1>{{.Name}}</h1>
<div class="meta"><span class="pill" id="status-pill" aria-live="polite" data-status="{{.Status}}">{{.StatusText}}</span><span class="kind">{{.Kind}}</span></div>
{{if eq .Status "succeeded"}}<div class="facts"><span>Bundle ID</span><strong>{{if .BundleID}}{{.BundleID}}{{else}}-{{end}}</strong><span>版本</span><strong>{{if .Version}}{{.Version}}{{else}}-{{end}}</strong><span>最低系统</span><strong>{{if .MinimumOS}}{{.MinimumOS}}{{else}}-{{end}}</strong><span>更新时间</span><strong><time datetime="{{.UpdatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{.UpdatedAt.Format "2006-01-02 15:04:05"}}</time></strong></div>
<a class="button" href="{{.SafeOTAURL}}">安装到 iPhone</a><a class="button secondary" href="{{.ArtifactURL}}">下载 IPA</a>
<p class="note">安装需要 HTTPS、受信任的签名和包含当前设备 UDID 的 Development/Ad Hoc 描述文件。</p>
{{else}}<div class="progress" id="progress"><div class="progress-top"><span>构建进度</span><span><b id="progress-label">{{.Progress.Label}}</b> · <span id="elapsed">{{.ElapsedText}}</span></span></div>
<div class="track{{if not .Progress.HasPercent}} indeterminate{{end}}" id="track"><div class="bar{{if .Progress.Busy}} busy{{end}}{{if eq .Status "failed"}} failed{{end}}" id="bar" style="width:{{.Progress.Width}}%"></div></div>
<div class="stages" id="stages">{{range .Progress.Stages}}<span class="stage {{.State}}">{{.Label}}</span>{{end}}</div></div>
{{if eq .Status "failed"}}<div class="error" id="error">{{if .Error}}{{.Error}}{{else}}构建失败{{end}}</div>{{else if eq .Status "canceled"}}<div class="notice">构建已取消。</div>{{else}}<div class="notice" id="pending">构建尚未完成，页面会自动刷新状态。</div><noscript><div class="notice" style="margin-top:12px">请稍后手动刷新页面。</div></noscript>{{end}}{{end}}
<details class="panel" id="details"{{if .ShowDetails}} open{{end}}><summary><span>构建信息与日志</span><span class="chev">▶</span></summary><div class="detail-body">
<div class="facts"><span>Bundle ID</span><strong>{{if .BundleID}}{{.BundleID}}{{else}}待构建完成{{end}}</strong><span>版本</span><strong>{{if .Version}}{{.Version}}{{else}}-{{end}}</strong><span>最低系统</span><strong>{{if .MinimumOS}}{{.MinimumOS}}{{else}}-{{end}}</strong><span>构建号</span><strong>{{if .BuildNumber}}{{.BuildNumber}}{{else}}-{{end}}</strong><span>架构</span><strong>{{if .Architectures}}{{range $i, $a := .Architectures}}{{if $i}}, {{end}}{{$a}}{{end}}{{else}}-{{end}}</strong><span>签名</span><strong>{{if .Signing}}{{.Signing}}{{else}}-{{end}}</strong><span>创建时间</span><strong><time datetime="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{.CreatedAt.Format "2006-01-02 15:04:05"}}</time></strong><span>更新时间</span><strong><time datetime="{{.UpdatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{.UpdatedAt.Format "2006-01-02 15:04:05"}}</time></strong>{{if .SHA256}}<span>SHA-256</span><strong style="font-family:ui-monospace,Menlo,monospace;font-size:12px">{{.SHA256}}</strong>{{end}}</div>
<div class="section-label"><span>构建日志</span><span id="log-meta" class="empty">{{if .LogsTail}}末尾 {{len .LogsTail}} 字符{{else}}暂无日志{{end}}</span></div>
<pre class="log" id="log">{{if .LogsTail}}{{.LogsTail}}{{else}}（暂无日志）{{end}}</pre>
</div></details>
</section></main>
<script>
(function(){
'use strict';
var page=document.getElementById('page');
if(!page)return;
var statusUrl=page.getAttribute('data-status-url');
var status=page.getAttribute('data-status')||'';
var check=document.getElementById('status-pill');
var stagesEl=document.getElementById('stages');
var track=document.getElementById('track');
var bar=document.getElementById('bar');
var labelEl=document.getElementById('progress-label');
var elapsedEl=document.getElementById('elapsed');
var details=document.getElementById('details');
var logEl=document.getElementById('log');
var logMeta=document.getElementById('log-meta');
var pendingEl=document.getElementById('pending');
var errorEl=document.getElementById('error');
var statusLabels={queued:'排队中',running:'构建中',succeeded:'已完成',failed:'构建失败',canceled:'已取消'};
var stageLabels={preparing:'准备',generating_project:'生成工程',building:'编译',signing:'签名',packaging:'打包',notarizing:'公证',stapling:'装订',validating:'校验',uploading:'上传'};
var terminal={succeeded:1,failed:1,canceled:1};
function parse(s){if(!s)return null;var d=new Date(s);return isNaN(d.getTime())?null:d;}
var start=parse(page.getAttribute('data-created'));
var end=parse(page.getAttribute('data-updated'));
function pad(n){return n<10?'0'+n:''+n;}
function fmt(d){return d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+' '+pad(d.getHours())+':'+pad(d.getMinutes())+':'+pad(d.getSeconds());}
Array.prototype.forEach.call(document.querySelectorAll('time[datetime]'),function(t){var d=parse(t.getAttribute('datetime'));if(d)t.textContent=fmt(d);});
function elapsedText(){if(!start)return '';var endMs=(terminal[status]&&end)?end.getTime():Date.now();var s=Math.max(0,Math.floor((endMs-start.getTime())/1000));var m=Math.floor(s/60),h=Math.floor(m/60);if(h>0)return '已用时 '+h+' 小时 '+(m%60)+' 分';if(m>0)return '已用时 '+m+' 分 '+(s%60)+' 秒';return '已用时 '+s+' 秒';}
if(elapsedEl&&start){elapsedEl.textContent=elapsedText();setInterval(function(){if(!terminal[status])elapsedEl.textContent=elapsedText();},1000);}
function setStatus(next){if(!next)return;status=next;if(check){check.setAttribute('data-status',status);check.textContent=statusLabels[status]||status;}}
function renderStages(list){if(!stagesEl||!list)return;var html='';for(var i=0;i<list.length;i++){var s=list[i];html+='<span class="stage '+(s.state||'')+'">'+(s.label||stageLabels[s.key]||s.key)+'</span>';}stagesEl.innerHTML=html;}
function renderProgress(p){if(!p||!track||!bar||!labelEl)return;if(p.percent===null||p.percent===undefined){track.classList.add('indeterminate');bar.style.width='38%';}else{track.classList.remove('indeterminate');bar.style.width=p.percent+'%';}var stageText=terminal[status]?(statusLabels[status]||'进行中'):(p.stage&&stageLabels[p.stage]?stageLabels[p.stage]:'进行中');labelEl.textContent=stageText+(p.percent===null||p.percent===undefined?'':' · '+p.percent+'%');if(terminal[status])bar.classList.remove('busy');else bar.classList.add('busy');if(status==='failed')bar.classList.add('failed');else bar.classList.remove('failed');renderStages(p.stages);}
function nearBottom(el){return el.scrollHeight-el.scrollTop-el.clientHeight<48;}
function setLogs(text){if(!logEl||typeof text!=='string')return;var stick=nearBottom(logEl);logEl.textContent=text||'（暂无日志）';if(logMeta)logMeta.textContent=text?('末尾 '+text.length+' 字符'):'暂无日志';if(stick)logEl.scrollTop=logEl.scrollHeight;}
function showError(message){if(!errorEl){errorEl=document.createElement('div');errorEl.className='error';errorEl.id='error';var anchor=document.getElementById('progress');if(anchor&&anchor.parentNode){anchor.parentNode.insertBefore(errorEl,anchor.nextSibling);}}
if(errorEl)errorEl.textContent=message||'构建失败';if(pendingEl)pendingEl.style.display='none';}
function apply(d){if(!d)return;setStatus(d.status);if(d.updated_at){var u=parse(d.updated_at);if(u)end=u;}if(d.logs!==undefined)setLogs(d.logs);if(status==='succeeded'){location.reload();return;}if(d.progress)renderProgress(d.progress);if(status==='failed'){showError(d.error);if(details)details.open=true;}else if(status==='canceled'){if(pendingEl){pendingEl.textContent='构建已取消。';}}}
var busy=false;
function poll(withLogs){if(busy||terminal[status])return;busy=true;var url=statusUrl+(withLogs?'?logs=1':'');fetch(url,{cache:'no-store'}).then(function(r){return r.ok?r.json():null;}).then(function(d){busy=false;if(!d)return;apply(d);if(terminal[status]&&details&&details.open&&d.logs===undefined){fetch(statusUrl+'?logs=1',{cache:'no-store'}).then(function(r){return r.json();}).then(function(x){if(x&&typeof x.logs==='string')setLogs(x.logs);}).catch(function(){});}}).catch(function(){busy=false;});}
var logsLoaded=false;
function fetchLogs(){if(logsLoaded)return;logsLoaded=true;fetch(statusUrl+'?logs=1',{cache:'no-store'}).then(function(r){return r.json();}).then(function(d){if(d&&typeof d.logs==='string')setLogs(d.logs);}).catch(function(){logsLoaded=false;});}
if(!terminal[status]){setInterval(function(){poll(details&&details.open);},3000);setTimeout(function(){poll(details&&details.open);},600);}
if(details){details.addEventListener('toggle',function(){if(!details.open)return;setTimeout(function(){if(terminal[status])fetchLogs();else poll(true);},120);});}
})();
</script>
</body></html>`
