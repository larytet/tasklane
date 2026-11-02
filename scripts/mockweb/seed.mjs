// The sample data behind the web UI mock: two projects, five people, a scrum board with an active sprint, comments and
// reactions, created through the real API of a fresh server. Run against an empty database, then record.mjs:
//   TASKLANE_ADMIN_EMAIL=admin@example.com TASKLANE_ADMIN_PASSWORD=adminpass1 TASKLANE_LISTEN=127.0.0.1:18282 tasklane serve
//   node scripts/mockweb/seed.mjs && node scripts/mockweb/record.mjs
const B=process.env.TASKLANE_MOCK_URL||'http://127.0.0.1:18282';
async function api(ck,m,p,b){const r=await fetch(B+p,{method:m,headers:{'Content-Type':'application/json','X-Requested-With':'tasklane',...(ck?{Cookie:ck}:{})},body:b?JSON.stringify(b):undefined});const t=await r.text();const o={s:r.status,ck:(r.headers.get('set-cookie')||'').split(';')[0],d:t?JSON.parse(t):null};if(r.status>=300)console.error(m,p,r.status,t.slice(0,200));return o;}
const login=async(e,pw='password123')=>(await api(null,'POST','/api/v1/login',{email:e,password:pw})).ck;
const root=await login('admin@example.com','adminpass1');
const people=[['maya@example.com','Maya Chen',true],['sam@example.com','Sam Ortiz'],['priya@example.com','Priya Nair'],['leo@example.com','Leo Brandt'],['ines@example.com','Ines Duarte']];
const id={},ck={};
await api(root,'POST','/api/v1/projects',{key:'SHOP',name:'Web Shop'});
await api(root,'POST','/api/v1/projects',{key:'OPS',name:'Platform Ops'});
for(const [e,n,adm] of people){const u=await api(root,'POST','/api/v1/users',{email:e,name:n,password:'password123',admin:!!adm});id[n]=u.d.id;
  for(const p of ['SHOP','OPS']) await api(root,'POST',`/api/v1/projects/${p}/members`,{user:u.d.id,role:adm?'admin':'member'});
  ck[n]=await login(e);}
const M=ck['Maya Chen'];
const board=(await api(M,'POST','/api/v1/projects/SHOP/boards',{name:'Web Shop board',kind:'scrum'})).d;
await api(M,'POST','/api/v1/projects/OPS/boards',{name:'Ops board',kind:'kanban'});
const I=[ // type, summary, pri, assignee, labels, status, est, parent idx, reporter, desc
 ['Epic','Checkout redesign','high','Maya Chen',['checkout'],'In Progress',null,null,'Maya Chen','One page checkout with saved cards, address autocomplete and a clear order summary.\n\nSuccess: checkout conversion up 5% on mobile.'],
 ['Epic','Search that understands typos','medium','Priya Nair',['search'],'To Do',null,null,'Maya Chen','Fuzzy matching, synonyms and a "did you mean" line on the results page.'],
 ['Bug','Checkout fails on 3DS challenge','highest','Sam Ortiz',['payments','checkout'],'In Progress',3,0,'Ines Duarte','Steps:\n\n1. Add any item to the cart\n2. Pay with a card that asks for a 3DS challenge\n3. Complete the challenge\n\nExpected: the order is created.\n\nActual: the page returns to the cart with "Payment failed" although the bank shows the charge as authorised.\n\nRunbook: https://example.com/runbook/payments'],
 ['Story','Save cards for returning customers','high','Leo Brandt',['payments'],'In Progress',5,0,'Maya Chen','As a returning customer I want my card remembered so I can pay with one click.\n\nCards are tokenised by the payment provider; we keep only the token, brand and last four digits.'],
 ['Story','Address autocomplete on the shipping step','medium','Priya Nair',['checkout'],'To Do',3,0,'Maya Chen','Suggest addresses as the customer types; fall back to manual entry when the lookup is slow or unavailable.'],
 ['Task','Order summary sidebar on mobile','medium','Ines Duarte',['checkout','mobile'],'Done',2,0,'Leo Brandt','Collapse the summary into a sticky bar on narrow screens.'],
 ['Bug','Coupon field accepts expired codes','high','Sam Ortiz',['checkout'],'Done',1,0,'Priya Nair','An expired code is accepted and then rejected at payment, after the customer has filled everything in.'],
 ['Story','Guest checkout without an account','medium','Leo Brandt',['checkout'],'To Do',5,0,'Maya Chen','Let customers pay without signing up; offer to create an account on the confirmation page.'],
 ['Task','Track checkout funnel events','low','Priya Nair',['analytics'],'In Progress',2,0,'Maya Chen','Emit step_viewed and step_completed with the cart value bucket.'],
 ['Story','Did-you-mean suggestions','medium','Priya Nair',['search'],'To Do',3,1,'Maya Chen','Show one suggestion when a query returns fewer than three results.'],
 ['Task','Synonym list for the catalogue','low',null,['search'],'To Do',2,1,'Priya Nair','Start with sizes and colours ("grey" = "gray", "XL" = "extra large").'],
 ['Bug','Search results jump when images load','medium','Ines Duarte',['search','frontend'],'In Progress',1,null,'Leo Brandt','Reserve image space with width and height attributes so the list does not shift.'],
 ['Bug','Wishlist badge shows the wrong count after sign-in','low','Leo Brandt',['frontend'],'To Do',1,null,'Sam Ortiz','The badge keeps the guest count until the page is reloaded.'],
 ['Story','Back in stock email','medium',null,['email'],'To Do',3,null,'Maya Chen','Customers can ask to be told when a sold-out size returns.'],
 ['Task','Upgrade the image CDN client','low','Sam Ortiz',['infra'],'Done',1,null,'Sam Ortiz','The old client is end of life in December.'],
 ['Story','Product page reviews with photos','medium','Ines Duarte',['catalogue'],'To Do',5,null,'Maya Chen','Let verified buyers attach up to three photos to a review.'],
 ['Bug','Price shows two decimals for JPY','medium','Priya Nair',['i18n'],'Done',1,null,'Ines Duarte','Yen has no minor unit; format with the currency\'s own digits.'],
 ['Task','Accessibility pass on the cart','high','Leo Brandt',['a11y'],'In Progress',3,null,'Maya Chen','Keyboard order, labels on quantity steppers, and contrast on the "remove" link.'],
 ['Story','Gift wrapping option','low',null,['checkout'],'To Do',2,0,'Leo Brandt','A paid option per order with a short message.'],
 ['Bug','Order confirmation email missing VAT line','high','Sam Ortiz',['email','payments'],'To Do',2,null,'Ines Duarte','EU orders need the VAT amount and rate on the confirmation.'],
 ['Task','Load test the new checkout','medium','Sam Ortiz',['perf','checkout'],'To Do',3,0,'Maya Chen','500 checkouts a minute for 30 minutes against staging.'],
 ['Story','Size guide per brand','low','Ines Duarte',['catalogue'],'To Do',3,null,'Priya Nair','Brands cut differently; show the brand\'s own chart.'],
];
const keys=[];
for(const [t,s,pri,asg,lab,st,est,par,rep,desc] of I){
  const r=await api(ck[rep],'POST','/api/v1/issues',{project:'SHOP',type:t,summary:s,priority:pri,assignee:asg?id[asg]:'',labels:lab,description:desc,parent:par!==null?keys[par]:''});
  keys.push(r.d.key);
  if(est!==null) await api(M,'PATCH','/api/v1/issues/'+r.d.key,{estimate:est});
  if(st!=='To Do'){
    const v=(await api(M,'GET','/api/v1/issues/'+r.d.key+'/view')).d;
    if(st==='Done'){const tr=v.transitions.find(x=>x.to==='In Progress');await api(M,'POST','/api/v1/issues/'+r.d.key+'/transitions',{transition:tr.id});const v2=(await api(M,'GET','/api/v1/issues/'+r.d.key+'/view')).d;await api(M,'POST','/api/v1/issues/'+r.d.key+'/transitions',{transition:v2.transitions.find(x=>x.to==='Done').id});}
    else {const tr=v.transitions.find(x=>x.to===st);await api(M,'POST','/api/v1/issues/'+r.d.key+'/transitions',{transition:tr.id});}
  }
}
// comments on the 3DS bug
const bug=keys[2];
const C=[['Ines Duarte','Reproduced on staging with the Visa test card ending 3220. The provider webhook arrives about 4 s after the redirect.'],
 ['Sam Ortiz','We read the payment status on redirect and treat "pending" as failed. Fix: wait for the webhook, or poll for up to 10 s before giving up.'],
 ['Maya Chen','Polling is fine for now. Please add a test for the slow-webhook case so this does not come back.'],
 ['Sam Ortiz','Branch is up: payments/3ds-pending. Draft PR links to this ticket.']];
const cids=[];
for(const [w,b] of C){cids.push((await api(ck[w],'POST','/api/v1/issues/'+bug+'/comments',{body:b})).d.id);}
await api(ck['Maya Chen'],'PUT',`/api/v1/issues/${bug}/comments/${cids[1]}/reactions`,{emoji:'👍',on:true});
await api(ck['Ines Duarte'],'PUT',`/api/v1/issues/${bug}/comments/${cids[1]}/reactions`,{emoji:'👍',on:true});
await api(ck['Sam Ortiz'],'PUT',`/api/v1/issues/${bug}/comments/${cids[2]}/reactions`,{emoji:'🎉',on:true});
await api(ck['Leo Brandt'],'POST','/api/v1/issues/'+keys[3]+'/comments',{body:'Provider sandbox keys are in the vault under payments/sandbox.'});
await api(ck['Priya Nair'],'POST','/api/v1/issues/'+keys[3]+'/comments',{body:'Can we show the card brand icon next to the last four digits?'});
await api(M,'POST','/api/v1/issues/'+bug+'/links',{type:'relates',target:keys[6]});
// sprints
const sps=(await api(M,'GET',`/api/v1/boards/${board.id}/sprints`)).d;
console.log('sprints',JSON.stringify(sps).slice(0,400));
let s1=Array.isArray(sps)&&sps[0];
if(!s1) s1=(await api(M,'POST',`/api/v1/boards/${board.id}/sprints`,{name:'Sprint 14',goal:'Ship saved cards and fix the 3DS failure'})).d;
else await api(M,'PATCH',`/api/v1/sprints/${s1.id}`,{goal:'Ship saved cards and fix the 3DS failure'});
await api(M,'POST',`/api/v1/boards/${board.id}/plan`,{sprint:s1.id,keys:[2,3,4,5,6,8,11,17,12].map(i=>keys[i])});
await api(M,'POST',`/api/v1/sprints/${s1.id}/start`);
const sps2=(await api(M,'GET',`/api/v1/boards/${board.id}/sprints`)).d;console.log('sprints after',JSON.stringify(sps2).slice(0,600));
// OPS issues
for(const [t,s,a,st] of [['Task','Rotate the database credentials','Sam Ortiz','In Progress'],['Bug','Nightly backup job timed out twice','Leo Brandt','To Do'],['Task','Move logs to the new retention policy','Ines Duarte','Done']]){
  const r=await api(M,'POST','/api/v1/issues',{project:'OPS',type:t,summary:s,assignee:id[a]});
  if(st!=='To Do'){const v=(await api(M,'GET','/api/v1/issues/'+r.d.key+'/view')).d;await api(M,'POST','/api/v1/issues/'+r.d.key+'/transitions',{transition:v.transitions.find(x=>x.to==='In Progress').id});
   if(st==='Done'){const v2=(await api(M,'GET','/api/v1/issues/'+r.d.key+'/view')).d;await api(M,'POST','/api/v1/issues/'+r.d.key+'/transitions',{transition:v2.transitions.find(x=>x.to==='Done').id});}}
}
// maya was mentioned / notified: comment by others on maya's issue
await api(ck['Priya Nair'],'POST','/api/v1/issues/'+keys[0]+'/comments',{body:'Design review is Thursday; I added the mobile mocks to the epic.'});
for(const n of Object.keys(ck)) await api(ck[n],'PUT','/api/v1/me/ui-prefs',{hints_seen:['create','views','settings']});
console.log(JSON.stringify({keys,board:board.id,me:id['Maya Chen']}));
