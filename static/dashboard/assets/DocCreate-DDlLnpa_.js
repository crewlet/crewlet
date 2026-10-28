import{r as e}from"./rolldown-runtime-hePW80VL.js";import{i as t,t as n}from"./react-DlYQp10C.js";import{Bt as r,Sn as ee,ct as te,t as ne,xn as re}from"./chunk-ETSF7SU6-D6VxiN7e.js";import{Bn as ie,Hn as ae,Vn as i,ao as a,io as o,sa as oe,ta as s}from"./seats-DcWv6vWp.js";import{w as se}from"./schema-Dyg8QGxo.js";import{t as ce}from"./chunk-JPQSVECQ-CXIcjvyf.js";import"./work-Bo69cclb.js";import{n as c}from"./chunk-ERFXYGRF-CbDLKaCb.js";import{n as le,t as ue}from"./chunk-IWZG7NPV-BpQz0vnz.js";import{r as de}from"./chunk-BXCL6P5Z-BgMltzZM.js";import{_ as fe,a as pe,d as me,f as he,g as l,h as ge,i as _e,l as ve,m as ye,n as be,o as xe,p as Se,r as u,s as d,t as Ce,u as we,v as Te,y as Ee}from"./AskAgent-BLpoykpA.js";import{n as f,r as De}from"./draft-BN9XCgYA.js";var p=e(t(),1),m=[{key:`meeting`,label:`Meeting notes`,description:`Who came, what was discussed, what was decided and who does what next`,titleExample:`Weekly planning, October 20`,body:`**Date:** the day it met · **Attendees:** who came

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
`}];function h(e){return m.find(t=>t.key===e)}var g=n(),Oe=[{key:`onboarding`,label:`Onboarding guide`,brief:`A one-page guide for new engineers on how we review incidents: the timeline, the roles and the follow-ups. Draw on the last three reviews in Doclet.`},{key:`proposal`,label:`Proposal`,brief:`A proposal for the change we discussed: the problem, the options we weighed, the one we recommend and what it costs. Written for the leads who decide.`},{key:`summary`,label:`Summary of a thread`,brief:`A summary of where the project stands for someone joining it today: what was decided, what is open, who owns what and where the work lives.`}];function _(e,t,n){let r=e.trim();return t.trim()===``?r:n&&t===n.body?`${r}\n\nFollow the ${n.label.toLowerCase()} outline:\n\n${t.trim()}`:`${r}\n\nBuild on this first draft:\n\n${t.trim()}`}function v(e){return[{path:`body`,label:`Body`,hint:`Markdown`,read:e=>e.body,write:(e,t)=>typeof t==`string`?{...e,body:t}:f(`The body is Markdown text.`),summary:e=>s(c(typeof e==`string`?e:``),`word`)},{path:`template`,label:`Template`,hint:`One of ${m.map(e=>e.key).join(`, `)}, or empty for none`,read:e=>e.template,write:(t,n)=>{if(!e)return f(`A template is offered while the body is empty or still a template's.`);if(typeof n!=`string`)return f(`A template is one choice.`);if(n.trim()===``)return{body:``,template:``};let r=h(n.trim());return r?{body:r.body,template:r.key}:f(`Choose one of ${m.map(e=>e.key).join(`, `)}.`)}}]}function y(){let e=i(),t=fe(),n=me(),f=ve(`doc`),y=se({app:!0}),b=Ee(),x=Te(),S=Se(),C=xe(),[w]=ae(`template`,``),[T,E]=(0,p.useState)(()=>h(w)),[D,O]=(0,p.useState)(``),[k,A]=(0,p.useState)(T?.body??``),[j,M]=(0,p.useState)(``),[ke,N]=(0,p.useState)(0),P=(0,p.useRef)(null),Ae=(0,p.useRef)(null),[F,I]=(0,p.useState)(null),L=(0,p.useRef)(!1),R=D.trim(),z=k.trim()!==``,B=!z||T!==void 0&&k===T.body,V=c(k),H=l(D),U=_e(f)?.seat,W=_(j,k,T),G=we(`doc`,W,void 0,ye(x)),K=oe(G),q=t.busy||n.busy,J=y.can?ge(D):y.reason,Y=n.held??(U?j.trim()===``?`Say what the doc should cover.`:be(D,`doc`)??(f.project?K>32768?`The brief and the draft are ${K.toLocaleString()} bytes, and a task holds at most ${32768 .toLocaleString()}.`:void 0:`Choose the project to file the task in.`):`Add an agent to the chart first.`),X=R!==``||j.trim()!==``||z&&!B;ie(X?(e,t)=>!L.current&&(I(()=>t),!0):null),De(d({what:`doc`,help:`The body is Markdown and a change replaces all of it. A template fills the body while it is empty or still holds a template untouched.`,step:C.open?`ask`:`write`,title:D,setTitle:O,brief:j,setBrief:M,who:f,extra:{value:{body:k,template:T?.key??``},fields:v(B),apply:e=>{e.template!==(T?.key??``)&&E(h(e.template)),A(e.body),N(e=>e+1)}},blocked:()=>q?`The doc is being created.`:null}));let Z=()=>X?I(()=>S):S(),Q=async()=>{J||q||(L.current=!0,await t.fromStarter(`doc`,R,z?{doc:{body:k}}:{}),L.current=!1)},je=async()=>{if(Y||q)return;L.current=!0;let t=await n.fileArtifactTask({kind:`doc`,title:R,brief:W,seat:f.seat,project:f.project});t?.item&&e.to([`worklet`,t.item]),L.current=!1},Me=e=>{let t=e?h(e):void 0;E(t),A(t?.body??``),N(e=>e+1)},Ne=e=>{(e.key!==`Enter`||!e.metaKey&&!e.ctrlKey)&&(e.key===`Enter`||e.key===`ArrowDown`?(e.preventDefault(),P.current?.focus()):e.key===`Escape`&&(e.preventDefault(),Z()))},$=t.refusal&&{title:`The doc was not created`,text:t.refusal},Pe=[...R?[{key:`title`,icon:(0,g.jsx)(r,{size:`sm`}),label:`“${R}”`,detail:`The doc's title`}]:[],...z?B&&T?[{key:`template`,icon:(0,g.jsx)(re,{size:`sm`}),label:`The ${T.label.toLowerCase()} outline`,detail:`For the agent to follow`}]:[{key:`draft`,icon:(0,g.jsx)(r,{size:`sm`}),label:`Your draft`,detail:`${s(V,`word`)}, for the agent to build on`}]:[]];return(0,g.jsxs)(he,{kind:`doc`,onCancel:Z,status:t.busy?`Creating…`:n.busy?`Filing the task…`:V>0?s(V,`word`):void 0,action:{label:`Create doc`,icon:(0,g.jsx)(te,{size:`sm`}),onSelect:()=>void Q(),...q?{disabledReason:`Creating the doc.`}:J?{disabledReason:J}:{}},handoff:{tool:u(f,C),open:()=>C.setOpen(!0),panel:C.open?(0,g.jsx)(Ce,{panel:C,who:f,intro:`${U?.name??`The agent`} gets a task with your brief, drafts the doc and publishes it on the task.`,brief:j,onBrief:M,briefRef:Ae,briefLabel:`What should the doc cover?`,briefPlaceholder:`A one-page guide for new engineers on how we review incidents.`,briefHelper:z?`What you wrote on the page goes with it.`:`Name the readers, the outline you have in mind and the pages to draw on.`,starters:Oe,carried:Pe,alone:`Write on the page, or start from a template, and it goes with the brief.`,task:{title:R||`Untitled doc`,body:G},steps:[`A task is filed to ${U?.name??`the agent`}${f.project?` in ${f.project}`:``}, with your brief.`,`${U?.name??`The agent`} drafts the doc and publishes it on the task.`,`You review it, edit it in place and share it.`],notice:pe(n.refusal),sendLabel:e=>`Ask ${e}`,onSend:()=>void je(),hold:Y,busy:n.busy}):null},children:[(0,g.jsx)(`div`,{className:`doc-create`,children:(0,g.jsxs)(`div`,{className:`doc-create-page`,children:[(0,g.jsxs)(`p`,{className:`doc-create-where`,children:[(0,g.jsx)(ee,{size:`sm`}),(0,g.jsxs)(`span`,{children:[`Private to you until you share it`,b?`, and listed on ${b}`:``]})]}),$&&(0,g.jsx)(o,{variant:`danger`,title:$.title,role:`alert`,children:$.text}),(0,g.jsx)(ne,{htmlFor:`doc-create-title`,error:H,children:e=>(0,g.jsx)(de,{id:e.id,"aria-label":`Title`,"aria-describedby":e.describedBy,placeholder:T?.titleExample??`Untitled doc`,autoComplete:`off`,autoFocus:!0,value:D,error:H!==void 0,disabled:q,onChange:e=>O(e.target.value),onKeyDown:Ne})}),B&&(0,g.jsx)(le,{className:`doc-create-starts`,label:`Start from a template`,hideLabel:!0,semantics:`radio`,activate:`manual`,allowNone:!0,value:T?.key??null,onValueChange:Me,children:m.map(e=>(0,g.jsx)(ue,{value:e.key,"aria-description":e.description,children:e.label},e.key))}),(0,g.jsx)(ce,{ref:P,className:`doc-create-editor`,value:k,onChange:A,label:R?`The text of “${R}”`:`The text of the new doc`,placeholder:`Write, or type a slash for blocks`,disabled:q,onSubmit:()=>void Q(),onEscape:Z},ke)]})}),(0,g.jsx)(a,{open:F!==null,destructive:!0,title:`Discard this doc?`,message:`Its title and what you wrote are not saved anywhere yet.`,cancelLabel:`Keep editing`,confirmLabel:`Discard`,onClose:()=>I(null),onConfirm:()=>{let e=F;L.current=!0,I(null),e?.()}})]})}export{y as DocCreate,_ as docBrief,v as docFields};