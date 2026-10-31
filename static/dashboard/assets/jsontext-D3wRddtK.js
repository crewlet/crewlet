function e(e){return`  `.repeat(e<32?e:32)}function t(t){if(t===``)return t;try{JSON.parse(t)}catch{return t}let i=``,a=0;for(let o=0;o<t.length;o++){let s=t[o];if(s===`"`){let e=n(t,o);i+=t.slice(o,e),o=e-1}else switch(s){case`{`:case`[`:{let n=s===`{`?`}`:`]`,c=r(t,o+1);if(t[c]===n){i+=s+n,o=c;continue}a++,i+=s+`
`+e(a);continue}case`}`:case`]`:a--,i+=`
`+e(a)+s;continue;case`,`:i+=`,
`+e(a);continue;case`:`:i+=`: `;continue;case` `:case`	`:case`
`:case`\r`:continue;default:i+=s}}return i}function n(e,t){for(let n=t+1;n<e.length;n++){let t=e[n];if(t===`\\`)n++;else if(t===`"`)return n+1}return e.length}function r(e,t){let n=t;for(;n<e.length;){let t=e[n];if(t!==` `&&t!==`	`&&t!==`
`&&t!==`\r`)return n;n++}return n}export{t};