import{r as e}from"./rolldown-runtime-XKyaHtl9.js";import{i as t,t as n}from"./react-C1iE8mnY.js";import{Ro as r,Ua as i,Za as ee,dr as te,fr as ne,mr as re,na as ie,zo as a}from"./authorityRemedy-DRpudcHO.js";import{r as ae}from"./chunk-I4PBFJGX-BmNYWrUI.js";import{n as oe,t as o}from"./chunk-L6HUVA33-CJ--5wib.js";import{Mt as s,X as se,gn as ce,hn as le,i as ue}from"./chunk-M2M6ZD7P-Vq6raJiq.js";import{t as de}from"./chunk-ENYHABQJ-C42nV6RS.js";import{i as c}from"./chunk-GCPSAYA3-2FoALo2T.js";import"./keymap-1XVuPq-v.js";import{w as fe}from"./schema-BPuOswSF.js";import{_ as pe,a as me,b as he,f as ge,g as _e,h as l,i as u,l as d,m as ve,n as ye,o as be,p as xe,r as Se,s as Ce,t as we,v as Te,x as Ee}from"./AskAgent-DY9Xkx66.js";import{f as De,g as Oe,l as f,m as p,y as ke}from"./draft-Bex1ZcuI.js";var m=e(t(),1),h=[{key:`meeting`,label:`Meeting notes`,description:`Who came, what was discussed, what was decided and who does what next`,titleExample:`Weekly planning, October 20`,body:`**Date:** the day it met · **Attendees:** who came

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
`}];function g(e){return h.find(t=>t.key===e)}var _=n(),Ae=[{key:`onboarding`,label:`Onboarding guide`,brief:`A one-page guide for new engineers on how we review incidents: the timeline, the roles and the follow-ups. Draw on the last three reviews in Doclet.`},{key:`proposal`,label:`Proposal`,brief:`A proposal for the change we discussed: the problem, the options we weighed, the one we recommend and what it costs. Written for the leads who decide.`},{key:`summary`,label:`Summary of a thread`,brief:`A summary of where the project stands for someone joining it today: what was decided, what is open, who owns what and where the work lives.`}];function v(e,t,n){let r=e.trim();return t.trim()===``?r:n&&t===n.body?`${r}\n\nFollow the ${n.label.toLowerCase()} outline:\n\n${t.trim()}`:`${r}\n\nBuild on this first draft:\n\n${t.trim()}`}function y(e){return[{path:`body`,label:`Body`,hint:`Markdown`,read:e=>e.body,write:(e,t)=>typeof t==`string`?{...e,body:t}:f(`The body is Markdown text.`),summary:e=>i(c(typeof e==`string`?e:``),`word`)},{path:`template`,label:`Template`,hint:`One of ${h.map(e=>e.key).join(`, `)}, or empty for none`,read:e=>e.template,write:(t,n)=>{if(!e)return f(`A template is offered while the body is empty or still a template's.`);if(typeof n!=`string`)return f(`A template is one choice.`);if(n.trim()===``)return{body:``,template:``};let r=g(n.trim());return r?{body:r.body,template:r.key}:f(`Choose one of ${h.map(e=>e.key).join(`, `)}.`)}}]}function b(){let e=ne(),t=Te(),n=xe(),f=d(`doc`),b=fe({app:!0}),x=Ee(),S=he(),C=l(),w=be(),[je]=re(`template`,``),[T,E]=p(()=>g(je)),[D,O]=p(``),[k,A]=p(T?.body??``),[j,M]=p(``),[Me,N]=(0,m.useState)(0),P=(0,m.useRef)(null),Ne=(0,m.useRef)(null),[F,I]=(0,m.useState)(null),L=(0,m.useRef)(!1),R=D.trim(),z=k.trim()!==``,B=!z||T!==void 0&&k===T.body,V=c(k),H=pe(D),U=u(f)?.seat,W=v(j,k,T),G=ge({kind:`doc`,brief:W},S),K=ee(G),q=t.busy||n.busy,J=b.can?_e(D):b.reason,Y=n.held??(U?j.trim()===``?`Say what the doc should cover.`:ye(D,`doc`)??(f.project?K>32768?`The brief and the draft are ${ie(K,32768)} for a task.`:void 0:`Choose the project to file the task in.`):`Add an agent to the chart first.`),X=R!==``||j.trim()!==``||z&&!B;te(X?(e,t)=>!L.current&&(I(()=>t),!0):null),De(Ce({what:`doc`,help:`The body is Markdown and a change replaces all of it. A template fills the body while it is empty or still holds a template untouched.`,step:w.open?`ask`:`write`,title:D,setTitle:O,brief:j,setBrief:M,who:f,extra:{value:{body:k,template:T?.key??``},fields:y(B),write:Oe({body:k,template:T?.key??``},{body:A,template:ke(E,e=>g(e))},()=>N(e=>e+1))},blocked:()=>q?`The doc is being created.`:null}));let Z=()=>X?I(()=>C):C(),Q=async()=>{J||q||(L.current=!0,await t.fromStarter(`doc`,R,z?{doc:{body:k}}:{}),L.current=!1)},Pe=async()=>{if(Y||q)return;L.current=!0;let t=await n.fileArtifactTask({kind:`doc`,title:R,brief:W,seat:f.seat,project:f.project});t?.item&&e.to([`worklet`,t.item]),L.current=!1},Fe=e=>{let t=e?g(e):void 0;E(t),A(t?.body??``),N(e=>e+1)},Ie=e=>{(e.key!==`Enter`||!e.metaKey&&!e.ctrlKey)&&(e.key===`Enter`||e.key===`ArrowDown`?(e.preventDefault(),P.current?.focus()):e.key===`Escape`&&(e.preventDefault(),Z()))},$=t.refusal&&{title:`The doc was not created`,text:t.refusal},Le=[...R?[{key:`title`,icon:(0,_.jsx)(s,{size:`sm`}),label:`“${R}”`,detail:`The doc's title`}]:[],...z?B&&T?[{key:`template`,icon:(0,_.jsx)(le,{size:`sm`}),label:`The ${T.label.toLowerCase()} outline`,detail:`For the agent to follow`}]:[{key:`draft`,icon:(0,_.jsx)(s,{size:`sm`}),label:`Your draft`,detail:`${i(V,`word`)}, for the agent to build on`}]:[]];return(0,_.jsxs)(ve,{kind:`doc`,onCancel:Z,status:t.busy?`Creating…`:n.busy?`Filing the task…`:V>0?i(V,`word`):void 0,action:{label:`Create doc`,icon:(0,_.jsx)(se,{size:`sm`}),onSelect:()=>void Q(),...q?{disabledReason:`Creating the doc.`}:J?{disabledReason:J}:{}},handoff:{tool:Se(f,w),open:()=>w.setOpen(!0),panel:w.open?(0,_.jsx)(we,{panel:w,who:f,intro:`${U?.name??`The agent`} gets a task with your brief, drafts the doc and publishes it on the task.`,brief:j,onBrief:M,briefRef:Ne,briefLabel:`What should the doc cover?`,briefPlaceholder:`A one-page guide for new engineers on how we review incidents.`,briefHelper:z?`What you wrote on the page goes with it.`:`Name the readers, the outline you have in mind and the pages to draw on.`,starters:Ae,carried:Le,alone:`Write on the page, or start from a template, and it goes with the brief.`,task:{title:R||`Untitled doc`,body:G},steps:[`A task is filed to ${U?.name??`the agent`}${f.project?` in ${f.project}`:``}, with your brief.`,`${U?.name??`The agent`} drafts the doc and publishes it on the task.`,`You review it, edit it in place and share it.`],notice:me(n.refusal),sendLabel:`Ask`,onSend:()=>void Pe(),hold:Y,busy:n.busy}):null},children:[(0,_.jsx)(`div`,{className:`doc-create`,children:(0,_.jsxs)(`div`,{className:`doc-create-page`,children:[(0,_.jsxs)(`p`,{className:`doc-create-where`,children:[(0,_.jsx)(ce,{size:`sm`}),(0,_.jsxs)(`span`,{children:[`Private to you until you share it`,x?`, and listed on ${x}`:``]})]}),$&&(0,_.jsx)(r,{variant:`danger`,title:$.title,role:`alert`,children:$.text}),(0,_.jsx)(ue,{htmlFor:`doc-create-title`,error:H,children:e=>(0,_.jsx)(ae,{id:e.id,"aria-label":`Title`,"aria-describedby":e.describedBy,placeholder:T?.titleExample??`Untitled doc`,autoComplete:`off`,autoFocus:!0,value:D,error:H!==void 0,disabled:q,onChange:e=>O(e.target.value),onKeyDown:Ie})}),B&&(0,_.jsx)(oe,{className:`doc-create-starts`,label:`Start from a template`,hideLabel:!0,semantics:`radio`,activate:`manual`,allowNone:!0,value:T?.key??null,onValueChange:Fe,children:h.map(e=>(0,_.jsx)(o,{value:e.key,"aria-description":e.description,children:e.label},e.key))}),(0,_.jsx)(de,{ref:P,className:`doc-create-editor`,value:k,onChange:A,label:R?`The text of “${R}”`:`The text of the new doc`,placeholder:`Write, or type a slash for blocks`,disabled:q,onSubmit:()=>void Q(),onEscape:Z},Me)]})}),(0,_.jsx)(a,{open:F!==null,destructive:!0,title:`Discard this doc?`,message:`Its title and what you wrote are not saved anywhere yet.`,cancelLabel:`Keep editing`,confirmLabel:`Discard`,onClose:()=>I(null),onConfirm:()=>{let e=F;L.current=!0,I(null),e?.()}})]})}export{b as DocCreate,v as docBrief,y as docFields};