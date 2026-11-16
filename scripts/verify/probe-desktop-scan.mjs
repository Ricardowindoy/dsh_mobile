// 量桌面（模拟 Windows Tauri UA）下扫码区的真实形态。
import { createServer } from 'node:http'
import { readFileSync, existsSync, statSync } from 'node:fs'
import { extname, join, resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
const HERE = dirname(fileURLToPath(import.meta.url))
const DIST = resolve(HERE, '..', '..', 'mobile', 'dist')
const MIME = { '.html':'text/html; charset=utf-8', '.css':'text/css; charset=utf-8', '.js':'text/javascript; charset=utf-8', '.svg':'image/svg+xml' }
const server = createServer((req,res)=>{let p=decodeURIComponent(req.url.split('?')[0]);if(p==='/')p='/index.html';const f=join(DIST,p);if(!existsSync(f)||!statSync(f).isFile()){res.writeHead(404);res.end();return}res.writeHead(200,{'Content-Type':MIME[extname(f)]||'application/octet-stream'});res.end(readFileSync(f))})
await new Promise(r=>server.listen(0,'127.0.0.1',r)); const BASE=`http://127.0.0.1:${server.address().port}/`
const ver=await(await fetch('http://127.0.0.1:9222/json/version')).json()
const ws=new WebSocket(ver.webSocketDebuggerUrl); await new Promise((r,j)=>{ws.onopen=r;ws.onerror=j})
let seq=0;const pending=new Map()
ws.onmessage=e=>{const m=JSON.parse(e.data);if(m.id&&pending.has(m.id)){const{res,rej}=pending.get(m.id);pending.delete(m.id);m.error?rej(new Error(JSON.stringify(m.error))):res(m.result)}}
const send=(method,params={},sessionId)=>new Promise((res,rej)=>{const id=++seq;pending.set(id,{res,rej});ws.send(JSON.stringify({id,method,params,sessionId}))})
const {targetId}=await send('Target.createTarget',{url:'about:blank'})
const {sessionId}=await send('Target.attachToTarget',{targetId,flatten:true})
await send('Page.enable',{},sessionId);await send('Runtime.enable',{},sessionId)
await send('Emulation.setDeviceMetricsOverride',{width:960,height:720,deviceScaleFactor:1,mobile:false},sessionId)
await send('Emulation.setUserAgentOverride',{userAgent:'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36'},sessionId)
await send('Page.navigate',{url:BASE},sessionId); await new Promise(r=>setTimeout(r,900))
const {result}=await send('Runtime.evaluate',{sessionId,returnByValue:true,expression:`(()=>{
  const h=(el)=>!!el?.hasAttribute('hidden')
  return {
    scanSectionHidden: h(document.querySelector('.scan')),
    scanPairHidden: h(document.querySelector('#scan-pair')),
    scanGalleryHidden: h(document.querySelector('#scan-gallery')),
    scanPasteHidden: h(document.querySelector('#scan-paste')),
    scanNote: document.querySelector('.scan-note')?.textContent?.trim(),
    groupTitleHidden: h(document.querySelector('.group-h')),
    pageSub: document.getElementById('page-sub')?.textContent?.trim(),
  }
})()`},sessionId)
console.log(JSON.stringify(result.value,null,2))
server.close();process.exit(0)
