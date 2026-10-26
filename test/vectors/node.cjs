const canonicalize = require('canonicalize').default;
const crypto = require('node:crypto');
const readline = require('node:readline');

function parseStrict(raw) {
  const value = JSON.parse(raw);
  // JSON.parse validates syntax but drops duplicate keys. Inspect raw tokens first.
  const tokens = raw.match(/"(?:[^"\\]|\\.)*"|[{}\[\]:,]|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null/g);
  const stack = [];
  for (let i=0; i<tokens.length; i++) {
    const t=tokens[i];
    if(t==='{' || t==='[') stack.push(t==='{' ? new Set() : null);
    else if(t==='}' || t===']') stack.pop();
    else if(t.startsWith('"')) {
      const s=JSON.parse(t);
      for(let j=0;j<s.length;j++) {
        const c=s.charCodeAt(j);
        if(c>=0xD800 && c<=0xDBFF) { const n=s.charCodeAt(++j); if(!(n>=0xDC00 && n<=0xDFFF)) throw Error('surrogate'); }
        else if(c>=0xDC00 && c<=0xDFFF) throw Error('surrogate');
      }
      if(tokens[i+1]===':') { const keys=stack.at(-1); if(keys.has(s)) throw Error('duplicate'); keys.add(s); }
    } else if(/^-?\d/.test(t)) {
      const n=Number(t);
      if(!Number.isFinite(n) || (Number.isInteger(n) && !Number.isSafeInteger(n))) throw Error('number');
    }
  }
  return value;
}
readline.createInterface({input:process.stdin}).on('line', raw => {
  try { const c=canonicalize(parseStrict(raw)); console.log(JSON.stringify({ok:true,canonical:c,digest:'sha256:'+crypto.createHash('sha256').update(c).digest('hex')})); }
  catch { console.log(JSON.stringify({ok:false})); }
});
