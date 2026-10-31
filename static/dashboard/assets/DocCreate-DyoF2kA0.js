import{r as e}from"./rolldown-runtime-hePW80VL.js";import{i as t,t as n}from"./react-DlYQp10C.js";import{Qt as r,Zt as ee,gt as i,t as te,z as a}from"./chunk-QLEJPU5R-CQFgfgbq.js";import{Ba as o,Cn as s,Pi as c,Sn as ne,Va as re,Vi as ie,wn as ae}from"./seats-CGMrdkEI.js";import{w as oe}from"./schema-39ehQVvC.js";import{t as se}from"./chunk-LVFG2RER-ZPqjf5Ly.js";import{n as ce}from"./chunk-F45VSK2M-BXVM7Q7_.js";import{n as le,t as ue}from"./chunk-UYZZV4DZ-DA7YDrPl.js";import{r as de}from"./chunk-PWG3VHRQ-BQmAMl7H.js";import{_ as fe,a as pe,c as me,d as l,f as he,g as ge,h as _e,i as u,l as d,m as f,n as p,o as m,p as ve,r as ye,s as be,t as xe,u as Se}from"./AskAgent--LzooG2G.js";import"./work-DmqzV5dC.js";var h=e(t(),1),g=[{key:`meeting`,label:`Meeting notes`,description:`Who came, what was discussed, what was decided and who does what next`,titleExample:`Weekly planning, October 20`,body:`**Date:** the day it met · **Attendees:** who came

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
`}];function _(e){return g.find(t=>t.key===e)}var v=n(),Ce=[{key:`onboarding`,label:`Onboarding guide`,brief:`A one-page guide for new engineers on how we review incidents: the timeline, the roles and the follow-ups. Draw on the last three reviews in Knowledge.`},{key:`proposal`,label:`Proposal`,brief:`A proposal for the change we discussed: the problem, the options we weighed, the one we recommend and what it costs. Written for the leads who decide.`},{key:`summary`,label:`Summary of a thread`,brief:`A summary of where the project stands for someone joining it today: what was decided, what is open, who owns what and where the work lives.`}];function y(e,t,n){let r=e.trim();return t.trim()===``?r:n&&t===n.body?`${r}\n\nFollow the ${n.label.toLowerCase()} outline:\n\n${t.trim()}`:`${r}\n\nBuild on this first draft:\n\n${t.trim()}`}function b(){let e=s(),t=_e(),n=d(),b=be(`doc`),x=oe({app:!0}),S=fe(),C=ge(),w=l(),T=m(),[E]=ae(`template`,``),[D,O]=(0,h.useState)(()=>_(E)),[k,A]=(0,h.useState)(``),[j,M]=(0,h.useState)(D?.body??``),[N,we]=(0,h.useState)(``),[Te,Ee]=(0,h.useState)(0),P=(0,h.useRef)(null),De=(0,h.useRef)(null),[F,I]=(0,h.useState)(null),L=(0,h.useRef)(!1),R=k.trim(),z=j.trim()!==``,B=!z||D!==void 0&&j===D.body,V=ce(j),H=f(k),U=u(b)?.seat,W=y(N,j,D),G=me(`doc`,W,void 0,he(C)),K=ie(G),q=t.busy||n.busy,J=x.can?ve(k):x.reason,Y=n.held??(U?N.trim()===``?`Say what the doc should cover.`:p(k,`doc`)??(b.project?K>32768?`The brief and the draft are ${K.toLocaleString()} bytes, and a task holds at most ${32768 .toLocaleString()}.`:void 0:`Choose the project to file the task in.`):`Add an agent to the chart first.`),X=R!==``||N.trim()!==``||z&&!B;ne(X?(e,t)=>!L.current&&(I(()=>t),!0):null);let Z=()=>X?I(()=>w):w(),Q=async()=>{J||q||(L.current=!0,await t.fromStarter(`doc`,R,z?{doc:{body:j}}:{}),L.current=!1)},Oe=async()=>{if(Y||q)return;L.current=!0;let t=await n.fileArtifactTask({kind:`doc`,title:R,brief:W,seat:b.seat,project:b.project});t?.item&&e.to([`work`,t.item]),L.current=!1},ke=e=>{let t=e?_(e):void 0;O(t),M(t?.body??``),Ee(e=>e+1)},Ae=e=>{(e.key!==`Enter`||!e.metaKey&&!e.ctrlKey)&&(e.key===`Enter`||e.key===`ArrowDown`?(e.preventDefault(),P.current?.focus()):e.key===`Escape`&&(e.preventDefault(),Z()))},$=t.refusal&&{title:`The doc was not created`,text:t.refusal},je=[...R?[{key:`title`,icon:(0,v.jsx)(i,{size:`sm`}),label:`“${R}”`,detail:`The doc's title`}]:[],...z?B&&D?[{key:`template`,icon:(0,v.jsx)(ee,{size:`sm`}),label:`The ${D.label.toLowerCase()} outline`,detail:`For the agent to follow`}]:[{key:`draft`,icon:(0,v.jsx)(i,{size:`sm`}),label:`Your draft`,detail:`${c(V,`word`)}, for the agent to build on`}]:[]];return(0,v.jsxs)(Se,{kind:`doc`,onCancel:Z,status:t.busy?`Creating…`:n.busy?`Filing the task…`:V>0?c(V,`word`):void 0,action:{label:`Create doc`,icon:(0,v.jsx)(a,{size:`sm`}),onSelect:()=>void Q(),...q?{disabledReason:`Creating the doc.`}:J?{disabledReason:J}:{}},handoff:{tool:ye(b,T),open:()=>T.setOpen(!0),panel:T.open?(0,v.jsx)(xe,{panel:T,who:b,intro:`${U?.name??`The agent`} gets a task with your brief, drafts the doc and publishes it on the task.`,brief:N,onBrief:we,briefRef:De,briefLabel:`What should the doc cover?`,briefPlaceholder:`A one-page guide for new engineers on how we review incidents.`,briefHelper:z?`What you wrote on the page goes with it.`:`Name the readers, the outline you have in mind and the pages to draw on.`,starters:Ce,carried:je,alone:`Write on the page, or start from a template, and it goes with the brief.`,task:{title:R||`Untitled doc`,body:G},steps:[`A task is filed to ${U?.name??`the agent`}${b.project?` in ${b.project}`:``}, with your brief.`,`${U?.name??`The agent`} drafts the doc and publishes it on the task.`,`You review it, edit it in place and share it.`],notice:pe(n.refusal),sendLabel:e=>`Ask ${e}`,onSend:()=>void Oe(),hold:Y,busy:n.busy}):null},children:[(0,v.jsx)(`div`,{className:`doc-create`,children:(0,v.jsxs)(`div`,{className:`doc-create-page`,children:[(0,v.jsxs)(`p`,{className:`doc-create-where`,children:[(0,v.jsx)(r,{size:`sm`}),(0,v.jsxs)(`span`,{children:[`Private to you until you share it`,S?`, and listed on ${S}`:``]})]}),$&&(0,v.jsx)(o,{variant:`danger`,title:$.title,role:`alert`,children:$.text}),(0,v.jsx)(te,{htmlFor:`doc-create-title`,error:H,children:e=>(0,v.jsx)(de,{id:e.id,"aria-label":`Title`,"aria-describedby":e.describedBy,placeholder:D?.titleExample??`Untitled doc`,autoComplete:`off`,autoFocus:!0,value:k,error:H!==void 0,disabled:q,onChange:e=>A(e.target.value),onKeyDown:Ae})}),B&&(0,v.jsx)(le,{className:`doc-create-starts`,label:`Start from a template`,hideLabel:!0,semantics:`radio`,activate:`manual`,allowNone:!0,value:D?.key??null,onValueChange:ke,children:g.map(e=>(0,v.jsx)(ue,{value:e.key,"aria-description":e.description,children:e.label},e.key))}),(0,v.jsx)(se,{ref:P,className:`doc-create-editor`,value:j,onChange:M,label:R?`The text of “${R}”`:`The text of the new doc`,placeholder:`Write, or type a slash for blocks`,disabled:q,onSubmit:()=>void Q(),onEscape:Z},Te)]})}),(0,v.jsx)(re,{open:F!==null,destructive:!0,title:`Discard this doc?`,message:`Its title and what you wrote are not saved anywhere yet.`,cancelLabel:`Keep editing`,confirmLabel:`Discard`,onClose:()=>I(null),onConfirm:()=>{let e=F;L.current=!0,I(null),e?.()}})]})}export{b as DocCreate,y as docBrief};