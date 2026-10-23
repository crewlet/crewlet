import{r as e}from"./rolldown-runtime-hePW80VL.js";import{i as t,t as n}from"./react-DlYQp10C.js";import{$ as r,At as i,dn as ee,t as te,un as ne}from"./chunk-ZI5G5RS7-DxeJltpr.js";import{Ai as a,Ta as o,_n as s,gn as c,hn as l,wa as re,wi as u}from"./seats-CohWJma4.js";import{t as ie}from"./chunk-TVSHR35W-BkBjLkex.js";import{n as ae}from"./chunk-DL5ERIFF-DH0rmUPU.js";import{n as oe,t as se}from"./chunk-UYZZV4DZ-D-RVpQJc.js";import"./work-B8nvlxKi.js";import{r as ce}from"./chunk-PWG3VHRQ-OvwtLfls.js";import{I as le,L as ue,a as de,c as fe,d,f as pe,i as me,l as he,n as ge,o as f,p,r as m,s as h,t as _e,u as ve}from"./AskAgent-Huuu_5sN.js";import{t as ye}from"./access-BZRUQrHu.js";var g=e(t(),1),_=[{key:`meeting`,label:`Meeting notes`,description:`Who came, what was discussed, what was decided and who does what next`,titleExample:`Weekly planning, October 20`,body:`**Date:** the day it met · **Attendees:** who came

## Agenda

1. The first topic.
2. The second topic.

## Notes

What was discussed, topic by topic.

## Decisions

- What was decided, and why.

## Action items

- [ ] Owner: what, by when.
`},{key:`decision`,label:`Decision record`,description:`The context, the options weighed, what was decided and why`,titleExample:`Move session storage to Postgres`,body:`**Status:** proposed · **Deciders:** who decides

## Context

The problem, and the forces at play.

## Options

1. **Option A.** What it is, and what it costs.
2. **Option B.** What it is, and what it costs.

## Decision

What was chosen, and why it beats the alternatives.

## Consequences

What becomes easier, what becomes harder, and what to revisit.
`},{key:`brief`,label:`Project brief`,description:`The problem, the goals, the scope and the milestones of a piece of work`,titleExample:`Self-serve billing`,body:`## Summary

The project in two sentences: the problem, and the outcome.

## Goals

- What success looks like, measurably.

## Non-goals

- What this project deliberately leaves out.

## Scope

What ships, and what is left for later.

## Milestones

| Milestone | Owner | Date |
| --- | --- | --- |
| The first milestone | Who | When |

## Risks

- What could go wrong, and how it is handled.
`},{key:`runbook`,label:`Runbook`,description:`The steps to follow when something specific happens, and how to undo them`,titleExample:`Database failover`,body:`When to use this runbook, in one sentence.

## Before you start

- The access you need.
- The people to tell.

## Steps

1. The first step, and what you should see.
2. The next step.

## Check it worked

How to confirm the system is healthy again.

## Roll back

How to undo the steps if they make things worse.

## Escalate

Who to call, and when.
`},{key:`retro`,label:`Retrospective`,description:`What went well, what did not and what the team will change`,titleExample:`Q3 launch retrospective`,body:`**Period:** the weeks it covers · **Team:** who took part

## What went well

- Something to keep doing.

## What did not go well

- Something that slowed the team down.

## What we will change

- [ ] Owner: the change, by when.
`},{key:`howto`,label:`How-to guide`,description:`A task explained step by step for someone doing it for the first time`,titleExample:`Set up a local environment`,body:`What this guide helps you do, and who it is for.

## Before you begin

- What you need first.

## Steps

1. The first step, and what you should see.
2. The next step.

## Troubleshooting

**Symptom.** What to try.
`}];function v(e){return _.find(t=>t.key===e)}var y=n(),be=[{key:`onboarding`,label:`Onboarding guide`,brief:`A one-page guide for new engineers on how we review incidents: the timeline, the roles and the follow-ups. Draw on the last three reviews in Knowledge.`},{key:`proposal`,label:`Proposal`,brief:`A proposal for the change we discussed: the problem, the options we weighed, the one we recommend and what it costs. Written for the leads who decide.`},{key:`summary`,label:`Summary of a thread`,brief:`A summary of where the project stands for someone joining it today: what was decided, what is open, who owns what and where the work lives.`}];function b(e,t,n){let r=e.trim();return t.trim()===``?r:n&&t===n.body?`${r}\n\nFollow the ${n.label.toLowerCase()} outline:\n\n${t.trim()}`:`${r}\n\nBuild on this first draft:\n\n${t.trim()}`}function x(){let e=c(),t=pe(),n=he(),x=h(`doc`),S=ye({app:!0}),C=p(),w=ue(),T=f(),[E]=s(`template`,``),[D,O]=(0,g.useState)(()=>v(E)),[k,A]=(0,g.useState)(``),[j,M]=(0,g.useState)(D?.body??``),[N,xe]=(0,g.useState)(``),[Se,Ce]=(0,g.useState)(0),P=(0,g.useRef)(null),we=(0,g.useRef)(null),[F,I]=(0,g.useState)(null),L=(0,g.useRef)(!1),R=k.trim(),z=j.trim()!==``,B=!z||D!==void 0&&j===D.body,V=ae(j),H=d(k),U=me(x)?.seat,W=b(N,j,D),G=fe(`doc`,W),K=a(G),q=t.busy||n.busy,J=S.can?ve(k):S.reason,Y=n.held??(U?N.trim()===``?`Say what the doc should cover.`:ge(k,`doc`)??(x.project?K>32768?`The brief and the draft are ${K.toLocaleString()} bytes, and a task holds at most ${32768 .toLocaleString()}.`:void 0:`Choose the project to file the task in.`):`Add an agent to the chart first.`),X=R!==``||N.trim()!==``||z&&!B;l(X?(e,t)=>!L.current&&(I(()=>t),!0):null);let Z=()=>X?I(()=>w):w(),Q=async()=>{J||q||(L.current=!0,await t.fromStarter(`doc`,R,z?{doc:{body:j}}:{}),L.current=!1)},Te=async()=>{if(Y||q)return;L.current=!0;let t=await n.fileArtifactTask({kind:`doc`,title:R,brief:W,seat:x.seat,project:x.project});t?.item&&e.to([`work`,t.item]),L.current=!1},Ee=e=>{let t=e?v(e):void 0;O(t),M(t?.body??``),Ce(e=>e+1)},De=e=>{(e.key!==`Enter`||!e.metaKey&&!e.ctrlKey)&&(e.key===`Enter`||e.key===`ArrowDown`?(e.preventDefault(),P.current?.focus()):e.key===`Escape`&&(e.preventDefault(),Z()))},$=t.refusal&&{title:`The doc was not created`,text:t.refusal},Oe=[...R?[{key:`title`,icon:(0,y.jsx)(i,{size:`sm`}),label:`“${R}”`,detail:`The doc's title`}]:[],...z?B&&D?[{key:`template`,icon:(0,y.jsx)(ne,{size:`sm`}),label:`The ${D.label.toLowerCase()} outline`,detail:`For the agent to follow`}]:[{key:`draft`,icon:(0,y.jsx)(i,{size:`sm`}),label:`Your draft`,detail:`${u(V,`word`)}, for the agent to build on`}]:[]];return(0,y.jsxs)(le,{kind:`doc`,onCancel:Z,status:t.busy?`Creating…`:n.busy?`Filing the task…`:V>0?u(V,`word`):void 0,action:{label:`Create doc`,icon:(0,y.jsx)(r,{size:`sm`}),onSelect:()=>void Q(),...q?{disabledReason:`Creating the doc.`}:J?{disabledReason:J}:{}},handoff:{tool:m(x,T),open:()=>T.setOpen(!0),panel:T.open?(0,y.jsx)(_e,{panel:T,who:x,intro:`${U?.name??`The agent`} gets a task with your brief, drafts the doc and publishes it on the task.`,brief:N,onBrief:xe,briefRef:we,briefLabel:`What should the doc cover?`,briefPlaceholder:`A one-page guide for new engineers on how we review incidents.`,briefHelper:z?`What you wrote on the page goes with it.`:`Name the readers, the outline you have in mind and the pages to draw on.`,starters:be,carried:Oe,alone:`Write on the page, or start from a template, and it goes with the brief.`,task:{title:R||`Untitled doc`,body:G},steps:[`A task is filed to ${U?.name??`the agent`}${x.project?` in ${x.project}`:``}, with your brief.`,`${U?.name??`The agent`} drafts the doc and publishes it on the task.`,`You review it, edit it in place and share it.`],notice:de(n.refusal),sendLabel:e=>`Ask ${e}`,onSend:()=>void Te(),hold:Y,busy:n.busy}):null},children:[(0,y.jsx)(`div`,{className:`doc-create`,children:(0,y.jsxs)(`div`,{className:`doc-create-page`,children:[(0,y.jsxs)(`p`,{className:`doc-create-where`,children:[(0,y.jsx)(ee,{size:`sm`}),(0,y.jsxs)(`span`,{children:[`Private to you until you share it`,C?`, and listed on ${C}`:``]})]}),$&&(0,y.jsx)(re,{variant:`danger`,title:$.title,role:`alert`,children:$.text}),(0,y.jsx)(te,{htmlFor:`doc-create-title`,error:H,children:e=>(0,y.jsx)(ce,{id:e.id,"aria-label":`Title`,"aria-describedby":e.describedBy,placeholder:D?.titleExample??`Untitled doc`,autoComplete:`off`,autoFocus:!0,value:k,error:H!==void 0,disabled:q,onChange:e=>A(e.target.value),onKeyDown:De})}),B&&(0,y.jsx)(oe,{className:`doc-create-starts`,label:`Start from a template`,hideLabel:!0,semantics:`radio`,activate:`manual`,allowNone:!0,value:D?.key??null,onValueChange:Ee,children:_.map(e=>(0,y.jsx)(se,{value:e.key,"aria-description":e.description,children:e.label},e.key))}),(0,y.jsx)(ie,{ref:P,className:`doc-create-editor`,value:j,onChange:M,label:R?`The text of “${R}”`:`The text of the new doc`,placeholder:`Write, or type a slash for blocks`,disabled:q,onSubmit:()=>void Q(),onEscape:Z},Se)]})}),(0,y.jsx)(o,{open:F!==null,destructive:!0,title:`Discard this doc?`,message:`Its title and what you wrote are not saved anywhere yet.`,cancelLabel:`Keep editing`,confirmLabel:`Discard`,onClose:()=>I(null),onConfirm:()=>{let e=F;L.current=!0,I(null),e?.()}})]})}export{x as DocCreate,b as docBrief};