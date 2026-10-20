import{r as e}from"./rolldown-runtime-hePW80VL.js";import{i as t,t as n}from"./react-DlYQp10C.js";import{J as r,R as i,Tn as a,Vn as ee,i as o,on as s,t as c}from"./chunk-6QFAK2WV-Ci3nx-D3.js";import{t as l}from"./chunk-CCAXYHQS-g8_rEhqY.js";import{An as u,On as te,Si as ne,gi as re,kn as ie,ma as ae,pa as d}from"./seats-BrHg7b30.js";import{t as f}from"./chunk-3VD3G3IL-CAVx0V1A.js";import{n as oe}from"./chunk-OOWPSBIS-NviSfqgk.js";import"./work-B87jRcaW.js";import{n as se,t as ce}from"./chunk-5YLON2PW-tL6NCz18.js";import{r as le}from"./chunk-TYKQ33CL-C4smLV6L.js";import{t as ue}from"./access-Dzv8TpOi.js";import{A as de,a as p,c as fe,i as pe,k as me,n as m,o as h,r as g,s as he,t as ge}from"./WhoBuildsIt-D3PG9qwq.js";var _=e(t(),1),v=[{key:`meeting`,label:`Meeting notes`,description:`Who came, what was discussed, what was decided and who does what next`,titleExample:`Weekly planning, October 20`,body:`**Date:** the day it met · **Attendees:** who came

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
`}];function y(e){return v.find(t=>t.key===e)}var b=n(),_e=[{value:`write`,label:`Write it`,icon:(0,b.jsx)(a,{size:`sm`})},{value:`agent`,label:`Ask an agent`,icon:(0,b.jsx)(i,{size:`sm`})}];function x(e,t,n){let r=e.trim();return t.trim()===``?r:n&&t===n.body?`${r}\n\nFollow the ${n.label.toLowerCase()} outline:\n\n${t.trim()}`:`${r}\n\nBuild on this first draft:\n\n${t.trim()}`}function S(){let e=ie(),t=he(),n=pe(),i=m(`doc`),a=ue({app:!0}),S=fe(),C=de(),[ve,ye]=u(`mode`,`write`),w=ve===`agent`?`agent`:`write`,[T]=u(`template`,``),[E,D]=(0,_.useState)(()=>y(T)),[O,k]=(0,_.useState)(``),[A,j]=(0,_.useState)(E?.body??``),[M,N]=(0,_.useState)(``),[P,be]=(0,_.useState)(0),F=(0,_.useRef)(null),I=(0,_.useRef)(null),[L,R]=(0,_.useState)(null),z=(0,_.useRef)(!1),B=O.trim(),V=A.trim()!==``,H=!V||E!==void 0&&A===E.body,U=oe(A),W=h(O),G=i.builders.find(e=>e.seat.handle===i.seat)?.seat,K=x(M,A,E),q=ne(g(`doc`,K)),J=t.busy||n.busy,Y=w===`write`?a.can?p(O):a.reason:n.held??p(O)??(G?M.trim()===``?`Say what the doc should cover.`:i.project?q>32768?`The brief and the draft are ${q} bytes, and a task holds at most 32768.`:void 0:`Choose the project to file the task in.`:`Add an agent to the chart first.`),X=B!==``||M.trim()!==``||V&&!H;te(X?(e,t)=>!z.current&&(R(()=>t),!0):null);let Z=()=>X?R(()=>C):C(),Q=async()=>{if(!(Y||J)){if(z.current=!0,w===`write`)await t.fromStarter(`doc`,B,V?{doc:{body:A}}:{});else{let t=await n.fileArtifactTask({kind:`doc`,title:B,brief:K,seat:i.seat,project:i.project});t?.item&&e.to([`work`,t.item])}z.current=!1}},xe=e=>{let t=e?y(e):void 0;D(t),j(t?.body??``),be(e=>e+1)},Se=e=>{(e.key!==`Enter`||!e.metaKey&&!e.ctrlKey)&&(e.key===`Enter`||e.key===`ArrowDown`?(e.preventDefault(),w===`write`?F.current?.focus():I.current?.focus()):e.key===`Escape`&&(e.preventDefault(),Z()))},$=w===`write`?t.refusal&&{title:`The doc was not created`,text:t.refusal}:n.refusal&&{title:`The task was not filed`,text:[n.refusal.sentence,n.refusal.hint].filter(Boolean).join(` `)};return(0,b.jsxs)(me,{kind:`doc`,onCancel:Z,status:J?w===`write`?`Creating…`:`Filing the task…`:w===`write`&&U>0?re(U,`word`):void 0,action:{label:w===`write`?`Create doc`:G?`Ask ${G.name}`:`Ask an agent`,icon:w===`write`?(0,b.jsx)(r,{size:`sm`}):(0,b.jsx)(ee,{size:`sm`}),onSelect:()=>void Q(),...J?{disabledReason:w===`write`?`Creating the doc.`:`Filing the task.`}:Y?{disabledReason:Y}:{}},children:[(0,b.jsx)(`div`,{className:`doc-create`,children:(0,b.jsxs)(`div`,{className:`doc-create-page`,children:[(0,b.jsxs)(`div`,{className:`doc-create-head`,children:[(0,b.jsx)(l,{label:`How to make it`,semantics:`radio`,size:`sm`,options:_e.map(e=>({value:e.value,label:e.label,icon:e.icon})),value:w,onValueChange:e=>ye(e===`agent`?`agent`:``)}),w===`write`&&(0,b.jsxs)(`p`,{className:`doc-create-where`,children:[(0,b.jsx)(s,{size:`sm`}),(0,b.jsxs)(`span`,{children:[`Private to you until you share it`,S?`, and listed on ${S}`:``]})]})]}),$&&(0,b.jsx)(d,{variant:`danger`,title:$.title,role:`alert`,children:$.text}),(0,b.jsx)(c,{htmlFor:`doc-create-title`,error:W,children:e=>(0,b.jsx)(le,{id:e.id,"aria-label":`Title`,"aria-describedby":e.describedBy,placeholder:E?.titleExample??`Untitled doc`,autoComplete:`off`,autoFocus:!0,value:O,error:W!==void 0,disabled:J,onChange:e=>k(e.target.value),onKeyDown:Se})}),w===`write`?(0,b.jsxs)(b.Fragment,{children:[H&&(0,b.jsx)(se,{className:`doc-create-starts`,label:`Start from a template`,hideLabel:!0,semantics:`radio`,activate:`manual`,allowNone:!0,value:E?.key??null,onValueChange:xe,children:v.map(e=>(0,b.jsx)(ce,{value:e.key,"aria-description":e.description,children:e.label},e.key))}),(0,b.jsx)(f,{ref:F,className:`doc-create-editor`,value:A,onChange:j,label:B?`The text of “${B}”`:`The text of the new doc`,placeholder:`Write, or type a slash for blocks`,disabled:J,onSubmit:()=>void Q(),onEscape:Z},P)]}):(0,b.jsxs)(`div`,{className:`doc-create-brief`,children:[(0,b.jsx)(c,{label:`Brief`,helper:V?H&&E?`The ${E.label.toLowerCase()} outline goes with it, for the agent to follow.`:`Your draft goes with it, for the agent to build on.`:`What it should cover, who reads it, and what to draw on.`,children:e=>(0,b.jsx)(o,{ref:I,id:e.id,"aria-describedby":e.describedBy,autoResize:!0,rows:5,placeholder:`A one-page guide for new engineers on how we review incidents: the timeline, the roles and the follow-ups. Draw on the last three reviews in Knowledge.`,value:M,disabled:J,onChange:e=>N(e.target.value),onKeyDown:e=>{e.key===`Escape`&&(e.preventDefault(),Z())}})}),(0,b.jsx)(ge,{who:i}),(0,b.jsx)(d,{variant:`neutral`,title:`What happens next`,children:(0,b.jsxs)(`ol`,{className:`doc-create-next`,children:[(0,b.jsxs)(`li`,{children:[`A task is filed to `,G?.name??`the agent`,i.project?` in ${i.project}`:``,`, with your brief.`]}),(0,b.jsxs)(`li`,{children:[G?.name??`The agent`,` drafts the doc and publishes it on the task.`]}),(0,b.jsx)(`li`,{children:`You review it, edit it in place and share it.`})]})})]})]})}),(0,b.jsx)(ae,{open:L!==null,destructive:!0,title:`Discard this doc?`,message:`What you wrote is not saved anywhere.`,cancelLabel:`Keep writing`,confirmLabel:`Discard`,onClose:()=>R(null),onConfirm:()=>{let e=L;z.current=!0,R(null),e?.()}})]})}export{S as DocCreate,x as docBrief};