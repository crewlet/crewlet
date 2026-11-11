import{r as e}from"./rolldown-runtime-XKyaHtl9.js";import{i as t,t as n}from"./react-C1iE8mnY.js";import{Ua as r,Za as ee,dr as te,fr as ne,mr as re,na as ie,os as i,ss as a}from"./authorityRemedy-BjioEn4K.js";import{r as o}from"./chunk-I4PBFJGX-BijLFw4i.js";import{n as ae,t as oe}from"./chunk-L6HUVA33-DplSIyqG.js";import{X as se,fn as ce,i as le,jt as s,pn as ue}from"./chunk-M2M6ZD7P-B4LYBm95.js";import{t as de}from"./chunk-3RKS3DLF-Ddwm3TBl.js";import{i as c}from"./chunk-GCPSAYA3-CHGP0cdw.js";import"./keymap-CIpTsMId.js";import{w as fe}from"./schema-DFVSM_Wk.js";import{_ as pe,a as me,b as he,d as ge,f as _e,g as ve,h as ye,i as be,l as xe,m as l,n as u,o as Se,p as Ce,r as we,s as Te,t as Ee,u as De,y as Oe}from"./AskAgent-BlEaeBAo.js";import{n as d,r as ke}from"./draft-CfPKp1fI.js";var f=e(t(),1),p=[{key:`meeting`,label:`Meeting notes`,description:`Who came, what was discussed, what was decided and who does what next`,titleExample:`Weekly planning, October 20`,body:`**Date:** the day it met · **Attendees:** who came

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
`}];function m(e){return p.find(t=>t.key===e)}var h=n(),Ae=[{key:`onboarding`,label:`Onboarding guide`,brief:`A one-page guide for new engineers on how we review incidents: the timeline, the roles and the follow-ups. Draw on the last three reviews in Doclet.`},{key:`proposal`,label:`Proposal`,brief:`A proposal for the change we discussed: the problem, the options we weighed, the one we recommend and what it costs. Written for the leads who decide.`},{key:`summary`,label:`Summary of a thread`,brief:`A summary of where the project stands for someone joining it today: what was decided, what is open, who owns what and where the work lives.`}];function g(e,t,n){let r=e.trim();return t.trim()===``?r:n&&t===n.body?`${r}\n\nFollow the ${n.label.toLowerCase()} outline:\n\n${t.trim()}`:`${r}\n\nBuild on this first draft:\n\n${t.trim()}`}function _(e){return[{path:`body`,label:`Body`,hint:`Markdown`,read:e=>e.body,write:(e,t)=>typeof t==`string`?{...e,body:t}:d(`The body is Markdown text.`),summary:e=>r(c(typeof e==`string`?e:``),`word`)},{path:`template`,label:`Template`,hint:`One of ${p.map(e=>e.key).join(`, `)}, or empty for none`,read:e=>e.template,write:(t,n)=>{if(!e)return d(`A template is offered while the body is empty or still a template's.`);if(typeof n!=`string`)return d(`A template is one choice.`);if(n.trim()===``)return{body:``,template:``};let r=m(n.trim());return r?{body:r.body,template:r.key}:d(`Choose one of ${p.map(e=>e.key).join(`, `)}.`)}}]}function v(){let e=ne(),t=pe(),n=ge(),d=xe(`doc`),v=fe({app:!0}),y=he(),b=Oe(),x=Ce(),S=Se(),[C]=re(`template`,``),[w,T]=(0,f.useState)(()=>m(C)),[E,D]=(0,f.useState)(``),[O,k]=(0,f.useState)(w?.body??``),[A,j]=(0,f.useState)(``),[je,M]=(0,f.useState)(0),N=(0,f.useRef)(null),P=(0,f.useRef)(null),[F,I]=(0,f.useState)(null),L=(0,f.useRef)(!1),R=E.trim(),z=O.trim()!==``,B=!z||w!==void 0&&O===w.body,V=c(O),H=ve(E),U=be(d)?.seat,W=g(A,O,w),G=De(`doc`,W,void 0,l(b)),K=ee(G),q=t.busy||n.busy,J=v.can?ye(E):v.reason,Y=n.held??(U?A.trim()===``?`Say what the doc should cover.`:u(E,`doc`)??(d.project?K>32768?`The brief and the draft are ${ie(K,32768)} for a task.`:void 0:`Choose the project to file the task in.`):`Add an agent to the chart first.`),X=R!==``||A.trim()!==``||z&&!B;te(X?(e,t)=>!L.current&&(I(()=>t),!0):null),ke(Te({what:`doc`,help:`The body is Markdown and a change replaces all of it. A template fills the body while it is empty or still holds a template untouched.`,step:S.open?`ask`:`write`,title:E,setTitle:D,brief:A,setBrief:j,who:d,extra:{value:{body:O,template:w?.key??``},fields:_(B),apply:e=>{e.template!==(w?.key??``)&&T(m(e.template)),k(e.body),M(e=>e+1)}},blocked:()=>q?`The doc is being created.`:null}));let Z=()=>X?I(()=>x):x(),Q=async()=>{J||q||(L.current=!0,await t.fromStarter(`doc`,R,z?{doc:{body:O}}:{}),L.current=!1)},Me=async()=>{if(Y||q)return;L.current=!0;let t=await n.fileArtifactTask({kind:`doc`,title:R,brief:W,seat:d.seat,project:d.project});t?.item&&e.to([`worklet`,t.item]),L.current=!1},Ne=e=>{let t=e?m(e):void 0;T(t),k(t?.body??``),M(e=>e+1)},Pe=e=>{(e.key!==`Enter`||!e.metaKey&&!e.ctrlKey)&&(e.key===`Enter`||e.key===`ArrowDown`?(e.preventDefault(),N.current?.focus()):e.key===`Escape`&&(e.preventDefault(),Z()))},$=t.refusal&&{title:`The doc was not created`,text:t.refusal},Fe=[...R?[{key:`title`,icon:(0,h.jsx)(s,{size:`sm`}),label:`“${R}”`,detail:`The doc's title`}]:[],...z?B&&w?[{key:`template`,icon:(0,h.jsx)(ce,{size:`sm`}),label:`The ${w.label.toLowerCase()} outline`,detail:`For the agent to follow`}]:[{key:`draft`,icon:(0,h.jsx)(s,{size:`sm`}),label:`Your draft`,detail:`${r(V,`word`)}, for the agent to build on`}]:[]];return(0,h.jsxs)(_e,{kind:`doc`,onCancel:Z,status:t.busy?`Creating…`:n.busy?`Filing the task…`:V>0?r(V,`word`):void 0,action:{label:`Create doc`,icon:(0,h.jsx)(se,{size:`sm`}),onSelect:()=>void Q(),...q?{disabledReason:`Creating the doc.`}:J?{disabledReason:J}:{}},handoff:{tool:we(d,S),open:()=>S.setOpen(!0),panel:S.open?(0,h.jsx)(Ee,{panel:S,who:d,intro:`${U?.name??`The agent`} gets a task with your brief, drafts the doc and publishes it on the task.`,brief:A,onBrief:j,briefRef:P,briefLabel:`What should the doc cover?`,briefPlaceholder:`A one-page guide for new engineers on how we review incidents.`,briefHelper:z?`What you wrote on the page goes with it.`:`Name the readers, the outline you have in mind and the pages to draw on.`,starters:Ae,carried:Fe,alone:`Write on the page, or start from a template, and it goes with the brief.`,task:{title:R||`Untitled doc`,body:G},steps:[`A task is filed to ${U?.name??`the agent`}${d.project?` in ${d.project}`:``}, with your brief.`,`${U?.name??`The agent`} drafts the doc and publishes it on the task.`,`You review it, edit it in place and share it.`],notice:me(n.refusal),sendLabel:`Ask`,onSend:()=>void Me(),hold:Y,busy:n.busy}):null},children:[(0,h.jsx)(`div`,{className:`doc-create`,children:(0,h.jsxs)(`div`,{className:`doc-create-page`,children:[(0,h.jsxs)(`p`,{className:`doc-create-where`,children:[(0,h.jsx)(ue,{size:`sm`}),(0,h.jsxs)(`span`,{children:[`Private to you until you share it`,y?`, and listed on ${y}`:``]})]}),$&&(0,h.jsx)(i,{variant:`danger`,title:$.title,role:`alert`,children:$.text}),(0,h.jsx)(le,{htmlFor:`doc-create-title`,error:H,children:e=>(0,h.jsx)(o,{id:e.id,"aria-label":`Title`,"aria-describedby":e.describedBy,placeholder:w?.titleExample??`Untitled doc`,autoComplete:`off`,autoFocus:!0,value:E,error:H!==void 0,disabled:q,onChange:e=>D(e.target.value),onKeyDown:Pe})}),B&&(0,h.jsx)(ae,{className:`doc-create-starts`,label:`Start from a template`,hideLabel:!0,semantics:`radio`,activate:`manual`,allowNone:!0,value:w?.key??null,onValueChange:Ne,children:p.map(e=>(0,h.jsx)(oe,{value:e.key,"aria-description":e.description,children:e.label},e.key))}),(0,h.jsx)(de,{ref:N,className:`doc-create-editor`,value:O,onChange:k,label:R?`The text of “${R}”`:`The text of the new doc`,placeholder:`Write, or type a slash for blocks`,disabled:q,onSubmit:()=>void Q(),onEscape:Z},je)]})}),(0,h.jsx)(a,{open:F!==null,destructive:!0,title:`Discard this doc?`,message:`Its title and what you wrote are not saved anywhere yet.`,cancelLabel:`Keep editing`,confirmLabel:`Discard`,onClose:()=>I(null),onConfirm:()=>{let e=F;L.current=!0,I(null),e?.()}})]})}export{v as DocCreate,g as docBrief,_ as docFields};