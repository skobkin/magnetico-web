const BASE="http://127.0.0.1:18081", PASSWORD="browser-check-pass-1";
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
(async()=>{
 const t=await (await fetch("http://127.0.0.1:9222/json/new?about:blank",{method:"PUT"})).json();
 const ws=new WebSocket(t.webSocketDebuggerUrl); await new Promise(r=>ws.onopen=r);
 let id=0; const p=new Map(); ws.onmessage=e=>{const m=JSON.parse(e.data); if(m.id&&p.has(m.id)){p.get(m.id)(m.result);p.delete(m.id)}};
 const send=(method,params={})=>new Promise(r=>{const i=++id;p.set(i,r);ws.send(JSON.stringify({id:i,method,params}))});
 const ev=async x=>(await send("Runtime.evaluate",{expression:x,returnByValue:true,awaitPromise:true})).result.value;
 const nav=async u=>{await send("Page.navigate",{url:u});await sleep(1800)};
 await send("Page.enable");
 await send("Emulation.setDeviceMetricsOverride",{width:360,height:800,deviceScaleFactor:1,mobile:true});
 await nav(BASE+"/login");
 await ev(`document.querySelector('input[name=username]').value='browser-admin';document.querySelector('input[name=password]').value=${JSON.stringify(PASSWORD)};document.querySelector('form').submit();1`);
 await sleep(1500);
 for (const path of process.argv.slice(2)) {
  await nav(BASE+path); await sleep(2500);
  const r=await ev(`(()=>{const W=document.documentElement.clientWidth; const out=[]; for(const e of document.querySelectorAll('body *')){const b=e.getBoundingClientRect(); if(b.width>0 && b.right>W+1){out.push(e.tagName.toLowerCase()+(e.id?'#'+e.id:'')+'.'+String(e.className).split(' ').join('.')+' right='+Math.round(b.right)+' w='+Math.round(b.width))}} return {W, sw:document.documentElement.scrollWidth, vv: window.innerWidth, n:out.length, first:out.slice(0,14)}})()`);
  console.log(path, JSON.stringify(r,null,1));
 }
 process.exit(0);
})();
