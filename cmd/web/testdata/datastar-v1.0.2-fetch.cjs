// Unmodified fetch/retry helper (Ln) from Datastar v1.0.2.
// https://raw.githubusercontent.com/starfederation/datastar/v1.0.2/bundles/datastar.js
// This excerpt runs only inside the bounded test VM with a mocked transport.
// The runtime version is checked against the actual rendered page by the Go test.
//
// License: https://raw.githubusercontent.com/starfederation/datastar/v1.0.2/LICENSE.md
// Copyright © Star Federation
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
// of the Software, and to permit persons to whom the Software is furnished to do
// so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.
Ln=(e,t)=>new Promise((n,r)=>{let s=t();if(!s)return;let{input:i,signal:o,headers:a,onopen:l,onmessage:c,onclose:u,openWhenHidden:f,fetch:m,retry:h="auto",retryInterval:v=1e3,retryScaler:d=2,retryMaxWait:g=3e4,retryMaxCount:b=10,responseOverrides:y,...$}=s,O={...a},C,de=()=>{if(C.abort(),!document.hidden){let S=t();if(!S)return;i=S.input,$.body=S.body,F()}};f||document.addEventListener("visibilitychange",de);let ie,q=()=>{document.removeEventListener("visibilitychange",de),clearTimeout(ie),C.abort()};o?.addEventListener("abort",()=>{q(),n()});let Y=m||window.fetch,Ge=l||(()=>{}),E=0,U=v,T=()=>{E<b?(re(Tn,e,{}),clearTimeout(ie),ie=setTimeout(F,v),E++,v=Math.min(v*d,g)):(re(An,e,{}),q(),r("Max retries reached."))},F=async()=>{C=new AbortController;let S=C.signal;try{let A=await Y(i,{...$,headers:O,signal:S});await Ge(A);let X=async(G,me,Be,we,...on)=>{let gt={[Be]:await me.text()};for(let We of on){let Ue=me.headers.get(`datastar-${ae(We)}`);if(we){let Me=we[We];Me&&(Ue=typeof Me=="string"?Me:JSON.stringify(Me))}Ue&&(gt[We]=Ue)}re(G,e,gt),q(),n()},M=A.status,pe=M===204,mt=M>=300&&M<400,sn=M>=400&&M<600;if(M!==200){if(u?.(),h!=="never"&&!pe&&!mt&&(h==="always"||h==="error"&&sn)){T();return}q(),n();return}E=0,v=U;let je=A.headers.get("Content-Type");if(je?.includes("text/html"))return await X("datastar-patch-elements",A,"elements",y,"selector","mode","namespace","useViewTransition");if(je?.includes("application/json"))return await X("datastar-patch-signals",A,"signals",y,"onlyIfMissing");if(je?.includes("text/javascript")){let G=document.createElement("script"),me=A.headers.get("datastar-script-attributes");if(me)for(let[Be,we]of Object.entries(JSON.parse(me)))G.setAttribute(Be,we);G.textContent=await A.text(),document.head.appendChild(G),q();return}if(await Rn(A.body,wn(Mn(G=>{G?O["last-event-id"]=G:delete O["last-event-id"]},G=>{U=v=G},c))),u?.(),h==="always"&&!mt){T();return}q(),n()}catch{if(!S.aborted)try{T()}catch(A){q(),r(A)}}};F()});

