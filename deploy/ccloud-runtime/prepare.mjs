#!/usr/bin/env node
// 仅处理已审计归档；可信哈希固定于源码，不接受调用方覆盖。
import {createHash} from 'node:crypto';
import {readFileSync,readdirSync,lstatSync,mkdirSync,chmodSync,existsSync} from 'node:fs';
import {resolve,relative,join} from 'node:path';
import {execFileSync} from 'node:child_process';
export const ARCHIVE_SHA256='df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87';
export const MANIFEST_SHA256='036011de6bb8af591eb38b1119d54f5956d9ba44dd46731df523ee31e2690c28';
const sha=b=>createHash('sha256').update(b).digest('hex');
export function prepare(archive,destination){
 if(sha(readFileSync(archive))!==ARCHIVE_SHA256)throw Error('untrusted archive');
 const dest=resolve(destination);
 if(existsSync(dest))throw Error('destination must not exist');
 mkdirSync(dest,{recursive:true,mode:0o755});
 // 哈希核对先于解压；不执行包脚本、安装器或CLI，不访问registry。
 execFileSync('tar',['-xzf',resolve(archive),'-C',dest],{stdio:'pipe'});
 const root=join(dest,'ccloud-cli');
 const manifestRaw=readFileSync(join(root,'BUNDLE.json'));
 if(sha(manifestRaw)!==MANIFEST_SHA256)throw Error('untrusted manifest');
 const manifest=JSON.parse(manifestRaw);
 if(manifest.package!=='@cvte/ccloud-cli'||manifest.version!=='2.2.1'||manifest.node_requirement!=='>=18.15'||manifest.credential_files_included!==false||manifest.native_addons!==false)throw Error('unexpected package identity');
 const expected=new Map();
 for(const f of manifest.files){
  if(typeof f.path!=='string'||!f.path||f.path.startsWith('/')||f.path.split('/').some(p=>!p||p==='.'||p==='..')||!/^([a-f0-9]{64})$/.test(f.sha256)||expected.has(f.path))throw Error('invalid manifest entry');
  expected.set(f.path,f.sha256);
 }
 let count=0;
 function walk(dir){
  for(const name of readdirSync(dir)){
   const path=join(dir,name),st=lstatSync(path),rel=relative(root,path).split('\\').join('/');
   if(st.isDirectory()){walk(path);chmodSync(path,0o755);}
   else if(st.isFile()){
    if(rel!=='BUNDLE.json'){
     if(!expected.has(rel)||sha(readFileSync(path))!==expected.get(rel)||/\.(node|so|dylib)$/i.test(rel))throw Error('package file verification failed');
     count++;
    }
    chmodSync(path,0o444);
   }else throw Error('non-regular package entry');
  }
 }
 walk(root);
 if(count!==expected.size||!expected.has('dist/ccloud-cli.js')||!expected.has('package.json'))throw Error('incomplete package');
 const pkg=JSON.parse(readFileSync(join(root,'package.json')));
 if(pkg.name!==manifest.package||pkg.version!==manifest.version)throw Error('package metadata mismatch');
 chmodSync(root,0o755);
 return {package:manifest.package,version:manifest.version,files:count,archive_sha256:ARCHIVE_SHA256,manifest_sha256:MANIFEST_SHA256};
}
if(process.argv[1]&&resolve(process.argv[1])===new URL(import.meta.url).pathname){
 try{
  if(process.argv.length!==4)throw Error('usage');
  console.log(JSON.stringify(prepare(process.argv[2],process.argv[3])));
 }catch{console.error('CCloud offline package verification failed; no package executed');process.exitCode=1;}
}
